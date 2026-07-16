package main

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func pubEvent(kind nostr.Kind, tags nostr.Tags, content string) *nostr.Event {
	e := &nostr.Event{Kind: kind, Tags: tags, Content: content}
	e.PubKey = nostr.PubKey{1, 2, 3} // deterministic non-zero pubkey for doc ids
	return e
}

func TestValidatePublication(t *testing.T) {
	cases := []struct {
		name    string
		event   *nostr.Event
		reject  bool
		msgPart string
	}{
		{"valid 30040", pubEvent(30040, nostr.Tags{{"d", "a1b2c3d4"}, {"title", "Effects of X on Y"}}, ""), false, ""},
		{"valid 30041", pubEvent(30041, nostr.Tags{{"d", "sec-1"}, {"title", "Introduction"}}, "Section body text."), false, ""},
		{"30040 missing d", pubEvent(30040, nostr.Tags{{"title", "T"}}, ""), true, "'d' tag"},
		{"30040 missing title", pubEvent(30040, nostr.Tags{{"d", "x"}}, ""), true, "'title' tag"},
		{"30041 missing title", pubEvent(30041, nostr.Tags{{"d", "x"}}, "body"), true, "'title' tag"},
		{"30040 non-empty content", pubEvent(30040, nostr.Tags{{"d", "x"}, {"title", "T"}}, "not allowed"), true, "empty content"},
		{"30041 non-empty content ok", pubEvent(30041, nostr.Tags{{"d", "x"}, {"title", "T"}}, "chapter text"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reject, msg := validatePublication(*tc.event)
			if reject != tc.reject {
				t.Fatalf("reject = %v, want %v (msg %q)", reject, tc.reject, msg)
			}
			if tc.reject && !strings.Contains(msg, tc.msgPart) {
				t.Errorf("msg %q missing %q", msg, tc.msgPart)
			}
		})
	}
}
