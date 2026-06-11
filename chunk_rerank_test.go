package main

import (
	"context"
	"errors"
	"iter"
	"testing"

	"fiatjaf.com/nostr"
)

// fakeChunkSearcher records calls and returns canned hits/err.
type fakeChunkSearcher struct {
	hits   []ChunkHit
	err    error
	called bool
	gotQ   string
	gotK   int
}

func (f *fakeChunkSearcher) SearchChunks(ctx context.Context, q string, k int) ([]ChunkHit, error) {
	f.called = true
	f.gotQ = q
	f.gotK = k
	return f.hits, f.err
}

// fakeStore implements the fetch function backed by an in-memory event slice.
// It applies filter.Matches so tests exercise the same post-filter semantics
// the real Typesense-backed fetch provides.
type fakeStore struct {
	events []nostr.Event
	calls  []nostr.Filter
}

func (f *fakeStore) fetch(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	f.calls = append(f.calls, filter)
	return func(yield func(nostr.Event) bool) {
		n := 0
		for _, e := range f.events {
			if !filter.Matches(e) {
				continue
			}
			if !yield(e) {
				return
			}
			n++
			if maxLimit > 0 && n >= maxLimit {
				return
			}
		}
	}
}

func collectEvents(seq iter.Seq[nostr.Event]) []nostr.Event {
	var out []nostr.Event
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func idsOf(events []nostr.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID.Hex())
	}
	return out
}

func TestChunkRerank_NoSearchField_BypassesChunks(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-a", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{err: errors.New("must not be called")}

	filter := nostr.Filter{Kinds: []nostr.Kind{30142}, Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if searcher.called {
		t.Error("chunk searcher was called for a filter without a search field")
	}
	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("got %v, want [%s]", idsOf(got), e1.ID.Hex())
	}
	if len(store.calls) != 1 || store.calls[0].Search != "" {
		t.Errorf("fetch not called with original filter: %+v", store.calls)
	}
}

func TestChunkRerank_NilSearcher_BypassesChunks(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-nil", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, nil, store.fetch, 250))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("got %v, want [%s]", idsOf(got), e1.ID.Hex())
	}
	if len(store.calls) != 1 || store.calls[0].Search != "mathematik" {
		t.Errorf("fetch must receive the original search filter, got %+v", store.calls)
	}
}

func TestChunkRerank_SearcherError_FallsBack(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-err", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{err: errors.New("indexer down")}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if !searcher.called {
		t.Fatal("searcher was not called")
	}
	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("fallback did not return plain search results: %v", idsOf(got))
	}
	if len(store.calls) != 1 || store.calls[0].Search != "mathematik" {
		t.Errorf("fallback must use the original filter, got %+v", store.calls)
	}
}

func TestChunkRerank_EmptyChunkResult_FallsBack(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-empty", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{hits: nil}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("empty chunk result must fall back to plain search, got %v", idsOf(got))
	}
}

func TestChunkRerank_OrdersByChunkScore(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-s1", 1_700_000_001)
	e2 := mkEvent(t, sk, "cr-s2", 1_700_000_002)
	e3 := mkEvent(t, sk, "cr-s3", 1_700_000_003)
	store := &fakeStore{events: []nostr.Event{e1, e2, e3}}
	// Deliberately unsorted: e2 best, e3 middle, e1 worst.
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		{EventID: e1.ID.Hex(), Score: 0.21},
		{EventID: e2.ID.Hex(), Score: 0.93},
		{EventID: e3.ID.Hex(), Score: 0.55},
	}}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	want := []string{e2.ID.Hex(), e3.ID.Hex(), e1.ID.Hex()}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %v", len(got), idsOf(got))
	}
	for i, w := range want {
		if got[i].ID.Hex() != w {
			t.Errorf("position %d: got %s, want %s", i, got[i].ID.Hex(), w)
		}
	}
}

func TestChunkRerank_DedupesParents(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-d1", 1_700_000_001)
	e2 := mkEvent(t, sk, "cr-d2", 1_700_000_002)
	store := &fakeStore{events: []nostr.Event{e1, e2}}
	// e1 has three chunk hits; its best (0.9) beats e2's (0.8).
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		{EventID: e1.ID.Hex(), Score: 0.4},
		{EventID: e2.ID.Hex(), Score: 0.8},
		{EventID: e1.ID.Hex(), Score: 0.9},
		{EventID: e1.ID.Hex(), Score: 0.1},
	}}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (deduped): %v", len(got), idsOf(got))
	}
	if got[0].ID != e1.ID || got[1].ID != e2.ID {
		t.Errorf("got order %v, want [%s %s] (ranked by best chunk score)", idsOf(got), e1.ID.Hex(), e2.ID.Hex())
	}
}

func TestChunkRerank_RespectsLimit(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-l1", 1_700_000_001)
	e2 := mkEvent(t, sk, "cr-l2", 1_700_000_002)
	e3 := mkEvent(t, sk, "cr-l3", 1_700_000_003)
	store := &fakeStore{events: []nostr.Event{e1, e2, e3}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		{EventID: e1.ID.Hex(), Score: 0.9},
		{EventID: e2.ID.Hex(), Score: 0.8},
		{EventID: e3.ID.Hex(), Score: 0.7},
	}}

	filter := nostr.Filter{Search: "mathematik", Limit: 2}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (filter.Limit)", len(got))
	}
	if got[0].ID != e1.ID || got[1].ID != e2.ID {
		t.Errorf("got %v, want top-2 by score", idsOf(got))
	}
}

func TestChunkRerank_AppliesRemainingFilters(t *testing.T) {
	sk1 := nostr.Generate()
	sk2 := nostr.Generate()
	e1 := mkEvent(t, sk1, "cr-f1", 1_700_000_001)
	e2 := mkEvent(t, sk2, "cr-f2", 1_700_000_002)
	store := &fakeStore{events: []nostr.Event{e1, e2}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		{EventID: e2.ID.Hex(), Score: 0.9},
		{EventID: e1.ID.Hex(), Score: 0.8},
	}}

	// Author filter excludes e2 even though its chunk score is higher.
	filter := nostr.Filter{Search: "mathematik", Authors: []nostr.PubKey{e1.PubKey}, Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("got %v, want only [%s] (author filter must apply)", idsOf(got), e1.ID.Hex())
	}
}

func TestChunkRerank_SkipsMissingParents(t *testing.T) {
	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cr-m1", 1_700_000_001)
	missing := mkEvent(t, sk, "cr-m2", 1_700_000_002) // valid id, NOT in store
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		{EventID: missing.ID.Hex(), Score: 0.9},
		{EventID: "not-a-hex-id", Score: 0.85},
		{EventID: e1.ID.Hex(), Score: 0.8},
	}}

	filter := nostr.Filter{Search: "mathematik", Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("got %v, want only [%s] (missing/invalid parents skipped)", idsOf(got), e1.ID.Hex())
	}
}
