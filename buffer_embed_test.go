package main

import (
	"context"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// countingEmbedder is a fake typesense30142.Embedder that records how many
// times Embed was called and how many texts each call carried, returning one
// zero-length placeholder vector per input text.
type countingEmbedder struct {
	mu    sync.Mutex
	calls [][]string // one entry per Embed() call, holding the texts it received
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string, input typesense30142.EmbedInput) ([][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]string, len(texts))
	copy(cp, texts)
	c.calls = append(c.calls, cp)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 2, 3}
	}
	return out, nil
}

func (c *countingEmbedder) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *countingEmbedder) totalTexts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		n += len(call)
	}
	return n
}

// TestPrepareBatch_BatchesEmbedCalls is the regression test for the
// 2026-07-28 write-path fix: PrepareBatch used to issue one embed HTTP call
// PER EVENT (nostrlib typesense30142/replace.go), which serialized the whole
// flush behind N blocking calls. The relay-side prepareBatch (buffer.go)
// must issue at most ceil(N/maxEmbedBatchSize) calls for a batch of N
// events, not N.
func TestPrepareBatch_BatchesEmbedCalls(t *testing.T) {
	embedder := &countingEmbedder{}
	tsDB := &typesense30142.TSBackend{
		Embedder:    embedder,
		EmbedFields: []string{"name"},
	}

	sk := nostr.Generate()
	const n = 100 // > 3*maxEmbedBatchSize(32), exercises multiple chunks
	events := make([]nostr.Event, n)
	for i := 0; i < n; i++ {
		evt := nostr.Event{
			PubKey:    nostr.GetPublicKey(sk),
			CreatedAt: nostr.Timestamp(int64(i) + 1_700_000_000),
			Kind:      nostr.Kind(30142),
			Tags:      nostr.Tags{{"d", "eb-" + string(rune('a'+i%26)) + string(rune('a'+i/26))}, {"name", "n"}},
		}
		if err := evt.Sign(sk); err != nil {
			t.Fatalf("sign: %v", err)
		}
		events[i] = evt
	}

	docs, errs := prepareBatch(tsDB, events)
	if len(errs) != 0 {
		t.Fatalf("unexpected conversion errors: %v", errs)
	}
	if len(docs) != n {
		t.Fatalf("got %d docs, want %d", len(docs), n)
	}

	wantCalls := (n + maxEmbedBatchSize - 1) / maxEmbedBatchSize
	if got := embedder.callCount(); got != wantCalls {
		t.Errorf("embed called %d times for %d events (maxEmbedBatchSize=%d), want %d — batching broken", got, n, maxEmbedBatchSize, wantCalls)
	}
	if got := embedder.totalTexts(); got != n {
		t.Errorf("embed received %d texts total, want %d", got, n)
	}

	// Every doc must have received its embedding.
	for i, doc := range docs {
		if len(doc.Embedding) == 0 {
			t.Errorf("doc %d (id=%s) has no embedding", i, doc.ID)
		}
	}
}

// TestPrepareBatch_NoEmbedderSkipsEmbedding verifies prepareBatch is a no-op
// wrt embeddings when the backend has no Embedder configured — mirrors
// nostrlib's PrepareBatch contract for non-semantic-search backends.
func TestPrepareBatch_NoEmbedderSkipsEmbedding(t *testing.T) {
	tsDB := &typesense30142.TSBackend{}

	sk := nostr.Generate()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(1_700_000_000),
		Kind:      nostr.Kind(30142),
		Tags:      nostr.Tags{{"d", "no-embed"}, {"name", "n"}},
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}

	docs, errs := prepareBatch(tsDB, []nostr.Event{evt})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if len(docs[0].Embedding) != 0 {
		t.Errorf("expected no embedding, got %v", docs[0].Embedding)
	}
}
