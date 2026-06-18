package main

import (
	"context"
	"iter"
	"sort"
	"testing"

	"fiatjaf.com/nostr"
)

// --- fakes ---

type fakeQueue struct{ items map[string]bool }

func newFakeQueue() *fakeQueue { return &fakeQueue{items: map[string]bool{}} }
func (q *fakeQueue) EnqueueProfileCandidate(pk string) error { q.items[pk] = true; return nil }
func (q *fakeQueue) RemoveProfileCandidate(pk string) error  { delete(q.items, pk); return nil }
func (q *fakeQueue) ListProfileQueue() ([]string, error) {
	out := make([]string, 0, len(q.items))
	for k := range q.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

type fakeSource struct {
	byPubkey map[nostr.PubKey]nostr.Event
	failAll  bool
}

func (f fakeSource) Fetch(_ context.Context, _ []string, pubkeys []nostr.PubKey) []nostr.Event {
	if f.failAll {
		return nil
	}
	var out []nostr.Event
	for _, pk := range pubkeys {
		if ev, ok := f.byPubkey[pk]; ok {
			out = append(out, ev)
		}
	}
	return out
}

type sliceQuerier struct{ events []nostr.Event }

func (s sliceQuerier) QueryEvents(_ nostr.Filter, _ int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for _, e := range s.events {
			if !yield(e) {
				return
			}
		}
	}
}

func mkKind0For(t *testing.T, sk nostr.SecretKey, name string) nostr.Event {
	t.Helper()
	ev := nostr.Event{Kind: 0, Content: `{"name":"` + name + `"}`, CreatedAt: 1700000000}
	ev.Sign(sk)
	return ev
}

// --- tests ---

func TestDrainStoresFoundLeavesMissingQueued(t *testing.T) {
	skA, skB := nostr.Generate(), nostr.Generate()
	pkA, pkB := skA.Public(), skB.Public()
	evA := mkKind0For(t, skA, "A")

	q := newFakeQueue()
	q.EnqueueProfileCandidate(pkA.Hex())
	q.EnqueueProfileCandidate(pkB.Hex())

	var stored []nostr.Event
	src := fakeSource{byPubkey: map[nostr.PubKey]nostr.Event{pkA: evA}} // B absent
	mgr := NewProfileManager(q, src, func(e nostr.Event) { stored = append(stored, e) }, func() []nostr.PubKey { return nil }, []string{"wss://x"}, 50)

	mgr.drainOnce(context.Background())

	if len(stored) != 1 || stored[0].PubKey != pkA {
		t.Fatalf("stored = %v, want [A]", stored)
	}
	left, _ := q.ListProfileQueue()
	if len(left) != 1 || left[0] != pkB.Hex() {
		t.Fatalf("queue = %v, want [B] (A drained, B retried later)", left)
	}
}

func TestDrainDropsInvalidPubkey(t *testing.T) {
	q := newFakeQueue()
	q.EnqueueProfileCandidate("not-a-valid-hex-pubkey")

	var stored []nostr.Event
	src := fakeSource{byPubkey: map[nostr.PubKey]nostr.Event{}}
	mgr := NewProfileManager(q, src, func(e nostr.Event) { stored = append(stored, e) }, func() []nostr.PubKey { return nil }, nil, 50)

	mgr.drainOnce(context.Background())

	if left, _ := q.ListProfileQueue(); len(left) != 0 {
		t.Fatalf("invalid pubkey should be dropped, queue = %v", left)
	}
	if len(stored) != 0 {
		t.Fatal("nothing should be stored")
	}
}

func TestEnqueueDelegatesToQueue(t *testing.T) {
	q := newFakeQueue()
	mgr := NewProfileManager(q, fakeSource{}, func(nostr.Event) {}, func() []nostr.PubKey { return nil }, nil, 50)
	pk := nostr.Generate().Public()
	mgr.Enqueue(pk)
	mgr.Enqueue(pk) // dedup
	if left, _ := q.ListProfileQueue(); len(left) != 1 || left[0] != pk.Hex() {
		t.Fatalf("queue = %v, want one entry", left)
	}
}

func TestBackfillAuthorsDistinct(t *testing.T) {
	skA, skB := nostr.Generate(), nostr.Generate()
	pkA, pkB := skA.Public(), skB.Public()
	e1 := nostr.Event{Kind: 30142, PubKey: pkA}
	e2 := nostr.Event{Kind: 30142, PubKey: pkB}
	e3 := nostr.Event{Kind: 30023, PubKey: pkA} // same author, different kind
	q := sliceQuerier{events: []nostr.Event{e1, e2, e3}}

	got := backfillAuthors(q, []nostr.Kind{30142, 30023}, 1000)
	if len(got) != 2 {
		t.Fatalf("distinct authors = %d, want 2 (got %v)", len(got), got)
	}
	seen := map[nostr.PubKey]bool{}
	for _, pk := range got {
		seen[pk] = true
	}
	if !seen[pkA] || !seen[pkB] {
		t.Fatalf("missing an author: %v", got)
	}
}
