package main

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru"
)

// TestSearchBackendFailureStillTerminatesREQ reproduces the 07-24 outage
// symptom directly against the real typesense30142.TSBackend and a real
// khatru relay/websocket client: when Typesense answers every request with
// 503 "Not Ready or Lagging" (its documented backpressure status), a
// subscribing client must still receive EOSE (or CLOSED) instead of hanging
// forever.
func TestSearchBackendFailureStillTerminatesREQ(t *testing.T) {
	// A Typesense stand-in that always answers 503, mirroring the "Not Ready
	// or Lagging" outage state.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"Not Ready or Lagging"}`))
	}))
	defer ts.Close()

	backend := &typesense30142.TSBackend{
		ApiKey:         "xyz",
		Host:           ts.URL,
		CollectionName: "amb_30142",
	}

	relay := khatru.NewRelay()
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return backend.QueryEvents(filter, 250)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		return 0, nil
	}
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error { return nil }

	server := httptest.NewServer(relay)
	defer server.Close()

	url := "ws" + server.URL[4:]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	sub, err := client.Subscribe(ctx, nostr.Filter{
		Kinds:  []nostr.Kind{30142},
		Search: "mathematik",
	}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	defer sub.Unsub()

	select {
	case <-sub.EndOfStoredEvents:
		// good: relay terminated the REQ handshake despite the backend failure.
	case ev := <-sub.Events:
		t.Fatalf("did not expect any event from a failing backend, got %v", ev.ID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for EOSE after search backend failure — client would hang forever")
	}
}

// TestRegistryFanoutFailureStillTerminatesREQ mirrors the production wiring
// more closely: multiple registered content types (as main.go registers one
// per kind family), all backed by Typesense collections on the SAME failing
// host (as in production, one Typesense instance serves every collection),
// composed through the registry exactly as relay.QueryStored does when
// chunk-rerank is disabled (reg.fetch directly). A kind-less filter (as a
// Negentropy sync or a broad client search would send) makes registry.selected
// fan out across every registered type serially.
func TestRegistryFanoutFailureStillTerminatesREQ(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"Not Ready or Lagging"}`))
	}))
	defer ts.Close()

	mkBackend := func(collection string) *typesense30142.TSBackend {
		return &typesense30142.TSBackend{ApiKey: "xyz", Host: ts.URL, CollectionName: collection}
	}

	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkBackend("amb_30142").QueryEvents, chunked: true},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkBackend("longform_30023").QueryEvents, chunked: true},
		contentType{kinds: []nostr.Kind{30818}, fetch: mkBackend("wiki_30818").QueryEvents, chunked: true},
		contentType{kinds: []nostr.Kind{31922, 31923, 31924, 31925}, fetch: mkBackend("calendar_31922").QueryEvents},
	)

	relay := khatru.NewRelay()
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return reg.fetch(filter, 250)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) { return reg.count(filter) }
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error { return nil }

	server := httptest.NewServer(relay)
	defer server.Close()

	url := "ws" + server.URL[4:]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	// No Kinds: registry.selected fans out across all four registered types.
	sub, err := client.Subscribe(ctx, nostr.Filter{Search: "mathematik"}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	defer sub.Unsub()

	select {
	case <-sub.EndOfStoredEvents:
	case ev := <-sub.Events:
		t.Fatalf("did not expect any event from a failing backend, got %v", ev.ID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for EOSE after fan-out search backend failure — client would hang forever")
	}
}

// TestSlowSearchBackendFanoutTerminatesWithinBudget is the actual root-cause
// reproduction: a Typesense that answers slowly (genuinely "lagging", not a
// fast clean error) fanned out across several registered content types —
// exactly registry.fetch's SERIAL loop over registry.selected. Each backend
// only ever costs its own per-call timeout, so no single call hangs forever;
// but with N registered collections on one degraded Typesense instance (this
// relay's actual topology — one Typesense container serves every collection,
// see docker-compose.yml), the SERIAL worst case is N × (that per-call cost).
// At production's registration count that is minutes — indistinguishable
// from "hangs forever" to any real client, which is what made the 07-24
// outage look like a total client-side hang even though the relay was, in
// the strictest sense, still "working".
//
// boundedSeq (query_budget.go) caps the total wait regardless of how many
// backends are registered or how slow any one of them is. This test proves
// the cap is real (not just present): budget is deliberately shorter than
// the unbounded serial worst case, so the assertion only holds when the
// wrapper actually cuts the wait short.
func TestSlowSearchBackendFanoutTerminatesWithinBudget(t *testing.T) {
	const backendDelay = 700 * time.Millisecond
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(backendDelay)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"Not Ready or Lagging"}`))
	}))
	defer slow.Close()

	mkBackend := func(collection string) *typesense30142.TSBackend {
		return &typesense30142.TSBackend{ApiKey: "xyz", Host: slow.URL, CollectionName: collection}
	}

	// Five registered content types, mirroring production's registry shape
	// (AMB, long-form, wiki, calendar, profiles — main.go registers one per
	// kind family). A kind-less filter (a broad client search, or a
	// Negentropy sync) makes registry.selected fan out across all five.
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkBackend("amb_30142").QueryEvents},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkBackend("longform_30023").QueryEvents},
		contentType{kinds: []nostr.Kind{30818}, fetch: mkBackend("wiki_30818").QueryEvents},
		contentType{kinds: []nostr.Kind{31922, 31923, 31924, 31925}, fetch: mkBackend("calendar_31922").QueryEvents},
		contentType{kinds: []nostr.Kind{0}, fetch: mkBackend("profiles_0").QueryEvents},
	)

	// 5 × 700ms ≈ 3.5s unbounded serial worst case. The budget and the test
	// deadline both sit well under that, so this only passes if boundedSeq
	// actually enforces the cap rather than being a no-op wrapper.
	const budget = 500 * time.Millisecond

	relay := khatru.NewRelay()
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return boundedSeq(reg.fetch(filter, 250), budget)
	}
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) { return reg.count(filter) }
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error { return nil }

	server := httptest.NewServer(relay)
	defer server.Close()

	url := "ws" + server.URL[4:]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{})
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	start := time.Now()
	sub, err := client.Subscribe(ctx, nostr.Filter{Search: "mathematik"}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	defer sub.Unsub()

	select {
	case <-sub.EndOfStoredEvents:
		elapsed := time.Since(start)
		t.Logf("EOSE arrived after %s (budget %s, unbounded serial worst case ~%s)", elapsed, budget, 5*backendDelay)
		if elapsed > 2*time.Second {
			t.Fatalf("EOSE arrived but too late (%s) — budget not effectively enforced", elapsed)
		}
	case ev := <-sub.Events:
		t.Fatalf("did not expect any event from a failing backend, got %v", ev.ID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for EOSE — search fetch budget did not terminate the REQ in time (this is the 07-24 outage symptom: client hangs)")
	}
}
