package main

import (
	"sort"
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

// pkOrZero parses a hex pubkey, falling back to the zero key for fixtures that
// don't care about the author identity.
func pkOrZero(hexKey string) nostr.PubKey {
	if pk, err := nostr.PubKeyFromHex(hexKey); err == nil {
		return pk
	}
	pk, _ := nostr.PubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002")
	return pk
}

type stampPatchCall struct {
	kind        nostr.Kind
	docID       string
	communities []string
}

func newTestStamper() (*CommunityStamper, *[]stampPatchCall) {
	var patches []stampPatchCall
	s := &CommunityStamper{
		isMember: func(_, _ string, _ nostr.Kind) bool { return true },
		patch: func(kind nostr.Kind, docID string, communities []string) error {
			patches = append(patches, stampPatchCall{kind, docID, communities})
			return nil
		},
		stampKinds: map[nostr.Kind]bool{30142: true, 30023: true, 30818: true, 31922: true, 31923: true},
		stopCh:     make(chan struct{}),
	}
	return s, &patches
}

func mkShare(author, coord, community string) nostr.Event {
	return nostr.Event{
		Kind:   16,
		PubKey: pkOrZero(author),
		Tags:   nostr.Tags{{"a", coord}, {"k", "30023"}, {"h", community}},
	}
}

func TestReconcileLocalStampsOwnPlusShared(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d1"
	s, patches := newTestStamper()
	s.sharesFor = func(c string) []nostr.Event {
		if c != coord {
			return nil
		}
		return []nostr.Event{mkShare(owner, coord, "C1")}
	}
	s.lookup = func(c string) (nostr.Event, bool) {
		// local content carries its own h tag C0
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d1"}, {"h", "C0"}}}, true
	}
	s.reconcile(coord, 30023, owner, "d1")

	if len(*patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(*patches))
	}
	got := (*patches)[0]
	want := []string{"C0", "C1"}
	sort.Strings(got.communities)
	if got.kind != 30023 || len(got.communities) != 2 || got.communities[0] != want[0] || got.communities[1] != want[1] {
		t.Fatalf("stampPatch = %+v", got)
	}
}

func TestReconcileNonMemberNotStamped(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d1"
	s, patches := newTestStamper()
	s.isMember = func(_, _ string, _ nostr.Kind) bool { return false }
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d1"}}}, true
	}
	s.reconcile(coord, 30023, owner, "d1")
	if len(*patches) != 1 || len((*patches)[0].communities) != 0 {
		t.Fatalf("non-member share must yield empty community set, got %+v", *patches)
	}
}

func TestReconcileAbsentNoDesiredSkips(t *testing.T) {
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return nil } // no shares → no desired
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false }
	s.reconcile("30023:p:d", 30023, "p", "d")
	if len(*patches) != 0 {
		t.Fatalf("absent content with no desired communities must not patch, got %+v", *patches)
	}
}

func TestReconcileAllDistinctCoords(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	c1 := "30023:" + owner + ":d1"
	c2 := "30023:" + owner + ":d2"
	s, patches := newTestStamper()
	s.allShares = func() []nostr.Event {
		return []nostr.Event{mkShare(owner, c1, "C1"), mkShare(owner, c1, "C1"), mkShare(owner, c2, "C2")}
	}
	s.sharesFor = func(coord string) []nostr.Event {
		var out []nostr.Event
		for _, e := range s.allShares() {
			if ref, ok := shareToRef(e); ok && ref.Coord == coord {
				out = append(out, e)
			}
		}
		return out
	}
	s.lookup = func(string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "x"}}}, true
	}
	s.reconcileAll()
	if len(*patches) != 2 {
		t.Fatalf("want 2 distinct-coord patches, got %d (%+v)", len(*patches), *patches)
	}
}

func TestReconcileFetchesAbsentThenStampsAndStores(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":d9"
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false } // absent
	var fetched, stored bool
	s.fetch = func(ref shareRef, _ []string) (nostr.Event, bool) {
		fetched = true
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "d9"}}}, true
	}
	s.validate = func(nostr.Event) (bool, string) { return false, "" }
	s.store = func(nostr.Event) { stored = true }
	s.reconcile(coord, 30023, owner, "d9")
	if !fetched || !stored || len(*patches) != 1 {
		t.Fatalf("absent path: fetched=%v stored=%v patches=%d", fetched, stored, len(*patches))
	}
}

// TestReconcileGuardSkipsUnstampableKind locks in Finding I1: reconcile must
// return immediately when the referenced kind is not in stampKinds, without
// calling fetch or patch.
func TestReconcileGuardSkipsUnstampableKind(t *testing.T) {
	s, patches := newTestStamper()
	// Override stampKinds so only 30023 is stampable; 9999 is not.
	s.stampKinds = map[nostr.Kind]bool{30023: true}
	var fetched bool
	s.fetch = func(shareRef, []string) (nostr.Event, bool) {
		fetched = true
		return nostr.Event{}, true
	}
	s.sharesFor = func(string) []nostr.Event {
		return []nostr.Event{mkShare("alice", "9999:p:d", "C1")}
	}
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false }
	s.reconcile("9999:p:d", 9999, "p", "d")
	if fetched {
		t.Error("reconcile must not fetch for an unstampable kind")
	}
	if len(*patches) != 0 {
		t.Errorf("reconcile must not patch for an unstampable kind, got %d patches", len(*patches))
	}
}

func TestReconcileFetchRejectedNotStored(t *testing.T) {
	const owner = "776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"
	coord := "30023:" + owner + ":dx"
	s, patches := newTestStamper()
	s.sharesFor = func(string) []nostr.Event { return []nostr.Event{mkShare(owner, coord, "C1")} }
	s.lookup = func(string) (nostr.Event, bool) { return nostr.Event{}, false }
	s.fetch = func(shareRef, []string) (nostr.Event, bool) {
		return nostr.Event{Kind: 30023, PubKey: pkOrZero(owner), Tags: nostr.Tags{{"d", "dx"}}}, true
	}
	s.validate = func(nostr.Event) (bool, string) { return true, "rejected" }
	var stored bool
	s.store = func(nostr.Event) { stored = true }
	s.reconcile(coord, 30023, owner, "dx")
	if stored || len(*patches) != 0 {
		t.Fatalf("rejected fetch must not store or patch: stored=%v patches=%d", stored, len(*patches))
	}
}
