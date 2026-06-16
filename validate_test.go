// validate_test.go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestValidateAMB(t *testing.T) {
	ok := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}, {"name", "Res"}}}
	if reject, _ := validateAMB(ok); reject {
		t.Fatal("valid AMB event rejected")
	}
	noD := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"name", "Res"}}}
	if reject, msg := validateAMB(noD); !reject || msg != "missing required 'd' tag" {
		t.Fatalf("missing d = %v %q", reject, msg)
	}
	noName := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}}}
	if reject, msg := validateAMB(noName); !reject || msg != "missing required 'name' tag" {
		t.Fatalf("missing name = %v %q", reject, msg)
	}
}

func TestValidateLongform(t *testing.T) {
	ok := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"d", "x"}, {"title", "T"}}}
	if reject, _ := validateLongform(ok); reject {
		t.Fatal("valid long-form event rejected")
	}
	noD := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"title", "T"}}}
	if reject, msg := validateLongform(noD); !reject || msg != "missing required 'd' tag" {
		t.Fatalf("missing d = %v %q", reject, msg)
	}
	noTitle := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"d", "x"}}}
	if reject, msg := validateLongform(noTitle); !reject || msg != "missing required 'title' tag" {
		t.Fatalf("missing title = %v %q", reject, msg)
	}
}
