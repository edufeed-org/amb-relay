package main

import (
	"iter"
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

func mkFetch(events ...nostr.Event) fetchFunc {
	return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for _, e := range events {
				if !yield(e) {
					return
				}
			}
		}
	}
}

func collect(seq iter.Seq[nostr.Event]) []nostr.Event {
	var out []nostr.Event
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func TestRegistryFetchRoutingAndDedup(t *testing.T) {
	amb := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	lf := nostr.Event{ID: nostr.ID{2}, Kind: 30023}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(amb)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(lf)},
	)

	// kind-scoped: only the AMB backend.
	got := collect(reg.fetch(nostr.Filter{Kinds: []nostr.Kind{30142}}, 10))
	if len(got) != 1 || got[0].ID != amb.ID {
		t.Fatalf("30142 filter = %v, want [amb]", got)
	}
	// no kinds: both, in registration order.
	got = collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 2 || got[0].ID != amb.ID || got[1].ID != lf.ID {
		t.Fatalf("no-kind filter = %v, want [amb lf]", got)
	}
}

func TestRegistryFetchDedupAcrossBackends(t *testing.T) {
	dup := nostr.Event{ID: nostr.ID{7}, Kind: 30142}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(dup)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(dup)}, // same id leaks in
	)
	got := collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 1 {
		t.Fatalf("expected de-dup to 1 event, got %d", len(got))
	}
}

func TestRegistryFetchEarlyExit(t *testing.T) {
	a := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	b := nostr.Event{ID: nostr.ID{2}, Kind: 30142}
	reg := newRegistry(contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(a, b)})
	var seen int
	for range reg.fetch(nostr.Filter{}, 10) {
		seen++
		break // caller early-exit
	}
	if seen != 1 {
		t.Fatalf("early exit yielded %d, want 1", seen)
	}
}
