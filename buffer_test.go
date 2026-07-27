package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// fakeWriter records Upsert, Patch, and PatchCommunity calls in arrival order.
type fakeWriter struct {
	mu               sync.Mutex
	upsertBatches    [][]nostr.ID // ids per Upsert call, in arrival order
	patches          []patchCall
	communityPatches []communityPatchCall
	upsertErr        []error
	patchErr         error
}

type patchCall struct {
	EventID string
	Content ContentEntry
}

type communityPatchCall struct {
	DocID       string
	Communities []string
}

func (f *fakeWriter) Upsert(events []nostr.Event) (int, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]nostr.ID, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	f.upsertBatches = append(f.upsertBatches, ids)
	if f.upsertErr != nil {
		// Return the recorded errs for THIS call only, then clear.
		errs := f.upsertErr
		f.upsertErr = nil
		return 0, errs
	}
	return len(events), nil
}

func (f *fakeWriter) Patch(event nostr.Event, content ContentEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patchCall{EventID: event.ID.Hex(), Content: content})
	return f.patchErr
}

func (f *fakeWriter) PatchCommunity(docID string, communities []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.communityPatches = append(f.communityPatches, communityPatchCall{DocID: docID, Communities: communities})
	return nil
}

func mkEvent(t *testing.T, sk nostr.SecretKey, d string, ts int64) nostr.Event {
	t.Helper()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(ts),
		Kind:      nostr.Kind(30142),
		Tags:      nostr.Tags{{"d", d}, {"name", "n-" + d}},
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return evt
}

// unsignedEvent builds a nostr.Event with a synthetic (non-cryptographic)
// ID and no signature, deliberately WITHOUT calling Sign()/SetID(). Those
// call into nostrlib's serializedHash/writeJSONString path, which has a
// pre-existing checkptr bug that intermittently trips `go test -race`
// (confirmed independent of anything in this bundle: pre-existing tests
// using the ordinary Sign-based mkEvent above trip the same panic on their
// own under -race). The buffer/Queue machinery under test never validates
// signatures or recomputes the ID, so a synthetic event is enough — and it
// keeps -race clean for the panic-safety/deadline properties the tests
// below actually check.
func unsignedEvent(sk nostr.SecretKey, d string) nostr.Event {
	var id nostr.ID
	copy(id[:], d)
	return nostr.Event{
		ID:        id,
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(1_700_000_000),
		Kind:      nostr.Kind(30142),
		Tags:      nostr.Tags{{"d", d}, {"name", "n-" + d}},
	}
}

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(t *testing.T, deadline time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out")
}

// TestTSWriteBuffer_BatchesEventOnlyTasks verifies that Queue() calls accumulate
// into batches sized by batchSize, NOT one upsert per event.
func TestTSWriteBuffer_BatchesEventOnlyTasks(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 50, 100*time.Millisecond)
	defer buf.Close()

	sk := nostr.Generate()
	const n = 200
	for i := 0; i < n; i++ {
		buf.Queue(mkEvent(t, sk, "d"+string(rune('a'+i%26))+string(rune('a'+i/26)), int64(i)+1_700_000_000))
	}

	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		total := 0
		for _, b := range w.upsertBatches {
			total += len(b)
		}
		return total >= n
	})

	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, b := range w.upsertBatches {
		total += len(b)
	}
	if total != n {
		t.Fatalf("upserted %d, want %d", total, n)
	}
	// At batchSize=50, n=200: expect ~4 batches, definitely fewer than n.
	if len(w.upsertBatches) >= n {
		t.Errorf("buffer is upserting one event per batch (%d batches for %d events) — batching broken", len(w.upsertBatches), n)
	}
	if len(w.patches) != 0 {
		t.Errorf("got %d patches for event-only tasks, want 0", len(w.patches))
	}
}

// TestTSWriteBuffer_ContentTaskForcesFlushThenPatch is the critical race-
// elimination property. When QueueContent is called for an event whose
// metadata is still pending in the batch, the buffer must flush the batch
// (so the doc exists in Typesense) BEFORE issuing the patch. Otherwise we
// reproduce the setcontent 404.
func TestTSWriteBuffer_ContentTaskForcesFlushThenPatch(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 100, 1*time.Hour) // very long tick → only forced flushes count
	defer buf.Close()

	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "ctf-1", 1_700_000_001)
	e2 := mkEvent(t, sk, "ctf-2", 1_700_000_002)
	e3 := mkEvent(t, sk, "ctf-3", 1_700_000_003)

	buf.Queue(e1)
	buf.Queue(e2)
	// e3 has content — must flush e1+e2+e3 then patch e3.
	buf.QueueContent(e3, ContentEntry{Text: "hi", FetchedAt: 7, Status: "ok"})

	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.patches) >= 1
	})

	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.patches) != 1 {
		t.Fatalf("got %d patches, want 1", len(w.patches))
	}
	if w.patches[0].EventID != e3.ID.Hex() {
		t.Errorf("patched %s, want %s", w.patches[0].EventID, e3.ID.Hex())
	}

	// Verify e3 was upserted BEFORE the patch — i.e., e3.ID appears in some
	// upsert batch. (We can't observe interleave order across goroutines, but
	// we CAN observe that e3 made it into an upsert at all, which combined
	// with the single-goroutine guarantee means upsert preceded patch.)
	sawE3 := false
	for _, b := range w.upsertBatches {
		for _, id := range b {
			if id == e3.ID {
				sawE3 = true
			}
		}
	}
	if !sawE3 {
		t.Errorf("e3 (%s) was patched but never upserted; race not eliminated", e3.ID.Hex())
	}

	// Spot-check: e1 and e2 should also have been upserted (by the forced flush).
	sawE1, sawE2 := false, false
	for _, b := range w.upsertBatches {
		for _, id := range b {
			if id == e1.ID {
				sawE1 = true
			}
			if id == e2.ID {
				sawE2 = true
			}
		}
	}
	if !sawE1 || !sawE2 {
		t.Errorf("forced flush incomplete: e1=%v e2=%v", sawE1, sawE2)
	}
}

// TestTSWriteBuffer_DrainsOnClose verifies Close() flushes any pending batch
// rather than dropping events.
func TestTSWriteBuffer_DrainsOnClose(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 100, 1*time.Hour)

	sk := nostr.Generate()
	for i := 0; i < 5; i++ {
		buf.Queue(mkEvent(t, sk, "drain-"+string(rune('a'+i)), int64(i)+1_700_000_500))
	}
	buf.Close() // should flush all 5

	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, b := range w.upsertBatches {
		total += len(b)
	}
	if total != 5 {
		t.Errorf("drained %d events on close, want 5", total)
	}
}

// TestTSWriteBuffer_CommunityPatchForcesFlushWhenPending verifies that
// QueueCommunityPatch flushes any pending event upserts BEFORE applying the
// community PATCH, WHEN the patch's target doc is itself still sitting
// unflushed in the batch — the same flush-before-patch ordering guarantee as
// QueueContent. This locks in the fix for C1: AMB community stamps routed
// through the buffer so they never race the batched flush.
//
// (Renamed from TestTSWriteBuffer_CommunityPatchForcesFlushThenPatch as part
// of the 2026-07-28 write-path fix: a stamp task now only forces a flush
// when its own doc is pending — see
// TestTSWriteBuffer_CommunityPatchSkipsFlushWhenNotPending for the
// complementary case that collapses the force-flush-per-stamp backlog.)
func TestTSWriteBuffer_CommunityPatchForcesFlushWhenPending(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 100, 1*time.Hour) // very long tick → only forced flushes
	defer buf.Close()

	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "cp-1", 1_700_002_001)
	e2 := mkEvent(t, sk, "cp-2", 1_700_002_002)
	e3 := mkEvent(t, sk, "cp-3", 1_700_002_003)

	buf.Queue(e1)
	buf.Queue(e2)
	buf.Queue(e3)
	// Community patch targets e3's OWN doc id, which is still pending
	// (unflushed) — must flush e1+e2+e3 first, then call PatchCommunity.
	docID, err := tsDocIDFromEvent(e3)
	if err != nil {
		t.Fatalf("tsDocIDFromEvent: %v", err)
	}
	wantCommunities := []string{"C1", "C2"}
	buf.QueueCommunityPatch(docID, wantCommunities)

	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.communityPatches) >= 1
	})

	w.mu.Lock()
	defer w.mu.Unlock()

	// (a) events were upserted
	total := 0
	for _, b := range w.upsertBatches {
		total += len(b)
	}
	if total != 3 {
		t.Errorf("upserted %d events before community patch, want 3", total)
	}

	// (b) community patch recorded with right docID + communities
	if len(w.communityPatches) != 1 {
		t.Fatalf("got %d community patches, want 1", len(w.communityPatches))
	}
	cp := w.communityPatches[0]
	if cp.DocID != docID {
		t.Errorf("community patch docID = %q, want %q", cp.DocID, docID)
	}
	if len(cp.Communities) != 2 || cp.Communities[0] != "C1" || cp.Communities[1] != "C2" {
		t.Errorf("community patch communities = %v, want %v", cp.Communities, wantCommunities)
	}

	// (c) upsert happened before community patch (ordering): all three events
	// must appear in upsertBatches (single-goroutine guarantee: if they're
	// there, they were written before the patch call in the same goroutine).
	sawE1, sawE2, sawE3 := false, false, false
	for _, b := range w.upsertBatches {
		for _, id := range b {
			switch id {
			case e1.ID:
				sawE1 = true
			case e2.ID:
				sawE2 = true
			case e3.ID:
				sawE3 = true
			}
		}
	}
	if !sawE1 || !sawE2 || !sawE3 {
		t.Errorf("flush incomplete before community patch: e1=%v e2=%v e3=%v", sawE1, sawE2, sawE3)
	}
}

// TestTSWriteBuffer_CommunityPatchSkipsFlushWhenNotPending verifies the
// 2026-07-28 write-path fix: a stamp task whose target doc is NOT sitting
// unflushed in the current batch must NOT force a flush of unrelated
// pending events — it should ride the normal batch cadence. Root cause: the
// old unconditional force-flush collapsed batching to ~1 event per upsert
// under stamper load (67 imports : 79 PATCHes in 24s on dev).
func TestTSWriteBuffer_CommunityPatchSkipsFlushWhenNotPending(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 100, 1*time.Hour) // very long tick → only forced flushes
	defer buf.Close()

	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "np-1", 1_700_003_001)
	e2 := mkEvent(t, sk, "np-2", 1_700_003_002)

	buf.Queue(e1)
	buf.Queue(e2)

	// Target a doc that is NOT in the pending batch at all (as if its own
	// upsert already landed in an earlier flush cycle).
	otherDocID := typesense30142.GenerateDocumentID(nostr.GetPublicKey(sk).Hex(), "not-pending")
	buf.QueueCommunityPatch(otherDocID, []string{"C9"})

	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.communityPatches) >= 1
	})

	// Give any (incorrect) flush a moment to show up before asserting absence.
	time.Sleep(50 * time.Millisecond)

	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.upsertBatches) != 0 {
		t.Errorf("got %d upsert batches, want 0 — stamp task for a non-pending doc must not force a flush of unrelated events", len(w.upsertBatches))
	}
	if len(w.communityPatches) != 1 || w.communityPatches[0].DocID != otherDocID {
		t.Fatalf("community patch not recorded correctly: %+v", w.communityPatches)
	}
}

// TestTSWriteBuffer_StampsUnderLoadFlushFarLessThanN is the throughput
// regression test for the 2026-07-28 fix: N content-writes each followed by
// a stamp task for an ALREADY-flushed doc (simulating the common case where
// the async CommunityStamper reconcile lands after the doc's own batch
// already flushed on the normal size/interval cadence) must produce a flush
// count far below N, not one flush per stamp.
func TestTSWriteBuffer_StampsUnderLoadFlushFarLessThanN(t *testing.T) {
	w := &fakeWriter{}
	const batchSize = 10
	const n = 10
	buf := newProjectorBuffer(w, batchSize, 1*time.Hour) // very long tick → only size-triggered flushes
	defer buf.Close()

	sk := nostr.Generate()
	events := make([]nostr.Event, n)
	for i := 0; i < n; i++ {
		events[i] = mkEvent(t, sk, "load-"+string(rune('a'+i)), int64(i)+1_700_004_000)
		buf.Queue(events[i])
	}
	// The nth Queue() hits batchSize and auto-flushes: pendingDocIDs is now
	// empty, so none of the following stamp tasks (one per event, as the
	// CommunityStamper does on every content write) should force a flush.
	for i := 0; i < n; i++ {
		docID, err := tsDocIDFromEvent(events[i])
		if err != nil {
			t.Fatalf("tsDocIDFromEvent: %v", err)
		}
		buf.QueueCommunityPatch(docID, []string{"C1"})
	}

	waitFor(t, 2*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.communityPatches) >= n
	})

	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.communityPatches) != n {
		t.Fatalf("got %d community patches, want %d", len(w.communityPatches), n)
	}
	// Exactly one flush: the automatic batchSize-triggered one. None of the
	// n stamp tasks should have forced an additional flush.
	if len(w.upsertBatches) != 1 {
		t.Errorf("got %d upsert batches for %d stamps, want 1 (flush count must stay far below N=%d)", len(w.upsertBatches), n, n)
	}
}

// TestTSWriteBuffer_DrainDoesNotInheritRetries verifies that during Close()
// drain, a failing upsert for one content task does not cause subsequent
// content tasks in the drain queue to skip their patches. The bug we're
// guarding against: a shared `retries` counter that the drain loop never
// resets, causing later patches to be silently dropped.
func TestTSWriteBuffer_DrainDoesNotInheritRetries(t *testing.T) {
	w := &fakeWriter{}
	// Make the very first Upsert fail; subsequent Upserts succeed.
	w.upsertErr = []error{errors.New("simulated TS outage")}
	buf := newProjectorBuffer(w, 100, 1*time.Hour)

	sk := nostr.Generate()
	e1 := mkEvent(t, sk, "drain-r1", 1_700_001_001)
	e2 := mkEvent(t, sk, "drain-r2", 1_700_001_002)

	// Both content tasks are queued before Close so they sit in the channel.
	buf.QueueContent(e1, ContentEntry{Text: "c1", FetchedAt: 1, Status: "ok"})
	buf.QueueContent(e2, ContentEntry{Text: "c2", FetchedAt: 2, Status: "ok"})

	buf.Close()

	w.mu.Lock()
	defer w.mu.Unlock()
	// Drain must not block on retry sleeps and must not silently drop e2's
	// patch just because e1's upsert failed. Both patches should land,
	// because Upsert retries are not the drain's job.
	if len(w.patches) != 2 {
		t.Fatalf("got %d patches after drain, want 2 (drain dropped patches due to retry-state bleed)", len(w.patches))
	}
}

// TestTSWriteBuffer_QueueAfterCloseDropsWithoutPanic locks in the CRITICAL
// fix: b.ch is never closed, so Queue/QueueContent/QueueCommunityPatch
// called after Close() has fully returned log-and-drop instead of sending
// on a closed channel (which would panic).
func TestTSWriteBuffer_QueueAfterCloseDropsWithoutPanic(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 10, 10*time.Millisecond)

	sk := nostr.Generate()
	buf.Queue(unsignedEvent(sk, "pre-close"))
	buf.Close()

	e2 := unsignedEvent(sk, "post-close")
	docID, err := tsDocIDFromEvent(e2)
	if err != nil {
		t.Fatalf("tsDocIDFromEvent: %v", err)
	}

	assertNoPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s after Close panicked: %v", name, r)
			}
		}()
		fn()
	}
	assertNoPanic("Queue", func() { buf.Queue(e2) })
	assertNoPanic("QueueContent", func() {
		buf.QueueContent(e2, ContentEntry{Text: "x", FetchedAt: 1, Status: "ok"})
	})
	assertNoPanic("QueueCommunityPatch", func() { buf.QueueCommunityPatch(docID, []string{"C1"}) })

	// Post-close calls must have been dropped, not processed.
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.upsertBatches {
		for _, id := range b {
			if id == e2.ID {
				t.Errorf("e2 was upserted after Close, want dropped")
			}
		}
	}
	if len(w.patches) != 0 {
		t.Errorf("got %d patches after Close, want 0 (post-close QueueContent should drop)", len(w.patches))
	}
	if len(w.communityPatches) != 0 {
		t.Errorf("got %d community patches after Close, want 0", len(w.communityPatches))
	}
}

// TestTSWriteBuffer_HammerCloseNoPanic hammers Queue from many concurrent
// goroutines while Close() runs, asserting the send never panics — the
// CRITICAL property this bundle fixes (b.ch is never closed, so there is no
// closed channel for a racing send to land on).
//
// Uses unsignedEvent (built once, single-threaded, before any goroutine
// starts, reused by value across all producers) rather than mkEvent/Sign —
// see unsignedEvent's doc comment for why: nostrlib's Sign/SetID path has a
// pre-existing checkptr bug that intermittently trips `go test -race` on
// its own, unrelated to concurrency in the code under test here.
func TestTSWriteBuffer_HammerCloseNoPanic(t *testing.T) {
	w := &fakeWriter{}
	buf := newProjectorBuffer(w, 20, 5*time.Millisecond)

	sk := nostr.Generate()
	event := unsignedEvent(sk, "hammer")

	const producers = 20
	var wg sync.WaitGroup
	var panicked atomic.Bool
	stop := make(chan struct{})

	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked.Store(true)
				}
			}()
			for {
				select {
				case <-stop:
					return
				default:
					buf.Queue(event)
				}
			}
		}()
	}

	// Let producers spam Queue for a moment, then close concurrently with
	// them still running — this is the exact race the fix targets.
	time.Sleep(20 * time.Millisecond)
	buf.Close()
	close(stop)
	wg.Wait()

	if panicked.Load() {
		t.Fatal("Queue panicked under concurrent Close (send on closed channel)")
	}
}

// blockingWriter's Upsert blocks until unblock is closed, simulating a
// Typesense call that never returns — the scenario tsDrainDeadline exists
// to bound.
type blockingWriter struct {
	unblock chan struct{}
}

func (w *blockingWriter) Upsert(events []nostr.Event) (int, []error) {
	<-w.unblock
	return len(events), nil
}
func (w *blockingWriter) Patch(nostr.Event, ContentEntry) error { return nil }
func (w *blockingWriter) PatchCommunity(string, []string) error { return nil }

// TestTSWriteBuffer_DrainDeadlineHonored verifies Close() returns within
// drainDeadline even when the writer's Upsert call never returns, and logs
// the abandoned queue depth instead of hanging shutdown indefinitely.
//
// A single plain Queue()'d event with batchSize=10 never triggers the
// writer from the live loop (it only appends to the in-memory batch below
// the size threshold), so the only place Upsert can be invoked is drain's
// final flush — making the blocking call deterministically exercise the
// deadline path rather than racing the live loop.
func TestTSWriteBuffer_DrainDeadlineHonored(t *testing.T) {
	w := &blockingWriter{unblock: make(chan struct{})}
	defer close(w.unblock) // let the abandoned goroutine finish eventually

	buf := newProjectorBuffer(w, 10, 1*time.Hour)
	buf.drainDeadline = 50 * time.Millisecond

	sk := nostr.Generate()
	buf.Queue(unsignedEvent(sk, "hang"))

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	start := time.Now()
	buf.Close()
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("Close() took %v, want well under 1s (drainDeadline=50ms)", elapsed)
	}
	if !strings.Contains(logBuf.String(), "drain deadline") {
		t.Errorf("expected a 'drain deadline' log line, got: %q", logBuf.String())
	}
}
