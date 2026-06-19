package main

import (
	"fmt"
	"strconv"

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
	ID      string   `json:"id"`
	RefE    []string `json:"refE,omitempty"`
	RefA    []string `json:"refA,omitempty"`
	RefKind int      `json:"refKind,omitempty"`
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

	doc := &ShareDocument{ID: id, structuredEnvelope: env}
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
			if n, err := strconv.Atoi(tag[1]); err == nil {
				doc.RefKind = n
			}
		}
	}
	return doc, nil
}
