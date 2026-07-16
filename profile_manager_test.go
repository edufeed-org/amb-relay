package main

import (
	"context"
	"iter"
	"sort"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
)

// --- fakes ---

type fakeQueue struct{ items map[string]bool }

func newFakeQueue() *fakeQueue                               { return &fakeQueue{items: map[string]bool{}} }
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

// relayAwareSource returns a pubkey's event only when one of the queried relays
// is listed for it, so a test can model a profile that lives only on a fallback
// relay.
type relayAwareSource struct {
	byRelay map[string]map[nostr.PubKey]nostr.Event
}

func (f relayAwareSource) Fetch(_ context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event {
	var out []nostr.Event
	for _, r := range relays {
		for _, pk := range pubkeys {
			if ev, ok := f.byRelay[r][pk]; ok {
				out = append(out, ev)
			}
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

// mkKind0JSON signs a kind-0 with explicit content JSON, for profiles that
// need more than a name (nip05 claims).
func mkKind0JSON(t *testing.T, sk nostr.SecretKey, content string) nostr.Event {
	t.Helper()
	ev := nostr.Event{Kind: 0, Content: content, CreatedAt: 1700000000}
	ev.Sign(sk)
	return ev
}

// noVerify is a stub verifier for tests that don't exercise NIP-05 verification.
func noVerify(context.Context, string, nostr.PubKey) bool { return false }

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
	store := func(e nostr.Event, _ bool) { stored = append(stored, e) }
	mgr := NewProfileManager(q, src, store, func() []nostr.PubKey { return nil }, noVerify, []string{"wss://x"}, nil, 50)

	mgr.drainOnce(context.Background())

	if len(stored) != 1 || stored[0].PubKey != pkA {
		t.Fatalf("stored = %v, want [A]", stored)
	}
	left, _ := q.ListProfileQueue()
	if len(left) != 1 || left[0] != pkB.Hex() {
		t.Fatalf("queue = %v, want [B] (A drained, B retried later)", left)
	}
}

func TestDrainFallbackResolvesMissing(t *testing.T) {
	skA, skB := nostr.Generate(), nostr.Generate()
	pkA, pkB := skA.Public(), skB.Public()
	evA := mkKind0For(t, skA, "A") // on the primary relay
	evB := mkKind0For(t, skB, "B") // only on the fallback relay

	q := newFakeQueue()
	q.EnqueueProfileCandidate(pkA.Hex())
	q.EnqueueProfileCandidate(pkB.Hex())

	src := relayAwareSource{byRelay: map[string]map[nostr.PubKey]nostr.Event{
		"wss://primary":  {pkA: evA},
		"wss://fallback": {pkB: evB},
	}}
	var stored []nostr.Event
	store := func(e nostr.Event, _ bool) { stored = append(stored, e) }
	mgr := NewProfileManager(q, src, store, func() []nostr.PubKey { return nil }, noVerify, []string{"wss://primary"}, []string{"wss://fallback"}, 50)

	mgr.drainOnce(context.Background())

	if len(stored) != 2 {
		t.Fatalf("stored = %d events, want 2 (A from primary, B from fallback)", len(stored))
	}
	got := map[nostr.PubKey]bool{}
	for _, e := range stored {
		got[e.PubKey] = true
	}
	if !got[pkA] || !got[pkB] {
		t.Fatalf("stored pubkeys = %v, want both A and B", got)
	}
	if left, _ := q.ListProfileQueue(); len(left) != 0 {
		t.Fatalf("queue = %v, want empty (both resolved)", left)
	}
}

func TestDrainDropsInvalidPubkey(t *testing.T) {
	q := newFakeQueue()
	q.EnqueueProfileCandidate("not-a-valid-hex-pubkey")

	var stored []nostr.Event
	src := fakeSource{byPubkey: map[nostr.PubKey]nostr.Event{}}
	store := func(e nostr.Event, _ bool) { stored = append(stored, e) }
	mgr := NewProfileManager(q, src, store, func() []nostr.PubKey { return nil }, noVerify, nil, nil, 50)

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
	mgr := NewProfileManager(q, fakeSource{}, func(nostr.Event, bool) {}, func() []nostr.PubKey { return nil }, noVerify, nil, nil, 50)
	pk := nostr.Generate().Public()
	mgr.Enqueue(pk)
	mgr.Enqueue(pk) // dedup
	if left, _ := q.ListProfileQueue(); len(left) != 1 || left[0] != pk.Hex() {
		t.Fatalf("queue = %v, want one entry", left)
	}
}

func TestBackfillProfileCandidatesUnionsCommunities(t *testing.T) {
	skAuthor, skCommunity := nostr.Generate(), nostr.Generate()
	authorPk, communityPk := skAuthor.Public(), skCommunity.Public()

	// A content event by authorPk, and a kind-16 share targeting communityPk via its h tag.
	content := nostr.Event{Kind: 30142, PubKey: authorPk, CreatedAt: 1700000000}
	share := nostr.Event{
		Kind:      16,
		PubKey:    nostr.Generate().Public(), // sharer, not a community
		CreatedAt: 1700000001,
		Tags:      nostr.Tags{{"h", communityPk.Hex()}, {"e", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}},
	}
	q := sliceQuerier{events: []nostr.Event{content, share}}

	got := backfillProfileCandidates(q, []nostr.Kind{30142}, []nostr.Kind{16}, 1000)

	has := func(target nostr.PubKey) bool {
		for _, pk := range got {
			if pk == target {
				return true
			}
		}
		return false
	}
	if !has(authorPk) {
		t.Errorf("union missing content author %s", authorPk.Hex())
	}
	if !has(communityPk) {
		t.Errorf("union missing discovered community %s", communityPk.Hex())
	}
	// No duplicates.
	seen := map[nostr.PubKey]bool{}
	for _, pk := range got {
		if seen[pk] {
			t.Errorf("duplicate pubkey %s in union", pk.Hex())
		}
		seen[pk] = true
	}
}

func TestEnqueueShareCommunities(t *testing.T) {
	communityPk := nostr.Generate().Public()
	share := nostr.Event{
		Kind:      16,
		PubKey:    nostr.Generate().Public(),
		CreatedAt: 1700000000,
		Tags:      nostr.Tags{{"h", communityPk.Hex()}, {"e", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}},
	}

	q := newFakeQueue()
	mgr := NewProfileManager(q, fakeSource{}, func(nostr.Event, bool) {}, func() []nostr.PubKey { return nil }, noVerify, []string{"wss://x"}, nil, 50)

	enqueueShareCommunities(mgr, share)

	queued, _ := q.ListProfileQueue()
	if len(queued) != 1 || queued[0] != communityPk.Hex() {
		t.Fatalf("queue = %v, want [%s]", queued, communityPk.Hex())
	}

	// nil ProfileManager must be a safe no-op (profiles disabled).
	enqueueShareCommunities(nil, share)
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

func TestFetchStoreVerifiesNIP05(t *testing.T) {
	skGood, skBad, skNone := nostr.Generate(), nostr.Generate(), nostr.Generate()
	pkGood, pkBad, pkNone := skGood.Public(), skBad.Public(), skNone.Public()
	src := fakeSource{byPubkey: map[nostr.PubKey]nostr.Event{
		pkGood: mkKind0JSON(t, skGood, `{"name":"good","nip05":"good@uni.example"}`),
		pkBad:  mkKind0JSON(t, skBad, `{"name":"bad","nip05":"fake@uni.example"}`),
		pkNone: mkKind0JSON(t, skNone, `{"name":"none"}`),
	}}
	q := newFakeQueue()
	for pk := range src.byPubkey {
		q.EnqueueProfileCandidate(pk.Hex())
	}
	var mu sync.Mutex
	verifiedBy := map[nostr.PubKey]bool{}
	verifierCalls := map[string]int{}
	store := func(e nostr.Event, v bool) {
		mu.Lock()
		defer mu.Unlock()
		verifiedBy[e.PubKey] = v
	}
	verify := func(_ context.Context, id string, pk nostr.PubKey) bool {
		mu.Lock()
		verifierCalls[id]++
		mu.Unlock()
		return id == "good@uni.example" && pk == pkGood
	}
	pm := NewProfileManager(q, src, store, func() []nostr.PubKey { return nil }, verify, []string{"wss://p"}, nil, 10)
	pm.drainOnce(context.Background())

	if !verifiedBy[pkGood] {
		t.Error("good profile: verified = false, want true")
	}
	if verifiedBy[pkBad] {
		t.Error("bad profile: verified = true, want false")
	}
	if v, ok := verifiedBy[pkNone]; !ok || v {
		t.Errorf("no-nip05 profile: stored=%v verified=%v, want stored unverified", ok, v)
	}
	if verifierCalls["good@uni.example"] != 1 || verifierCalls["fake@uni.example"] != 1 || len(verifierCalls) != 2 {
		t.Errorf("verifier calls = %v, want exactly one call per claimed identifier", verifierCalls)
	}
}

func TestVerifyNIP05RejectsInvalidIdentifierWithoutHTTP(t *testing.T) {
	pk := nostr.Generate().Public()
	// Empty and non-identifier strings must short-circuit before any network
	// call — a canceled context would make an HTTP attempt fail differently.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if verifyNIP05(ctx, "", pk) {
		t.Error("empty identifier verified")
	}
	if verifyNIP05(ctx, "not an identifier", pk) {
		t.Error("garbage identifier verified")
	}
}
