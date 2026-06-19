package main

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func TestShareCommunities(t *testing.T) {
	repost := nostr.Event{
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "abc"},
			{"k", "30142"},
			{"p", "AUTHOR_pubkey_not_a_community"},
			{"h", "comm1"},
			{"h", "comm2"},
		},
	}
	got := shareCommunities(&repost)
	if len(got) != 2 || got[0] != "comm1" || got[1] != "comm2" {
		t.Fatalf("kind-16 communities = %v, want [comm1 comm2] (p must NOT count)", got)
	}

	targeted := nostr.Event{
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "x"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
			{"h", "commB"},
			{"p", "commA"}, // duplicate must be deduped
		},
	}
	got = shareCommunities(&targeted)
	if len(got) != 2 || got[0] != "commA" || got[1] != "commB" {
		t.Fatalf("kind-30222 communities = %v, want [commA commB] (p counts, deduped)", got)
	}
}

func TestNostrToShare_Repost(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xab},
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "EVENT_ID_REF"},
			{"a", "30142:auth:slug"},
			{"k", "30142"},
			{"p", "author"},
			{"h", "comm1"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	if doc.ID != evt.ID.Hex() {
		t.Errorf("kind-16 doc id = %q, want event id %q", doc.ID, evt.ID.Hex())
	}
	if len(doc.RefE) != 1 || doc.RefE[0] != "EVENT_ID_REF" {
		t.Errorf("RefE = %v", doc.RefE)
	}
	if len(doc.RefA) != 1 || doc.RefA[0] != "30142:auth:slug" {
		t.Errorf("RefA = %v", doc.RefA)
	}
	if doc.RefKind != 30142 {
		t.Errorf("RefKind = %d, want 30142", doc.RefKind)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "comm1" {
		t.Errorf("Community = %v, want [comm1]", doc.Community)
	}
	if doc.EventKind != 16 {
		t.Errorf("EventKind = %d, want 16", doc.EventKind)
	}
}

func TestNostrToShare_TargetedPublicationDocID(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xcd},
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "slug-1"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	want := nostrToShareDocID(t, evt.PubKey.Hex(), "slug-1")
	if doc.ID != want {
		t.Errorf("kind-30222 doc id = %q, want addressable id %q", doc.ID, want)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "commA" {
		t.Errorf("Community = %v, want [commA] (from p)", doc.Community)
	}
}

func TestNostrToShare_TargetedPublicationMissingD(t *testing.T) {
	evt := nostr.Event{Kind: 30222, Tags: nostr.Tags{{"a", "30142:auth:slug"}, {"p", "commA"}}}
	if _, err := nostrToShare(&evt); err == nil {
		t.Fatal("expected error for kind-30222 missing 'd' tag")
	}
}

func nostrToShareDocID(t *testing.T, pubkey, d string) string {
	t.Helper()
	return typesense30142.GenerateDocumentID(pubkey, d)
}
