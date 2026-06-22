package main

import (
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestParseListCoord(t *testing.T) {
	got := parseListCoord([]string{"a", "30000:abcd:members", "wss://hint.example"})
	if got == nil || got.Pubkey != "abcd" || got.DTag != "members" || got.Relay != "wss://hint.example" {
		t.Fatalf("parseListCoord = %+v", got)
	}
	if parseListCoord([]string{"a", "30000:abcd:members"}) == nil {
		t.Fatal("missing relay hint should still parse")
	}
	if parseListCoord([]string{"a", "30168:abcd:form", "", "form"}) != nil {
		t.Fatal("non-30000 (form) must return nil")
	}
	if parseListCoord([]string{"a", "garbage"}) != nil {
		t.Fatal("malformed coord must return nil")
	}
}

func TestParseCommunitySections(t *testing.T) {
	ev := &nostr.Event{Kind: 10222, Tags: nostr.Tags{
		{"d", "communikey"},
		{"content", "Articles"},
		{"k", "30023"},
		{"a", "30000:owner1:articles", "wss://a.example"},
		{"content", "Calendar"},
		{"k", "31922"},
		{"k", "31923"},
		{"content", "Chat"},
		{"k", "9"},
		{"a", "30168:owner1:chatform", "", "form"},
	}}
	secs := parseCommunitySections(ev)
	if len(secs) != 3 {
		t.Fatalf("want 3 sections, got %d", len(secs))
	}
	if len(secs[0].Kinds) != 1 || secs[0].Kinds[0] != 30023 || secs[0].List == nil || secs[0].List.Pubkey != "owner1" {
		t.Fatalf("articles section = %+v list=%+v", secs[0].Kinds, secs[0].List)
	}
	if len(secs[1].Kinds) != 2 || secs[1].List != nil {
		t.Fatalf("calendar section = %+v list=%+v", secs[1].Kinds, secs[1].List)
	}
	if secs[2].List != nil {
		t.Fatalf("chat section must ignore form ref, got list=%+v", secs[2].List)
	}
}

func TestMembershipAllows(t *testing.T) {
	m := communityMembership{Owner: "owner1", Members: map[nostr.Kind]map[string]bool{30023: {"alice": true}}}
	if !m.allows("owner1", 30023) {
		t.Fatal("owner always a member")
	}
	if !m.allows("alice", 30023) {
		t.Fatal("listed member allowed")
	}
	if m.allows("mallory", 30023) {
		t.Fatal("unlisted on restricted kind denied")
	}
	if !m.allows("mallory", 31923) {
		t.Fatal("unrestricted kind open to anyone")
	}
}

func TestBuildMembershipOpenWins(t *testing.T) {
	sections := []communitySection{
		{Kinds: []nostr.Kind{30023}, List: &listCoord{Pubkey: "owner1", DTag: "a"}},
		{Kinds: []nostr.Kind{30023}}, // open section, same kind
		{Kinds: []nostr.Kind{31923}, List: &listCoord{Pubkey: "owner1", DTag: "cal"}},
	}
	lists := map[string]map[string]bool{"owner1:a": {"alice": true}, "owner1:cal": {"bob": true}}
	m := buildMembership("owner1", sections, lists)
	if _, restricted := m.Members[30023]; restricted {
		t.Fatal("open covering section ⇒ open wins for 30023")
	}
	if !m.Members[31923]["bob"] || m.Members[31923]["x"] {
		t.Fatalf("31923 restricted to its list, got %+v", m.Members[31923])
	}
}

func TestBuildMembershipUnreachableListOwnerOnly(t *testing.T) {
	sections := []communitySection{{Kinds: []nostr.Kind{30023}, List: &listCoord{Pubkey: "owner1", DTag: "missing"}}}
	m := buildMembership("owner1", sections, map[string]map[string]bool{}) // list fetch failed
	set, restricted := m.Members[30023]
	if !restricted || len(set) != 0 {
		t.Fatalf("unreachable list ⇒ restricted owner-only, got restricted=%v set=%+v", restricted, set)
	}
	if !m.allows("owner1", 30023) || m.allows("alice", 30023) {
		t.Fatal("owner-only restriction misbehaves")
	}
}

// --- Task 3: registry tests ---

type fakeCommunitySource struct {
	resolved map[string]communityMembership // present ⇒ (m, true)
	calls    int
}

func (f *fakeCommunitySource) Resolve(_ context.Context, _ []string, c string) (communityMembership, bool) {
	f.calls++
	m, ok := f.resolved[c]
	return m, ok
}

func TestBackfillCommunitiesDistinct(t *testing.T) {
	q := sliceQuerier{events: []nostr.Event{
		{Kind: 16, Tags: nostr.Tags{{"h", "C1"}, {"e", "x"}}},
		{Kind: 30142, Tags: nostr.Tags{{"h", "C1"}, {"h", "C2"}}},
		{Kind: 30222, Tags: nostr.Tags{{"p", "C3"}, {"d", "z"}, {"e", "y"}}},
	}}
	got := backfillCommunities(q, []nostr.Kind{16, 30142, 30222}, 1000)
	// first-seen order: C1, C2, C3
	if len(got) != 3 || got[0] != "C1" || got[1] != "C2" || got[2] != "C3" {
		t.Fatalf("backfillCommunities = %v", got)
	}
}

func TestRefreshResolvesAndOpenDefault(t *testing.T) {
	const c1 = "0000000000000000000000000000000000000000000000000000000000000002"
	const c2 = "0000000000000000000000000000000000000000000000000000000000000003"
	src := &fakeCommunitySource{resolved: map[string]communityMembership{
		c1: {Owner: c1, Members: map[nostr.Kind]map[string]bool{30023: {"alice": true}}},
	}}
	r := NewCommunityRegistry(src, func() []string { return []string{c1, c2} }, []string{"wss://r"})
	r.refresh(context.Background())

	if r.IsMember(c1, "mallory", 30023) {
		t.Fatal("C1 30023 restricted; mallory denied")
	}
	if !r.IsMember(c1, "alice", 30023) {
		t.Fatal("alice is a member")
	}
	if !r.IsMember(c2, "anyone", 30023) {
		t.Fatal("unresolved community open by default")
	}
}

func TestRefreshKeepsLastKnownOnFailure(t *testing.T) {
	const c1 = "0000000000000000000000000000000000000000000000000000000000000002"
	src := &fakeCommunitySource{resolved: map[string]communityMembership{
		c1: {Owner: c1, Members: map[nostr.Kind]map[string]bool{30023: {"alice": true}}},
	}}
	r := NewCommunityRegistry(src, func() []string { return []string{c1} }, []string{"wss://r"})
	r.refresh(context.Background()) // C1 resolved restricted
	// now C1 disappears
	src.resolved = map[string]communityMembership{}
	r.refresh(context.Background())
	if r.IsMember(c1, "mallory", 30023) {
		t.Fatal("failed re-resolve must keep last-known restriction, not flip open")
	}
}

func TestInitResolvesInBackground(t *testing.T) {
	const c1 = "0000000000000000000000000000000000000000000000000000000000000002"
	src := &fakeCommunitySource{resolved: map[string]communityMembership{c1: {Owner: c1}}}
	r := NewCommunityRegistry(src, func() []string { return []string{c1} }, []string{"wss://r"})
	if err := r.Init(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.RLock()
		_, ok := r.resolved[c1]
		r.mu.RUnlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !r.IsMember(c1, c1, 30023) {
		t.Fatal("owner of resolved community must be a member after Init")
	}
}
