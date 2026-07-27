package main

import (
	"fmt"
	"iter"
	"log"
	"time"

	"fiatjaf.com/nostr"
)

// DefaultQueryFetchBudget bounds the wall-clock time a single REQ's fetch is
// allowed to run before the relay gives up waiting and lets khatru terminate
// the subscription handshake (EOSE) with whatever was collected so far.
//
// Why this exists: each Typesense-backed contentType.fetch is already bounded
// by its own HTTP client timeout (httpTimeout, 30s, in nostrlib's
// typesense30142 package) — a single stalled or "Not Ready or Lagging"
// backend can never block forever. But registry.fetch (content_registry.go)
// fans a kind-less filter (e.g. a broad client search, or a Negentropy sync)
// out across EVERY registered content type SERIALLY. With N registered
// collections all pointed at the same degraded Typesense instance — the
// relay's actual topology, see docker-compose.yml — worst case is N × 30s
// before EOSE ships. At today's registration count that is several minutes,
// which is indistinguishable from "hangs forever" to any real client (most
// give up long before that). This wrapper caps the total regardless of how
// many content types are registered or how slow any one of them is.
const DefaultQueryFetchBudget = 20 * time.Second

// boundedSeq wraps an iter.Seq[nostr.Event] so that iterating it never runs
// longer than budget. The underlying seq keeps running to completion in its
// own goroutine — bounded by its own lower-level timeouts — but the consumer
// (khatru's handleRequest, which drives the for-range that eventually
// triggers EOSE) stops waiting once budget elapses, receiving only the events
// that arrived in time. This turns an unbounded/slow-backend stall into a
// bounded, predictable one: the REQ handshake always terminates.
func boundedSeq(seq iter.Seq[nostr.Event], budget time.Duration) iter.Seq[nostr.Event] {
	if budget <= 0 {
		return seq
	}
	return func(yield func(nostr.Event) bool) {
		results := make(chan nostr.Event)
		stop := make(chan struct{})

		go func() {
			defer close(results)
			for ev := range seq {
				select {
				case results <- ev:
				case <-stop:
					return
				}
			}
		}()

		timer := time.NewTimer(budget)
		defer timer.Stop()

		for {
			select {
			case ev, ok := <-results:
				if !ok {
					return
				}
				if !yield(ev) {
					close(stop)
					return
				}
			case <-timer.C:
				log.Printf("query: search fetch exceeded %s budget, terminating REQ early (EOSE with partial/empty results)", budget)
				close(stop)
				return
			}
		}
	}
}

// boundedCount wraps a registry.count-shaped function so a single COUNT
// request never blocks longer than budget. registry.count (content_registry.go)
// has the exact same serial fan-out hazard as registry.fetch — it loops over
// every selected content type's own count call, each bounded only by its own
// HTTP client timeout — so a kind-less COUNT during a degraded/lagging
// Typesense can stack up the same N × (per-call timeout) stall that
// boundedSeq guards QueryStored against. Unlike a query result, a count has
// no partial value to stream back as it arrives, so on timeout this reports
// zero with a descriptive error; khatru's handleCountRequest
// (nostrlib/khatru/responding.go) sends that error as a NOTICE and still
// replies with a CountEnvelope carrying the zero, so the COUNT round-trip
// always terminates within budget instead of blocking the websocket handler.
// budget<=0 disables the wrapper (matching boundedSeq's convention) and
// returns fn unchanged.
func boundedCount(fn func(nostr.Filter) (uint32, error), budget time.Duration) func(nostr.Filter) (uint32, error) {
	if budget <= 0 {
		return fn
	}
	return func(filter nostr.Filter) (uint32, error) {
		type result struct {
			n   uint32
			err error
		}
		done := make(chan result, 1)

		go func() {
			n, err := fn(filter)
			done <- result{n, err}
		}()

		timer := time.NewTimer(budget)
		defer timer.Stop()

		select {
		case res := <-done:
			return res.n, res.err
		case <-timer.C:
			log.Printf("query: COUNT exceeded %s budget, returning early with a timeout error", budget)
			return 0, fmt.Errorf("count timed out after %s", budget)
		}
	}
}
