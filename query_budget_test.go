package main

import (
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// TestBoundedCountTerminatesWithinBudget is the COUNT-path analogue of
// TestSlowSearchBackendFanoutTerminatesWithinBudget: a registry.count-shaped
// function that blocks well past the budget (standing in for a
// registry.count serial fan-out over several content types on one degraded
// Typesense instance — the exact same hang mechanism as the REQ path, one
// envelope over) must not make boundedCount's caller wait longer than
// budget.
func TestBoundedCountTerminatesWithinBudget(t *testing.T) {
	const backendDelay = 2 * time.Second
	const budget = 100 * time.Millisecond

	slow := func(nostr.Filter) (uint32, error) {
		time.Sleep(backendDelay)
		return 42, nil
	}

	wrapped := boundedCount(slow, budget)

	start := time.Now()
	n, err := wrapped(nostr.Filter{})
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("boundedCount did not return within budget: took %s (budget %s, backend delay %s)", elapsed, budget, backendDelay)
	}
	if err == nil {
		t.Fatalf("expected a timeout error when the wrapped count exceeds budget, got nil (n=%d)", n)
	}
	if n != 0 {
		t.Fatalf("expected 0 on timeout, got %d", n)
	}
}

// TestBoundedCountPassesThroughFastResult ensures the wrapper is transparent
// for the common case: a count that finishes well inside budget returns the
// real value and no error.
func TestBoundedCountPassesThroughFastResult(t *testing.T) {
	fast := func(nostr.Filter) (uint32, error) {
		return 7, nil
	}

	wrapped := boundedCount(fast, 1*time.Second)

	n, err := wrapped(nostr.Filter{})
	if err != nil {
		t.Fatalf("unexpected error from a fast count: %v", err)
	}
	if n != 7 {
		t.Fatalf("expected 7, got %d", n)
	}
}

// TestBoundedCountPropagatesUnderlyingError ensures a fast, clean error from
// the wrapped function (e.g. a Typesense error surfaced synchronously) is
// passed straight through, not masked by the budget wrapper.
func TestBoundedCountPropagatesUnderlyingError(t *testing.T) {
	wantErr := errors.New("boom")
	failing := func(nostr.Filter) (uint32, error) {
		return 0, wantErr
	}

	wrapped := boundedCount(failing, 1*time.Second)

	n, err := wrapped(nostr.Filter{})
	if err != wantErr {
		t.Fatalf("expected the underlying error to propagate unchanged, got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

// TestBoundedCountZeroBudgetDisables mirrors boundedSeq's convention:
// budget<=0 means "no cap" — the wrapper must not add a timeout at all, even
// one that would never fire in this test's timescale, so the intent is
// explicit rather than accidental via a very large number.
func TestBoundedCountZeroBudgetDisables(t *testing.T) {
	calls := 0
	fn := func(nostr.Filter) (uint32, error) {
		calls++
		return 5, nil
	}

	wrapped := boundedCount(fn, 0)

	n, err := wrapped(nostr.Filter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5, got %d", n)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call to the underlying fn, got %d", calls)
	}
}
