package main

import (
	"context"
	"iter"
	"strconv"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/semantic"
)

// fakeChunkSearcher records calls and returns canned hits/err.
type fakeChunkSearcher struct {
	hits     []semantic.ChunkHit
	err      error
	called   bool
	gotQ     string
	gotK     int
	gotKinds []int
}

func (f *fakeChunkSearcher) SearchChunks(ctx context.Context, q string, k int, kinds []int) ([]semantic.ChunkHit, error) {
	f.called = true
	f.gotQ = q
	f.gotK = k
	f.gotKinds = kinds
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

func tagValue(t *testing.T, e nostr.Event, key string) string {
	t.Helper()
	tag := e.Tags.Find(key)
	if tag == nil {
		t.Fatalf("missing %q tag in %v", key, e.Tags)
	}
	return tag[1]
}

// mkLongformEvent builds a signed kind-30023 (NIP-23 long-form) event with the
// d + title tags the relay requires when LONGFORM_ENABLED is on.
func mkLongformEvent(t *testing.T, sk nostr.SecretKey, d string, ts int64) nostr.Event {
	t.Helper()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(ts),
		Kind:      nostr.Kind(30023),
		Tags:      nostr.Tags{{"d", d}, {"title", "title-" + d}},
		Content:   "long-form body for " + d,
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return evt
}

// coordFor mirrors how the indexer addresses a parent event in chunk hits:
// "<kind>:<pubkey>:<d-tag>".
func coordFor(e nostr.Event) string {
	return strconv.Itoa(int(e.Kind)) + ":" + e.PubKey.Hex() + ":" + e.Tags.GetD()
}

// TestCrossContent_RerankInterleavesBothCollections is the end-to-end proof
// that a NIP-50 search over kinds:[30142,30023] merges the two Typesense
// collections via the registry, ranks the parents by chunk score, and emits a
// kind-21142 snippet carrying the correct parent k tag after each — all
// without a live Typesense.
func TestCrossContent_RerankInterleavesBothCollections(t *testing.T) {
	authorSK := nostr.Generate()
	relaySK := nostr.Generate()

	amb := mkEvent(t, authorSK, "amb-doc", 1_700_000_001) // kind 30142
	lf := mkLongformEvent(t, authorSK, "lf-doc", 1_700_000_002)

	// Each backend only knows its own collection's events and applies
	// filter.Matches, exactly like the real Typesense-backed fetch.
	ambStore := &fakeStore{events: []nostr.Event{amb}}
	lfStore := &fakeStore{events: []nostr.Event{lf}}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: ambStore.fetch},
		contentType{kinds: []nostr.Kind{30023}, fetch: lfStore.fetch},
	)
	fetch := reg.fetch

	// Long-form scores higher than AMB, so it must come first regardless of
	// which collection it lives in. Coords carry the kind-prefix the snippet
	// k tag is derived from.
	searcher := &fakeChunkSearcher{hits: []semantic.ChunkHit{
		{EventID: amb.ID.Hex(), EventCoord: coordFor(amb), Score: 0.40, Snippet: "amb passage"},
		{EventID: lf.ID.Hex(), EventCoord: coordFor(lf), Score: 0.95, Snippet: "longform passage"},
	}}

	filter := nostr.Filter{
		Search: "mathematik",
		Kinds:  []nostr.Kind{30142, 30023, semantic.KindSearchSnippet},
		Limit:  10,
	}
	got := collectEvents(semantic.ChunkRerankQuery(context.Background(), filter, searcher, fetch, 250, relaySK))

	// Expect: lf, lf-snippet, amb, amb-snippet.
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (lf, snippet, amb, snippet): %v", len(got), idsOf(got))
	}

	// Both parents present, ranked by chunk score (lf 0.95 > amb 0.40).
	if got[0].ID != lf.ID {
		t.Errorf("position 0 = %s, want long-form parent %s", got[0].ID.Hex(), lf.ID.Hex())
	}
	if got[2].ID != amb.ID {
		t.Errorf("position 2 = %s, want AMB parent %s", got[2].ID.Hex(), amb.ID.Hex())
	}
	if got[0].Kind != 30023 {
		t.Errorf("first parent kind = %d, want 30023", got[0].Kind)
	}
	if got[2].Kind != 30142 {
		t.Errorf("second parent kind = %d, want 30142", got[2].Kind)
	}

	// Each parent is followed by its kind-21142 snippet carrying the correct
	// parent k tag and e reference.
	cases := []struct {
		parent  nostr.Event
		snip    nostr.Event
		wantK   string
		wantSnp string
	}{
		{lf, got[1], "30023", "longform passage"},
		{amb, got[3], "30142", "amb passage"},
	}
	for i, c := range cases {
		if c.snip.Kind != semantic.KindSearchSnippet {
			t.Fatalf("case %d: event kind = %d, want 21142 snippet", i, c.snip.Kind)
		}
		if c.snip.PubKey != nostr.GetPublicKey(relaySK) {
			t.Errorf("case %d: snippet signed by %s, want relay key", i, c.snip.PubKey.Hex())
		}
		if v := tagValue(t, c.snip, "k"); v != c.wantK {
			t.Errorf("case %d: snippet k tag = %q, want %q", i, v, c.wantK)
		}
		if v := tagValue(t, c.snip, "e"); v != c.parent.ID.Hex() {
			t.Errorf("case %d: snippet e tag = %q, want parent %q", i, v, c.parent.ID.Hex())
		}
		if v := tagValue(t, c.snip, "a"); v != coordFor(c.parent) {
			t.Errorf("case %d: snippet a tag = %q, want %q", i, v, coordFor(c.parent))
		}
		if c.snip.Content != c.wantSnp {
			t.Errorf("case %d: snippet content = %q, want %q", i, c.snip.Content, c.wantSnp)
		}
	}
}
