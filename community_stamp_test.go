package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestShareToRefAtag(t *testing.T) {
	ev := nostr.Event{
		Kind:   16,
		PubKey: mustPK(t, "0000000000000000000000000000000000000000000000000000000000000002"),
		Tags: nostr.Tags{
			{"e", "deadbeef"},
			{"a", "30023:776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4:my-d", "wss://hint.example"},
			{"k", "30023"},
			{"h", "C1"},
			{"h", "C2"},
		},
	}
	ref, ok := shareToRef(ev)
	if !ok {
		t.Fatal("expected a parseable share ref")
	}
	if ref.Coord != "30023:776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4:my-d" {
		t.Fatalf("coord = %q", ref.Coord)
	}
	if ref.Kind != 30023 || ref.DTag != "my-d" || ref.RelayHint != "wss://hint.example" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Pubkey != "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4" {
		t.Fatalf("pubkey = %q", ref.Pubkey)
	}
	if len(ref.Communities) != 2 || ref.Communities[0] != "C1" || ref.Communities[1] != "C2" {
		t.Fatalf("communities = %v", ref.Communities)
	}
}

func TestShareToRefSkips(t *testing.T) {
	// No `a` tag → skip.
	noA := nostr.Event{Kind: 16, Tags: nostr.Tags{{"e", "x"}, {"h", "C1"}}}
	if _, ok := shareToRef(noA); ok {
		t.Fatal("share without a-tag must be skipped")
	}
	// `a` tag but no targeted community → skip.
	noComm := nostr.Event{Kind: 16, Tags: nostr.Tags{{"a", "30023:abcd:d"}}}
	if _, ok := shareToRef(noComm); ok {
		t.Fatal("share with no community target must be skipped")
	}
	// Malformed `a` value → skip.
	badA := nostr.Event{Kind: 16, Tags: nostr.Tags{{"a", "garbage"}, {"h", "C1"}}}
	if _, ok := shareToRef(badA); ok {
		t.Fatal("malformed a-coord must be skipped")
	}
}

func TestContentOwnCommunities(t *testing.T) {
	ev := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"h", "C1"}, {"h", "C2"}, {"h", "C1"}, {"t", "x"}}}
	got := contentOwnCommunities(ev)
	if len(got) != 2 || got[0] != "C1" || got[1] != "C2" {
		t.Fatalf("own communities = %v", got)
	}
}

func TestDeriveStampsMemberGated(t *testing.T) {
	refs := []shareRef{
		{Coord: "30023:p:d", Kind: 30023, Author: "alice", Communities: []string{"C1", "C2"}},
		{Coord: "30023:p:d", Kind: 30023, Author: "mallory", Communities: []string{"C3"}},
	}
	// alice is a member of C1 only; mallory of nothing.
	isMember := func(community, pubkey string, _ nostr.Kind) bool {
		return pubkey == "alice" && community == "C1"
	}
	stamps := deriveStamps(refs, isMember)
	set := stamps["30023:p:d"]
	if !set["C1"] || set["C2"] || set["C3"] {
		t.Fatalf("member-gated stamps = %v", set)
	}
}

func mustPK(t *testing.T, hex string) nostr.PubKey {
	t.Helper()
	pk, err := nostr.PubKeyFromHex(hex)
	if err != nil {
		t.Fatalf("bad pubkey hex %q: %v", hex, err)
	}
	return pk
}
