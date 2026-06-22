package main

import (
	"testing"

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
