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

func TestValidateShare(t *testing.T) {
	cases := []struct {
		name   string
		event  nostr.Event
		reject bool
	}{
		{"repost with h + e", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"e", "id"}}}, false},
		{"repost with h + a", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"a", "30142:x:y"}}}, false},
		{"repost no community", nostr.Event{Kind: 16, Tags: nostr.Tags{{"e", "id"}, {"p", "author"}}}, true},
		{"repost no reference", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}}}, true},
		{"targeted via p with a + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"p", "c"}, {"a", "30142:x:y"}}}, false},
		{"targeted via h with e + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"h", "c"}, {"e", "id"}}}, false},
		{"targeted missing d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"p", "c"}, {"a", "30142:x:y"}}}, true},
		{"targeted no community", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"a", "30142:x:y"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reject, msg := validateShare(c.event)
			if reject != c.reject {
				t.Errorf("validateShare = %v (%q), want reject=%v", reject, msg, c.reject)
			}
		})
	}
}
