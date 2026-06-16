package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func makeSimpleEvent(kind nostr.Kind, tags nostr.Tags) nostr.Event {
	return nostr.Event{Kind: kind, Tags: tags}
}

func TestRegistryValidateAndStoreDispatch(t *testing.T) {
	var storedAMB, storedLF int
	reg := newRegistry(
		contentType{
			kinds:    []nostr.Kind{30142},
			validate: func(nostr.Event) (bool, string) { return false, "" },
			store:    func(nostr.Event) { storedAMB++ },
		},
		contentType{
			kinds:    []nostr.Kind{30023},
			validate: func(nostr.Event) (bool, string) { return true, "lf-rejected" },
			store:    func(nostr.Event) { storedLF++ },
		},
	)

	if reject, _ := reg.validate(makeSimpleEvent(30142, nil)); reject {
		t.Fatal("30142 should pass validation")
	}
	if reject, msg := reg.validate(makeSimpleEvent(30023, nil)); !reject || msg != "lf-rejected" {
		t.Fatalf("30023 validate = %v %q, want true lf-rejected", reject, msg)
	}
	if reject, msg := reg.validate(makeSimpleEvent(31337, nil)); !reject || msg != "kind not accepted" {
		t.Fatalf("unregistered kind = %v %q, want true 'kind not accepted'", reject, msg)
	}

	reg.store(makeSimpleEvent(30142, nil))
	reg.store(makeSimpleEvent(30023, nil))
	reg.store(makeSimpleEvent(31337, nil)) // no-op
	if storedAMB != 1 || storedLF != 1 {
		t.Fatalf("store counts = %d %d, want 1 1", storedAMB, storedLF)
	}
}

func TestRegistryKindsUnion(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}},
		contentType{kinds: []nostr.Kind{30023}},
	)
	got := reg.kinds()
	if len(got) != 2 || got[0] != 30142 || got[1] != 30023 {
		t.Fatalf("kinds() = %v, want [30142 30023]", got)
	}
}
