package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
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

// TestTSWriteBuffer_CommunityPatchForcesFlushThenPatch verifies that
// QueueCommunityPatch flushes any pending event upserts BEFORE applying the
// community PATCH — the same flush-before-patch ordering guarantee as
// QueueContent. This locks in the fix for C1: AMB community stamps routed
// through the buffer so they never race the batched flush.
func TestTSWriteBuffer_CommunityPatchForcesFlushThenPatch(t *testing.T) {
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
	// Community patch: must flush e1+e2+e3 first, then call PatchCommunity.
	const testDocID = "test-doc-id-community"
	wantCommunities := []string{"C1", "C2"}
	buf.QueueCommunityPatch(testDocID, wantCommunities)

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
	if cp.DocID != testDocID {
		t.Errorf("community patch docID = %q, want %q", cp.DocID, testDocID)
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
