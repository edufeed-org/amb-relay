package main

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

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

// --- Fix round 1, finding 1: Graph's on-demand loop must share one
// timeout across the whole call (not one per source) and must cap how many
// unknown sources it attempts per call. ---

// blockingMappingSource.Fetch blocks until ctx is done, on every call, so
// a Graph call driving N sources sequentially against a per-source timeout
// would take N times as long as one against a single, shared timeout.
type blockingMappingSource struct {
	mu    sync.Mutex
	calls int
}

func (b *blockingMappingSource) Fetch(ctx context.Context, _ []string, _ nostr.Filter) ([]nostr.Event, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingMappingSource) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestMappingRegistryGraphSharedTimeoutCapsOnDemandLoads(t *testing.T) {
	src := &blockingMappingSource{}
	r := NewMappingRegistry(src, []string{"wss://r"}, Trust{})
	r.onDemandTimeout = 50 * time.Millisecond // registry field: tests shrink it instead of waiting out mappingOnDemandTimeout

	var authors []string
	for i := byte(1); i <= 6; i++ { // 6 unknown sources, more than maxOnDemandLoadsPerQuery
		authors = append(authors, nostr.GetPublicKey([32]byte{31: i}).Hex())
	}
	trust := Trust{Authors: authors}

	start := time.Now()
	if _, ok := r.Graph(context.Background(), trust); ok {
		t.Fatal("blocked, never-resolving sources must not yield a graph")
	}
	elapsed := time.Since(start)

	// A per-source timeout would serialize to roughly 6 * onDemandTimeout
	// (300ms); a shared one keeps the whole call to roughly one
	// onDemandTimeout, since once it expires every further load call sees
	// an already-done context and returns immediately.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Graph blocked for %v, want ~one onDemandTimeout shared across the whole call", elapsed)
	}
	if calls := src.callCount(); calls > maxOnDemandLoadsPerQuery {
		t.Fatalf("attempted %d on-demand loads for 6 unknown sources, want at most %d", calls, maxOnDemandLoadsPerQuery)
	}
}

// --- Fix round 1, finding 4: pageRelay's "did this page move the cursor
// forward" guard must be scoped per relay, not against the shared,
// cross-relay byID dedupe set. ---

func fakeEventID(i int) nostr.ID {
	var id nostr.ID
	binary.BigEndian.PutUint32(id[:4], uint32(i))
	return id
}

// mirroredStoreFetcher returns a pageFetcher reading from store (assumed
// sorted by CreatedAt descending, as a relay would deliver it), applying
// Until/Limit like a real relay: events with CreatedAt <= Until, newest
// first, at most Limit of them. Every relay url shares the same store, so
// two "relays" built from it are exact mirrors of each other.
func mirroredStoreFetcher(store []nostr.Event) pageFetcher {
	return func(_ context.Context, _ string, f nostr.Filter) ([]nostr.Event, error) {
		var out []nostr.Event
		for _, ev := range store {
			if f.Until != 0 && ev.CreatedAt > f.Until {
				continue
			}
			out = append(out, ev)
			if len(out) >= f.Limit {
				break
			}
		}
		return out, nil
	}
}

func TestPageRelayMirroredRelaysDoNotFalselyFail(t *testing.T) {
	const total = 650 // > 2*mappingPageSize, so paging spans 3 pages per relay
	store := make([]nostr.Event, total)
	for i := 0; i < total; i++ {
		store[i] = nostr.Event{ID: fakeEventID(i), CreatedAt: nostr.Timestamp(1_000_000 - i)}
	}
	fetch := mirroredStoreFetcher(store)

	byID := map[nostr.ID]nostr.Event{}
	for _, url := range []string{"wss://mirror-a", "wss://mirror-b"} {
		if err := pageRelay(context.Background(), url, nostr.Filter{}, byID, fetch); err != nil {
			t.Fatalf("pageRelay(%s) = %v, want no error (a mirrored relay's already-seen first page must not look stalled)", url, err)
		}
	}
	if len(byID) != total {
		t.Fatalf("byID has %d events after two mirrored relays, want %d (full dedupe, no loss)", len(byID), total)
	}
	for i := 0; i < total; i++ {
		if _, ok := byID[fakeEventID(i)]; !ok {
			t.Fatalf("byID missing event %d", i)
		}
	}
}
