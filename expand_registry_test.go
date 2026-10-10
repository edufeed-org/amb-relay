package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

type fakeMappingSource struct {
	mu     sync.Mutex
	events []nostr.Event
	err    error
	calls  int
}

func (f *fakeMappingSource) Fetch(_ context.Context, _ []string, filter nostr.Filter) ([]nostr.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []nostr.Event
	for _, ev := range f.events {
		if filter.Matches(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeMappingSource) set(events []nostr.Event, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events, f.err = events, err
}

func (f *fakeMappingSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const level2 = "https://w3id.org/kim/educationalLevel/level_2"

// levelWorld: Klassenstufen 5-6 broadMatch KIM level_2 in concordance "k",
// plus an author-only mapping by Other (no concordance).
func levelWorld() ([]nostr.Event, string) {
	o := vt.Owner
	conc := vt.ConcordanceAddr(o, "k")
	ks, el := vt.SchemeAddr(o, "klassenstufen"), vt.SchemeAddr(o, "educational-level")
	return []nostr.Event{
		vt.Concept(o, "klassenstufen", "5-6", ""),
		vt.Concept(o, "educational-level", "level_2", level2),
		vt.Concordance(o, "k", "nostr:"+ks, "nostr:"+el, []string{ks, el}),
		vt.Mapping(o, vt.ID(o, "5-6"), "broadMatch", level2, "nostr:"+ks, "nostr:"+el, conc, ks, el),
		// a stranger's mapping claiming membership: must be ignored
		vt.Mapping(vt.Other, "https://evil", "broadMatch", level2, "nostr:"+ks, "nostr:"+el, conc),
		// Other's own mapping, trusted only via expand:<npub>
		vt.Mapping(vt.Other, "https://other/x", "broadMatch", level2, "S", "T", ""),
	}, conc
}

func expandIDs(t *testing.T, r *MappingRegistry, trust Trust) []string {
	t.Helper()
	g, ok := r.Graph(context.Background(), trust)
	if !ok {
		t.Fatalf("Graph(%+v) not ok", trust)
	}
	ids, _ := g.Expand(level2, false)
	return ids
}

func TestMappingRegistryDefaults(t *testing.T) {
	events, conc := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{Concordances: []string{conc}})

	if _, ok := r.Graph(context.Background(), r.Defaults()); ok {
		t.Fatal("cold start: defaults are loaded by refresh, not on the query path")
	}
	r.refresh(context.Background())
	want := []string{level2, vt.ID(vt.Owner, "5-6")}
	if got := expandIDs(t, r, r.Defaults()); !reflect.DeepEqual(got, want) {
		t.Fatalf("expand = %v, want %v", got, want)
	}

	calls := src.callCount()
	expandIDs(t, r, r.Defaults())
	if src.callCount() != calls {
		t.Fatal("a loaded trust set must not refetch")
	}
}

func TestMappingRegistryKeepsLastGoodGraph(t *testing.T) {
	events, conc := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{Concordances: []string{conc}})
	r.refresh(context.Background())

	src.set(nil, errors.New("relay down"))
	r.refresh(context.Background())
	if got := expandIDs(t, r, r.Defaults()); len(got) != 2 {
		t.Fatalf("after failed refresh: %v", got)
	}

	src.set(nil, nil) // relay answers, but with nothing (dropped connection)
	r.refresh(context.Background())
	if got := expandIDs(t, r, r.Defaults()); len(got) != 2 {
		t.Fatalf("after empty refresh: %v", got)
	}
}

func TestMappingRegistryOnDemandAuthor(t *testing.T) {
	events, _ := levelWorld()
	src := &fakeMappingSource{events: events}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{})
	got := expandIDs(t, r, Trust{Authors: []string{vt.Other.Hex()}})
	sort.Strings(got[1:]) // narrower order follows mapping d (a hash)
	if !reflect.DeepEqual(got, []string{level2, "https://evil", "https://other/x"}) {
		t.Fatalf("author trust = %v", got)
	}
}

func TestMappingRegistryNegativeCache(t *testing.T) {
	src := &fakeMappingSource{}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{})
	missing := Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "nope")}}
	if _, ok := r.Graph(context.Background(), missing); ok {
		t.Fatal("unresolvable concordance must not yield a graph")
	}
	calls := src.callCount()
	r.Graph(context.Background(), missing)
	if src.callCount() != calls {
		t.Fatal("a failed source must not be retried on every query")
	}
}
