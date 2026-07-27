package main

import (
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
