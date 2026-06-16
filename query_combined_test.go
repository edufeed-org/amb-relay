package main

import (
	"iter"
	"slices"
	"testing"

	"fiatjaf.com/nostr"
)

func mkEvt(kind nostr.Kind, d string) nostr.Event {
	e := nostr.Event{Kind: kind, Tags: nostr.Tags{{"d", d}}}
	e.ID = e.GetID()
	return e
}

func TestCombinedFetchRoutesByKind(t *testing.T) {
	amb := mkEvt(30142, "a1")
	lf := mkEvt(30023, "l1")

	ambFetch := func(f nostr.Filter, max int) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) { yield(amb) }
	}
	lfFetch := func(f nostr.Filter, max int) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) { yield(lf) }
	}
	combined := combinedFetch(ambFetch, lfFetch)

	// kind 30142 only → AMB backend only
	var got []nostr.Kind
	for e := range combined(nostr.Filter{Kinds: []nostr.Kind{30142}}, 10) {
		got = append(got, e.Kind)
	}
	if !slices.Equal(got, []nostr.Kind{30142}) {
		t.Errorf("30142-only routed to %v", got)
	}

	// both kinds → both backends
	got = nil
	for e := range combined(nostr.Filter{Kinds: []nostr.Kind{30142, 30023}}, 10) {
		got = append(got, e.Kind)
	}
	if len(got) != 2 {
		t.Errorf("both-kinds returned %v", got)
	}

	// no kinds (e.g. ID-only fetch from chunk-rerank) → both backends
	got = nil
	for e := range combined(nostr.Filter{}, 10) {
		got = append(got, e.Kind)
	}
	if len(got) != 2 {
		t.Errorf("no-kind fetch returned %v want both", got)
	}
}
