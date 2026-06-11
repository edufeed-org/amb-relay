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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, nil, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

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
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Errorf("got %v, want only [%s] (missing/invalid parents skipped)", idsOf(got), e1.ID.Hex())
	}
}

// optInFilter asks for parents and snippets, the kind-21142 opt-in shape.
func optInFilter(limit int) nostr.Filter {
	return nostr.Filter{
		Search: "mathematik",
		Kinds:  []nostr.Kind{30142, kindSearchSnippet},
		Limit:  limit,
	}
}

func hitFor(e nostr.Event, score float64, snippet string) ChunkHit {
	return ChunkHit{
		EventID:    e.ID.Hex(),
		EventCoord: "30142:" + e.PubKey.Hex() + ":" + e.Tags.GetD(),
		Score:      score,
		Snippet:    snippet,
	}
}

func TestRerank_InterleavesSnippetsWhenOptedIn(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-a", 1_700_000_001)
	e2 := mkEvent(t, sk, "sn-b", 1_700_000_002)
	store := &fakeStore{events: []nostr.Event{e1, e2}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		hitFor(e2, 0.9, "passage about b"),
		hitFor(e1, 0.5, "passage about a"),
	}}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(10), searcher, store.fetch, 250, relaySK))

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (parent, snippet, parent, snippet): %v", len(got), idsOf(got))
	}
	// Order: best parent, its snippet, next parent, its snippet.
	if got[0].ID != e2.ID || got[2].ID != e1.ID {
		t.Errorf("parent order wrong: %v", idsOf(got))
	}
	for i, parent := range []nostr.Event{e2, e1} {
		snip := got[i*2+1]
		if snip.Kind != kindSearchSnippet {
			t.Fatalf("event %d kind = %d, want 21142", i*2+1, snip.Kind)
		}
		if snip.PubKey != nostr.GetPublicKey(relaySK) {
			t.Errorf("snippet %d signed by %s, want relay key", i, snip.PubKey.Hex())
		}
		if got := tagValue(t, snip, "e"); got != parent.ID.Hex() {
			t.Errorf("snippet %d e tag = %q, want parent %q", i, got, parent.ID.Hex())
		}
	}
	if got[1].Content != "passage about b" || got[3].Content != "passage about a" {
		t.Errorf("snippet contents wrong: %q / %q", got[1].Content, got[3].Content)
	}
}

func TestRerank_NoSnippetsWithoutKindOptIn(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-no", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{hitFor(e1, 0.9, "a passage")}}

	filter := nostr.Filter{Search: "mathematik", Kinds: []nostr.Kind{30142}, Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, relaySK))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Fatalf("got %v, want only the parent", idsOf(got))
	}
	for _, e := range got {
		if e.Kind == kindSearchSnippet {
			t.Error("snippet emitted without kind opt-in")
		}
	}
}

func TestRerank_NoSnippetsOnFallback(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-fb", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{err: errors.New("indexer down")}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(10), searcher, store.fetch, 250, relaySK))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Fatalf("fallback must deliver plain results, got %v", idsOf(got))
	}
	if got[0].Kind == kindSearchSnippet {
		t.Error("snippet emitted on fallback path")
	}
}

func TestRerank_LimitCountsParents(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-l1", 1_700_000_001)
	e2 := mkEvent(t, sk, "sn-l2", 1_700_000_002)
	e3 := mkEvent(t, sk, "sn-l3", 1_700_000_003)
	store := &fakeStore{events: []nostr.Event{e1, e2, e3}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		hitFor(e1, 0.9, "p1"),
		hitFor(e2, 0.8, "p2"),
		hitFor(e3, 0.7, "p3"),
	}}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(2), searcher, store.fetch, 250, relaySK))

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (limit=2 parents + 2 snippets)", len(got))
	}
	if got[0].ID != e1.ID || got[2].ID != e2.ID {
		t.Errorf("wrong parents under limit: %v", idsOf(got))
	}
	if got[1].Kind != kindSearchSnippet || got[3].Kind != kindSearchSnippet {
		t.Error("expected snippet after each parent")
	}
}
