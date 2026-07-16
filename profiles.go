package main

import (
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/sdk"
)

// ProfileDocument is the Typesense document shape for a kind-0 profile stored
// in the profiles collection. One document per author pubkey (id = pubkey hex),
// so a newer kind-0 upserts over the older one. The structured envelope carries
// the full kind-0 event (eventRaw) so the query path can return the real signed
// event; profilesDB has no RawEventStore, so reconstruction is from eventRaw.
type ProfileDocument struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	About       string `json:"about,omitempty"`
	NIP05       string `json:"nip05,omitempty"`
	// NIP05Verified is relay-computed at fetch time (ProfileManager resolves
	// the claimed identifier's .well-known/nostr.json): it never comes from
	// the event itself, so nostrToProfile leaves it false. No omitempty —
	// an explicit false must reach Typesense so refreshes can revoke it.
	NIP05Verified bool `json:"nip05_verified"`
	structuredEnvelope
}

// nostrToProfile projects a kind-0 event into a ProfileDocument using the pure
// sdk.ParseMetadata parser (errors on non-kind-0 or malformed JSON content).
func nostrToProfile(event *nostr.Event) (*ProfileDocument, error) {
	meta, err := sdk.ParseMetadata(*event)
	if err != nil {
		return nil, fmt.Errorf("parse kind-0 metadata: %w", err)
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	return &ProfileDocument{
		ID:                 event.PubKey.Hex(),
		Name:               meta.Name,
		DisplayName:        meta.DisplayName,
		About:              meta.About,
		NIP05:              meta.NIP05,
		structuredEnvelope: env,
	}, nil
}

// profileProjector wraps the pure nostrToProfile projection with the
// caller-computed verification result; verification is a network side effect
// owned by ProfileManager, never by the projection.
func profileProjector(nip05Verified bool) func(*nostr.Event) (*ProfileDocument, error) {
	return func(e *nostr.Event) (*ProfileDocument, error) {
		doc, err := nostrToProfile(e)
		if err != nil {
			return nil, err
		}
		doc.NIP05Verified = nip05Verified
		return doc, nil
	}
}

// storeProfile projects and upserts a kind-0 event to the profiles collection.
func storeProfile(enabled bool, ts *typesense30142.TSBackend, event nostr.Event, nip05Verified bool) {
	storeStructured(enabled, ts, event, "profile", profileProjector(nip05Verified))
}

// profileSchema returns the Typesense collection schema for kind-0 profiles.
// Mirrors wikiSchema's envelope naming so the shared query path reconstructs
// events identically.
func profileSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "name", Type: "string", Optional: true},
			{Name: "display_name", Type: "string", Optional: true},
			{Name: "about", Type: "string", Optional: true},
			{Name: "nip05", Type: "string", Optional: true},
			{Name: "nip05_verified", Type: "bool", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}
