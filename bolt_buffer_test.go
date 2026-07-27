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
	"fiatjaf.com/nostr/eventstore"
)

// fakeBoltWriter is a minimal boltEventWriter that lets tests script a
// sequence of SaveEvent/ReplaceEvent outcomes without touching real BoltDB.
type fakeBoltWriter struct {
	mu sync.Mutex

	// errs is consumed in order, one entry per SaveEvent/ReplaceEvent call.
	// When exhausted, calls succeed (nil error).
	errs []error

	saveCalls    []nostr.Event
	replaceCalls []nostr.Event
}

func (f *fakeBoltWriter) nextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeBoltWriter) SaveEvent(evt nostr.Event) error {
	f.mu.Lock()
	f.saveCalls = append(f.saveCalls, evt)
	f.mu.Unlock()
	return f.nextErr()
}

func (f *fakeBoltWriter) ReplaceEvent(evt nostr.Event) ([]nostr.Event, error) {
	f.mu.Lock()
	f.replaceCalls = append(f.replaceCalls, evt)
	f.mu.Unlock()
	return nil, f.nextErr()
}

func (f *fakeBoltWriter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saveCalls) + len(f.replaceCalls)
}

// TestBoltBuffer_DuplicateErrorDoesNotRetry verifies that ErrDupEvent (routine
// under duplicate-check races/projection lag) returns immediately with no
// backoff sleep, instead of burning 31s across 5 retries and stalling the
// single sequential worker goroutine.
func TestBoltBuffer_DuplicateErrorDoesNotRetry(t *testing.T) {
	w := &fakeBoltWriter{errs: []error{eventstore.ErrDupEvent}}
	buf := &BoltWriteBuffer{boltDB: w}

	sk := nostr.Generate()
	evt := mkEvent(t, sk, "d1", 1_700_000_000)

	start := time.Now()
	buf.process(boltOp{event: evt, replace: false})
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("process() took %v for a duplicate error, expected a fast no-op (<200ms)", elapsed)
	}
	if got := w.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 SaveEvent call (no retries), got %d", got)
	}
}

// TestBoltBuffer_InvalidPrefixErrorDoesNotRetry verifies any "invalid:"/
// "blocked:"/"duplicate:" prefixed error (permanent per NIP-01 OK semantics)
// is treated as non-retryable, matching ErrDupEvent's fast-path behavior.
func TestBoltBuffer_InvalidPrefixErrorDoesNotRetry(t *testing.T) {
	w := &fakeBoltWriter{errs: []error{errors.New("invalid: id is computed incorrectly")}}
	buf := &BoltWriteBuffer{boltDB: w}

	sk := nostr.Generate()
	evt := mkEvent(t, sk, "d1", 1_700_000_000)

	start := time.Now()
	buf.process(boltOp{event: evt, replace: false})
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("process() took %v for an invalid: error, expected a fast single attempt (<200ms)", elapsed)
	}
	if got := w.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 SaveEvent call (no retries), got %d", got)
	}
}

// TestBoltBuffer_TransientErrorStillRetries verifies genuine transient (I/O)
// errors keep the existing retry/backoff behavior and succeed once the
// backend recovers. Uses an injected sleepFn to assert backoff sequence
// without wall-clock delays.
func TestBoltBuffer_TransientErrorStillRetries(t *testing.T) {
	w := &fakeBoltWriter{errs: []error{errors.New("i/o error: disk full"), errors.New("i/o error: disk full")}}
	buf := &BoltWriteBuffer{boltDB: w}

	// Record sleep calls instead of actually sleeping.
	var recordedSleeps []time.Duration
	buf.sleepFn = func(d time.Duration) {
		recordedSleeps = append(recordedSleeps, d)
	}

	sk := nostr.Generate()
	evt := mkEvent(t, sk, "d1", 1_700_000_000)

	buf.process(boltOp{event: evt, replace: false})

	// Two failures before success means two backoff sleeps: 1s + 2s.
	if len(recordedSleeps) != 2 {
		t.Fatalf("expected 2 sleep calls (1s and 2s), got %d: %v", len(recordedSleeps), recordedSleeps)
	}
	if recordedSleeps[0] != 1*time.Second {
		t.Fatalf("first backoff should be 1s, got %v", recordedSleeps[0])
	}
	if recordedSleeps[1] != 2*time.Second {
		t.Fatalf("second backoff should be 2s, got %v", recordedSleeps[1])
	}
	if got := w.callCount(); got != 3 {
		t.Fatalf("expected 3 SaveEvent calls (2 failures + 1 success), got %d", got)
	}
}

// TestBoltBuffer_ExhaustedRetriesDrops verifies a persistently failing
// transient error still gives up after boltMaxRetries attempts (unchanged
// behavior), rather than retrying forever. Uses an injected sleepFn to
// assert backoff sequence (1s, 2s, 4s, 8s, 16s) without wall-clock delays.
func TestBoltBuffer_ExhaustedRetriesDrops(t *testing.T) {
	persistentErr := errors.New("i/o error: disk full")
	errs := make([]error, boltMaxRetries)
	for i := range errs {
		errs[i] = persistentErr
	}
	w := &fakeBoltWriter{errs: errs}
	buf := &BoltWriteBuffer{boltDB: w}

	// Record sleep calls instead of actually sleeping.
	var recordedSleeps []time.Duration
	buf.sleepFn = func(d time.Duration) {
		recordedSleeps = append(recordedSleeps, d)
	}

	sk := nostr.Generate()
	evt := mkEvent(t, sk, "d1", 1_700_000_000)

	buf.process(boltOp{event: evt, replace: false})

	// All 5 retries should fail, with sleeps: 1s, 2s, 4s, 8s, 16s.
	expectedSleeps := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}
	if len(recordedSleeps) != len(expectedSleeps) {
		t.Fatalf("expected %d sleep calls, got %d: %v", len(expectedSleeps), len(recordedSleeps), recordedSleeps)
	}
	for i, expected := range expectedSleeps {
		if recordedSleeps[i] != expected {
			t.Fatalf("sleep[%d] should be %v, got %v", i, expected, recordedSleeps[i])
		}
	}
	if got := w.callCount(); got != boltMaxRetries {
		t.Fatalf("expected %d SaveEvent calls (all retries exhausted), got %d", boltMaxRetries, got)
	}
}

// TestBoltBuffer_ReplaceEventDuplicateDoesNotRetry verifies the ReplaceEvent
// path (addressable events) gets the same fast-path treatment.
func TestBoltBuffer_ReplaceEventDuplicateDoesNotRetry(t *testing.T) {
	w := &fakeBoltWriter{errs: []error{eventstore.ErrDupEvent}}
	buf := &BoltWriteBuffer{boltDB: w}

	sk := nostr.Generate()
	evt := mkEvent(t, sk, "d1", 1_700_000_000)

	start := time.Now()
	buf.process(boltOp{event: evt, replace: true})
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("process() took %v for a duplicate error via ReplaceEvent, expected fast no-op (<200ms)", elapsed)
	}
	if got := w.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 ReplaceEvent call (no retries), got %d", got)
	}
}

// TestBoltBuffer_WorkerContinuesAfterPermanentRejection verifies the single
// sequential worker keeps draining the channel after a permanent rejection —
// the original bug had a duplicate stall the whole queue for 31s per
// occurrence, blocking every op behind it.
func TestBoltBuffer_WorkerContinuesAfterPermanentRejection(t *testing.T) {
	w := &fakeBoltWriter{errs: []error{eventstore.ErrDupEvent}}
	buf := NewBoltWriteBuffer(w)
	defer buf.Close()

	sk := nostr.Generate()
	dup := mkEvent(t, sk, "d-dup", 1_700_000_000)
	following := mkEvent(t, sk, "d-following", 1_700_000_001)

	start := time.Now()
	buf.Queue(dup, false)
	buf.Queue(following, false)

	waitFor(t, 2*time.Second, func() bool {
		return w.callCount() >= 2
	})
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("second op took %v to process after a duplicate ahead of it in queue, expected <1s (no 31s stall)", elapsed)
	}
}

// TestBoltBuffer_QueueAfterCloseDropsWithoutPanic locks in the CRITICAL fix:
// b.ch is never closed, so Queue() called after Close() has fully returned
// logs-and-drops (returning false) instead of sending on a closed channel
// (which would panic).
func TestBoltBuffer_QueueAfterCloseDropsWithoutPanic(t *testing.T) {
	w := &fakeBoltWriter{}
	buf := NewBoltWriteBuffer(w)

	sk := nostr.Generate()
	buf.Queue(unsignedEvent(sk, "pre-close"), false)
	buf.Close()

	before := w.callCount()

	var queued bool
	var panicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		queued = buf.Queue(unsignedEvent(sk, "post-close"), false)
	}()

	if panicked {
		t.Fatal("Queue after Close panicked (send on closed channel)")
	}
	if queued {
		t.Error("Queue after Close returned true, want false (dropped)")
	}
	if got := w.callCount(); got != before {
		t.Errorf("post-close event reached the writer (callCount %d -> %d), want dropped", before, got)
	}
}

// TestBoltBuffer_HammerCloseNoPanic hammers Queue from many concurrent
// goroutines while Close() runs, asserting the send never panics. Uses
// unsignedEvent (built once, single-threaded, before any goroutine starts,
// reused by value across all producers) rather than mkEvent/Sign — see
// unsignedEvent's doc comment: nostrlib's Sign/SetID path has a
// pre-existing checkptr bug that intermittently trips `go test -race` on
// its own, unrelated to concurrency in the code under test here.
func TestBoltBuffer_HammerCloseNoPanic(t *testing.T) {
	w := &fakeBoltWriter{}
	buf := NewBoltWriteBuffer(w)

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
					buf.Queue(event, false)
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

// blockingBoltWriter's SaveEvent/ReplaceEvent block until unblock is
// closed, simulating a BoltDB call that never returns — the scenario
// boltDrainDeadline exists to bound.
type blockingBoltWriter struct {
	unblock chan struct{}
}

func (w *blockingBoltWriter) SaveEvent(nostr.Event) error {
	<-w.unblock
	return nil
}

func (w *blockingBoltWriter) ReplaceEvent(nostr.Event) ([]nostr.Event, error) {
	<-w.unblock
	return nil, nil
}

// TestBoltBuffer_DrainDeadlineHonored verifies drain() returns within
// drainDeadline even when the writer's SaveEvent call never returns, and
// logs the abandoned queue depth instead of hanging shutdown indefinitely.
//
// drain() is exercised directly (not through Close()/run()) so the test is
// deterministic: BoltWriteBuffer processes one op at a time with no
// batching, so if the run() goroutine's live loop happened to dequeue the
// op before drain() started, the blocking call would be unbounded (a
// pre-existing, out-of-scope property of the single already-in-flight-op
// case — see process()'s own ~31s retry budget). Calling drain() directly
// on a buffer whose channel already holds the op sidesteps that race
// entirely and tests the deadline bound itself.
func TestBoltBuffer_DrainDeadlineHonored(t *testing.T) {
	w := &blockingBoltWriter{unblock: make(chan struct{})}
	defer close(w.unblock) // let the abandoned goroutine finish eventually

	buf := &BoltWriteBuffer{
		boltDB:        w,
		ch:            make(chan boltOp, 10),
		drainDeadline: 50 * time.Millisecond,
	}

	sk := nostr.Generate()
	buf.ch <- boltOp{event: unsignedEvent(sk, "bolt-hang")}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	start := time.Now()
	buf.drain()
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("drain() took %v, want well under 1s (drainDeadline=50ms)", elapsed)
	}
	if !strings.Contains(logBuf.String(), "drain deadline") {
		t.Errorf("expected a 'drain deadline' log line, got: %q", logBuf.String())
	}
}
