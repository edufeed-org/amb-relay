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

func TestValidateWiki(t *testing.T) {
	ok := nostr.Event{Kind: 30818, Tags: nostr.Tags{{"d", "x"}}}
	if reject, _ := validateWiki(ok); reject {
		t.Error("valid wiki event (d only) rejected")
	}
	noD := nostr.Event{Kind: 30818, Tags: nostr.Tags{{"title", "x"}}}
	if reject, _ := validateWiki(noD); !reject {
		t.Error("wiki event missing d tag accepted")
	}
}

func TestValidateTransferkiosk(t *testing.T) {
	ok := nostr.Event{Kind: 30143, Tags: nostr.Tags{{"d", "x"}, {"name", "y"}}}
	if reject, _ := validateTransferkiosk(ok); reject {
		t.Error("valid event rejected")
	}
	noD := nostr.Event{Kind: 30144, Tags: nostr.Tags{{"name", "y"}}}
	if reject, _ := validateTransferkiosk(noD); !reject {
		t.Error("missing d not rejected")
	}
	noName := nostr.Event{Kind: 30145, Tags: nostr.Tags{{"d", "x"}}}
	if reject, _ := validateTransferkiosk(noName); !reject {
		t.Error("missing name not rejected")
	}
}
