package main

import (
	"errors"
	"sync"
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
