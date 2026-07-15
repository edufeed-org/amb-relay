package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestValidateDeletion(t *testing.T) {
	served := map[nostr.Kind]bool{30142: true, 30023: true}
	validate := validateDeletion(served)

	cases := []struct {
		name   string
		tags   nostr.Tags
		reject bool
		msg    string
	}{
		{"e tag only", nostr.Tags{{"e", "abc"}}, false, ""},
		{"a tag served kind", nostr.Tags{{"a", "30142:pubkey:d-tag"}}, false, ""},
		{"a tag foreign kind", nostr.Tags{{"a", "1:pubkey:d-tag"}}, true, "deletion references no kind served by this relay"},
		{"a foreign but e present", nostr.Tags{{"a", "1:pubkey:d"}, {"e", "abc"}}, false, ""},
		{"a served among foreign", nostr.Tags{{"a", "1:p:d"}, {"a", "30023:p:d"}}, false, ""},
		{"k tag only, no target", nostr.Tags{{"k", "30142"}}, true, "missing 'e' or 'a' tag referencing the event to delete"},
		{"no tags", nostr.Tags{}, true, "missing 'e' or 'a' tag referencing the event to delete"},
		{"malformed a coord", nostr.Tags{{"a", "notacoord"}}, true, "deletion references no kind served by this relay"},
		{"empty tag value skipped", nostr.Tags{{"e", ""}}, true, "missing 'e' or 'a' tag referencing the event to delete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := nostr.Event{Kind: nostr.KindDeletion, Tags: c.tags}
			reject, msg := validate(ev)
			if reject != c.reject || msg != c.msg {
				t.Errorf("validate = (%v, %q), want (%v, %q)", reject, msg, c.reject, c.msg)
			}
		})
	}
}
