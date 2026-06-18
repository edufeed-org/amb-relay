package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func mkKind0(t *testing.T, content string) nostr.Event {
	t.Helper()
	sk := nostr.Generate()
	ev := nostr.Event{Kind: 0, Content: content, CreatedAt: 1700000000}
	ev.Sign(sk)
	return ev
}

func TestNostrToProfileMapsFields(t *testing.T) {
	ev := mkKind0(t, `{"name":"e-teaching","display_name":"e-teaching.org","about":"Portal","nip05":"info@e-teaching.org"}`)
	doc, err := nostrToProfile(&ev)
	if err != nil {
		t.Fatalf("nostrToProfile: %v", err)
	}
	if doc.ID != ev.PubKey.Hex() {
		t.Errorf("ID = %q, want pubkey hex %q", doc.ID, ev.PubKey.Hex())
	}
	if doc.Name != "e-teaching" || doc.DisplayName != "e-teaching.org" || doc.About != "Portal" || doc.NIP05 != "info@e-teaching.org" {
		t.Errorf("fields not mapped: %+v", doc)
	}
	if doc.EventKind != 0 || doc.EventPubKey != ev.PubKey.Hex() || doc.EventRaw == "" {
		t.Errorf("envelope not filled: %+v", doc.structuredEnvelope)
	}
}

func TestNostrToProfileMissingOptionalFields(t *testing.T) {
	ev := mkKind0(t, `{"name":"solo"}`)
	doc, err := nostrToProfile(&ev)
	if err != nil {
		t.Fatalf("nostrToProfile: %v", err)
	}
	if doc.Name != "solo" || doc.DisplayName != "" || doc.About != "" || doc.NIP05 != "" {
		t.Errorf("unexpected fields: %+v", doc)
	}
}

func TestNostrToProfileRejectsNonKind0(t *testing.T) {
	sk := nostr.Generate()
	ev := nostr.Event{Kind: 1, Content: "{}", CreatedAt: 1700000000}
	ev.Sign(sk)
	if _, err := nostrToProfile(&ev); err == nil {
		t.Fatal("expected error for non-kind-0 event, got nil")
	}
}

func TestProfileSchemaHasSearchableAndEnvelopeFields(t *testing.T) {
	s := profileSchema("profiles_0")
	if s.Name != "profiles_0" || s.DefaultSortingField != "eventCreatedAt" {
		t.Fatalf("schema header wrong: %+v", s)
	}
	want := map[string]bool{"id": false, "name": false, "display_name": false, "about": false, "nip05": false, "eventRaw": false}
	for _, f := range s.Fields {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("schema missing field %q", name)
		}
	}
}
