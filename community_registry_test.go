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
