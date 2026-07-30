package main

import (
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Community-share event kinds. kindGenericRepost is the NIP-18 generic repost —
// the current edufeed-app community-share mechanism (one `h` per target
// community + e/a/k/p references). kindTargetedPublication is the legacy
// Communikey "targeted publication": addressable, targets a community via `p`
// (majority) or `h`. Kind 6 (kind-1 reposts) and 1985 (labels) are NOT used.
const (
	kindGenericRepost       nostr.Kind = 16
	kindTargetedPublication nostr.Kind = 30222
)

// ShareDocument is the Typesense document for a community share event. It records
// which communities the share targets (Community, folded kind-aware from h/p) and
// which content it references (RefE/RefA/RefKind), so a
// {"kinds":[16,30222],"#h":["X"]} REQ lists what was shared into community X. It
// embeds structuredEnvelope for raw-event reconstruction; Community is set
// explicitly rather than via the envelope's h-only fold, because kind 30222 also
// targets via `p`.
type ShareDocument struct {
	ID string `json:"id"`
	// D is the addressable identifier of a kind-30222 targeted publication,
	// empty for kind-16 reposts (which are not addressable). Without it the
	// collection has no queryable `d` identity, which also breaks a-tag
	// deletion enforcement: handleDeleteRequest looks its target up by the
	// parsed d identifier through this same collection, so the lookup found
	// nothing at every d length and khatru reported the deletion as OK while
	// the share stayed live. See nostrlib#6.
	D    string   `json:"d,omitempty"`
	RefE []string `json:"refE,omitempty"`
	RefA []string `json:"refA,omitempty"`
	// RefKind is the referenced kind as a string rather than a number, so the
	// shared query path can filter it with the same backtick-quoted exact-match
	// syntax it uses for every other tag. `k` tag values are strings on the
	// wire anyway.
	RefKind string `json:"refKind,omitempty"`
	structuredEnvelope
}

// shareCommunities folds the target community pubkeys from a share event,
// kind-aware: `h` for both kinds, plus `p` for kind 30222 (legacy targeted
// publications target via `p`). On kind 16 a `p` tag is the original author of
// the reposted content, never a community, so it is ignored. Order-preserving
// and deduped.
func shareCommunities(event *nostr.Event) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch {
		case tag[0] == "h":
			add(tag[1])
		case tag[0] == "p" && event.Kind == kindTargetedPublication:
			add(tag[1])
		}
	}
	return out
}

// nostrToShare projects a kind-16 or kind-30222 event into a ShareDocument.
func nostrToShare(event *nostr.Event) (*ShareDocument, error) {
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	env.Community = shareCommunities(event)

	var id string
	if event.Kind == kindTargetedPublication {
		dTag := event.Tags.GetD()
		if dTag == "" {
			return nil, fmt.Errorf("targeted publication %s missing required 'd' tag", event.ID.Hex())
		}
		id = typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag)
	} else {
		id = event.ID.Hex()
	}

	doc := &ShareDocument{ID: id, D: event.Tags.GetD(), structuredEnvelope: env}
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "e":
			doc.RefE = append(doc.RefE, tag[1])
		case "a":
			doc.RefA = append(doc.RefA, tag[1])
		case "k":
			doc.RefKind = tag[1]
		}
	}
	return doc, nil
}

// validateShare accepts a community share event. Both kinds require at least one
// target community (kind-aware: h, or p for 30222) AND at least one content
// reference (e or a). Kind 30222 is addressable, so it additionally requires a
// `d` tag. A kind-16 repost without an `h` tag is a plain repost, not a community
// share, and is rejected.
func validateShare(event nostr.Event) (reject bool, msg string) {
	if len(shareCommunities(&event)) == 0 {
		return true, "share event missing community target (h tag, or p tag for kind 30222)"
	}
	hasRef, hasD := false, false
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "e", "a":
			hasRef = true
		case "d":
			hasD = true
		}
	}
	if !hasRef {
		return true, "share event missing content reference (e or a tag)"
	}
	if event.Kind == kindTargetedPublication && !hasD {
		return true, "targeted publication missing required 'd' tag"
	}
	return false, ""
}

// sharesSchema returns the Typesense collection schema for community share
// events. Reuses the envelope fields (eventID/eventKind/…/community) so the
// shared query path reconstructs events and #h / community:<pubkey> queries hit
// the same `community` field as every other collection.
func sharesSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string", Optional: true, Facet: true},
			{Name: "refE", Type: "string[]", Optional: true, Facet: true},
			{Name: "refA", Type: "string[]", Optional: true, Facet: true},
			{Name: "refKind", Type: "string", Optional: true, Facet: true},
		}, structuredEnvelopeFields()...),
	}
}

// sharesTagFields is the nostr-tag -> Typesense-field mapping for the
// community_shares collection. The shared query path maps tags onto the AMB
// collection's field names, which this collection does not use: #a resolved to
// nostr_a and #e/#k had no mapping at all, so "which communities carry this
// resource" returned 0 for every input while the data sat indexed and faceted.
// #d needs no entry — the query path emits `d:=` directly, and the schema now
// declares that field. See nostrlib#6.
func sharesTagFields() map[string]string {
	return map[string]string{
		"a": "refA",
		"e": "refE",
		"k": "refKind",
	}
}

// storeShare projects and upserts a share event to the community_shares Typesense
// collection via the shared structured-collection helper (fire-and-forget; the
// event is already durable in BoltDB).
func storeShare(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "share", nostrToShare)
}
