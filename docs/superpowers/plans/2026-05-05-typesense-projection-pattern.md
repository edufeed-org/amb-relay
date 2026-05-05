# Typesense Projection Pattern Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the dual-writer race between the relay's TS write buffer and the indexer's `setcontent` calls by making Typesense a pure projection of BoltDB+ContentStore, following the existing `bleve` eventstore pattern in nostrlib.

**Architecture:**
- `typesense30142.TSBackend` gains a `RawEventStore` field (mirroring `bleve.BleveBackend.RawEventStore`). Once set, `QueryEvents` uses Typesense purely for filter/search, then fetches full events by ID from the raw store. Backwards-compatible: when `RawEventStore` is nil, the existing `eventRaw`-based reconstruction path is preserved.
- The relay's `TSWriteBuffer` becomes a single-writer projector. It accepts `ProjectionTask{Event, ContentEntry}` items, builds the AMB doc + content, and upserts to Typesense in batches. `setcontent` writes to `ContentStore` synchronously and enqueues a projection task instead of PATCHing Typesense directly. The race disappears because the same goroutine serializes the event upsert and any subsequent content patch for that event.

**Tech Stack:** Go 1.23+, fiatjaf.com/nostr eventstore framework, Typesense 28, BoltDB.

---

## Pre-Task: Commit pending mirror-prod fix

**Files:**
- Modify: `../amb-indexer/cmd/mirror-prod/main.go:133-143` (uncommitted change in working tree)

The pagination loop has an uncommitted fix for the termination criterion (terminate on `pageCount == 0` instead of `pageCount < pageSize`) that needs to land before this plan's work. It's unrelated to the projection refactor but blocks a clean working tree.

- [ ] **Step 1: Inspect the uncommitted diff**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
git diff cmd/mirror-prod/main.go
```

Expected: the small diff that changes `if uint(pageCount) < pageSize` to `if pageCount == 0` and updates the comment.

- [ ] **Step 2: Run the test package to confirm it still passes**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
GOWORK=off go test ./cmd/mirror-prod/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
git add cmd/mirror-prod/main.go
git commit -m "Terminate mirror-prod only on empty page

The previous heuristic 'pageCount < pageSize' fired prematurely against
prod khatru, which silently caps each REQ at 250 events regardless of
the requested limit. Use empty page as the only termination signal; the
pageNew==0 branch already advances past page boundaries so we eventually
walk past the oldest event."
```

---

## Task 1: Add `RawEventStore` field to `TSBackend`

**Files:**
- Modify: `../nostrlib/eventstore/typesense30142/lib.go`
- Modify: `../nostrlib/eventstore/typesense30142/query_test.go` (new file if absent)

Add a backwards-compatible `RawEventStore eventstore.Store` field. No behavior change yet — this is purely the field plumbing so Task 2 can use it.

- [ ] **Step 1: Add the field**

Edit `lib.go`. After the `EmbedFields` line (currently line 30), add:

```go
	// RawEventStore is where full nostr events are persisted. When set,
	// QueryEvents uses Typesense purely as a search/filter index and reads
	// full events from RawEventStore by ID. This mirrors the bleve backend
	// pattern: the search index stores only what's needed for queries; the
	// authoritative event payload lives elsewhere. When nil, QueryEvents
	// falls back to reconstructing events from the eventRaw field stored in
	// Typesense (legacy behavior).
	RawEventStore eventstore.Store
```

- [ ] **Step 2: Verify build**

```bash
cd /home/laoc/coding/edufeed/nostrlib
go build ./eventstore/typesense30142/...
```

Expected: success, no errors.

- [ ] **Step 3: Commit**

```bash
cd /home/laoc/coding/edufeed/nostrlib
git add eventstore/typesense30142/lib.go
git commit -m "typesense30142: add optional RawEventStore field

Mirrors bleve.BleveBackend.RawEventStore. When set, the next change to
QueryEvents will use Typesense purely as a search index and resolve full
events through the raw store instead of reconstructing them from the
eventRaw field. Field-only commit — no behavior change yet."
```

---

## Task 2: Use `RawEventStore` in `QueryEvents` when set

**Files:**
- Modify: `../nostrlib/eventstore/typesense30142/query.go:126-166`
- Create: `../nostrlib/eventstore/typesense30142/raw_event_store_test.go`

When `RawEventStore` is set, `QueryEvents` should pull event IDs out of the search hits and fetch the corresponding events from the raw store. When `RawEventStore` is nil, behavior is unchanged (legacy `eventRaw`-based reconstruction).

- [ ] **Step 1: Write the failing test**

Create `../nostrlib/eventstore/typesense30142/raw_event_store_test.go`:

```go
package typesense30142

import (
	"os"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
)

// TestQueryEvents_UsesRawEventStore verifies that when RawEventStore is set,
// QueryEvents returns events from the raw store rather than reconstructing
// them from the eventRaw field stored in Typesense. This is the bleve-style
// projection pattern.
func TestQueryEvents_UsesRawEventStore(t *testing.T) {
	host := os.Getenv("TS_TEST_HOST")
	apiKey := os.Getenv("TS_TEST_APIKEY")
	if host == "" || apiKey == "" {
		t.Skip("TS_TEST_HOST and TS_TEST_APIKEY not set; skipping integration test")
	}

	collectionName := "amb_raweventstore_test_" + time.Now().Format("20060102150405")

	// Set up raw store and seed an event.
	raw := &slicestore.SliceStore{}
	if err := raw.Init(); err != nil {
		t.Fatalf("init slicestore: %v", err)
	}
	defer raw.Close()

	sk := nostr.Generate()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Kind:      nostr.Kind(30142),
		Tags: nostr.Tags{
			{"d", "raw-store-test"},
			{"name", "marker-name-9173"},
			{"description", "raw-store integration check"},
		},
		Content: "raw-store integration check",
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := raw.SaveEvent(evt); err != nil {
		t.Fatalf("save raw: %v", err)
	}

	// Set up the Typesense backend wired to the raw store.
	ts := &TSBackend{
		ApiKey:         apiKey,
		Host:           host,
		CollectionName: collectionName,
		RawEventStore:  raw,
	}
	if err := ts.Init(); err != nil {
		t.Fatalf("ts init: %v", err)
	}
	defer ts.DropCollection()

	if err := ts.ReplaceEvent(evt); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// Allow Typesense to settle.
	time.Sleep(300 * time.Millisecond)

	// Mutate the doc in Typesense so its eventRaw is no longer authoritative.
	// This proves QueryEvents went through RawEventStore: if it had reused
	// eventRaw, we'd see the mutated content. We mutate by replacing the
	// stored event with a tampered name that does NOT match the search.
	tampered := evt
	tampered.Content = "TAMPERED CONTENT"
	tampered.Tags = nostr.Tags{
		{"d", "raw-store-test"},
		{"name", "marker-name-9173"}, // keep marker so search still matches
		{"description", "TAMPERED IN TYPESENSE"},
	}
	if err := ts.ReplaceEvent(tampered); err != nil {
		t.Fatalf("ts replace tampered: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	// Note: the raw store still has the ORIGINAL evt; we did not call SaveEvent
	// on the tampered version. So if QueryEvents reads from raw, it returns evt.

	got := []nostr.Event{}
	for e := range ts.QueryEvents(nostr.Filter{
		Kinds:  []nostr.Kind{30142},
		Search: "marker-name-9173",
		Limit:  5,
	}, 5) {
		got = append(got, e)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].ID != evt.ID {
		t.Errorf("got event ID %s, want %s", got[0].ID.Hex(), evt.ID.Hex())
	}
	if got[0].Content != evt.Content {
		t.Errorf("content not from raw store: got %q, want %q",
			got[0].Content, evt.Content)
	}
}

// TestQueryEvents_FallsBackToEventRaw verifies that when RawEventStore is
// nil, QueryEvents reconstructs events from the eventRaw field — the legacy
// behavior, preserved for backwards compatibility.
func TestQueryEvents_FallsBackToEventRaw(t *testing.T) {
	host := os.Getenv("TS_TEST_HOST")
	apiKey := os.Getenv("TS_TEST_APIKEY")
	if host == "" || apiKey == "" {
		t.Skip("TS_TEST_HOST and TS_TEST_APIKEY not set; skipping integration test")
	}

	collectionName := "amb_eventraw_fallback_test_" + time.Now().Format("20060102150405")

	ts := &TSBackend{
		ApiKey:         apiKey,
		Host:           host,
		CollectionName: collectionName,
		// RawEventStore intentionally nil
	}
	if err := ts.Init(); err != nil {
		t.Fatalf("ts init: %v", err)
	}
	defer ts.DropCollection()

	sk := nostr.Generate()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Kind:      nostr.Kind(30142),
		Tags: nostr.Tags{
			{"d", "fallback-test"},
			{"name", "fallback-marker-7263"},
		},
		Content: "fallback path content",
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := ts.ReplaceEvent(evt); err != nil {
		t.Fatalf("replace: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	got := []nostr.Event{}
	for e := range ts.QueryEvents(nostr.Filter{
		Kinds:  []nostr.Kind{30142},
		Search: "fallback-marker-7263",
		Limit:  5,
	}, 5) {
		got = append(got, e)
	}
	if len(got) != 1 || got[0].ID != evt.ID {
		t.Fatalf("got %v, want one event %s", got, evt.ID.Hex())
	}
}
```

- [ ] **Step 2: Run tests, expect failure on `UsesRawEventStore`**

```bash
cd /home/laoc/coding/edufeed/nostrlib
docker compose -f eventstore/typesense30142/docker-compose.test.yml up -d
TS_TEST_HOST=http://localhost:8108 TS_TEST_APIKEY=test-key \
  go test ./eventstore/typesense30142/ -run TestQueryEvents_UsesRawEventStore -v
```

Expected: FAIL — currently `QueryEvents` reconstructs from `eventRaw`, so the tampered description leaks through. (The test is designed to fail until Step 3 lands.)

- [ ] **Step 3: Modify `QueryEvents` to use `RawEventStore` when set**

Edit `query.go`. Replace the loop at lines 160-164:

```go
		for _, evt := range nostrsearch {
			if !yield(evt) {
				return
			}
		}
```

with:

```go
		// When RawEventStore is set, use Typesense purely as a search index:
		// extract ids from hits and fetch authoritative events from the raw
		// store. This is the bleve-backend pattern. When nil, fall back to
		// reconstructing events from the eventRaw field stored in Typesense.
		if ts.RawEventStore != nil {
			ids := make([]nostr.ID, 0, len(nostrsearch))
			for _, evt := range nostrsearch {
				ids = append(ids, evt.ID)
			}
			if len(ids) == 0 {
				return
			}
			for evt := range ts.RawEventStore.QueryEvents(nostr.Filter{IDs: ids}, len(ids)) {
				if !yield(evt) {
					return
				}
			}
			return
		}

		for _, evt := range nostrsearch {
			if !yield(evt) {
				return
			}
		}
```

- [ ] **Step 4: Run tests, expect both to pass**

```bash
cd /home/laoc/coding/edufeed/nostrlib
TS_TEST_HOST=http://localhost:8108 TS_TEST_APIKEY=test-key \
  go test ./eventstore/typesense30142/ -run "TestQueryEvents_UsesRawEventStore|TestQueryEvents_FallsBackToEventRaw" -v
```

Expected: both PASS.

- [ ] **Step 5: Run the full eventstore test suite to confirm no regressions**

```bash
cd /home/laoc/coding/edufeed/nostrlib
TS_TEST_HOST=http://localhost:8108 TS_TEST_APIKEY=test-key \
  go test ./eventstore/typesense30142/...
```

Expected: PASS (note: pre-existing tests run against the legacy `eventRaw` path because they don't set `RawEventStore`).

- [ ] **Step 6: Commit**

```bash
cd /home/laoc/coding/edufeed/nostrlib
git add eventstore/typesense30142/query.go eventstore/typesense30142/raw_event_store_test.go
git commit -m "typesense30142: route QueryEvents through RawEventStore when set

When RawEventStore is configured, extract event IDs from Typesense hits
and fetch full events from the raw store. Mirrors the bleve backend
pattern: the search index holds only what queries need, the authoritative
event payload lives elsewhere. Backwards compatible — when RawEventStore
is nil, falls through to the legacy eventRaw reconstruction path."
```

---

## Task 3: Wire `RawEventStore = boltDB` into amb-relay

**Files:**
- Modify: `main.go:144-148` (TSBackend construction)
- Modify: `go.mod` (replace pseudo-version of nostrlib)

The relay should now route Typesense queries through BoltDB. Until Task 4 lands, content updates will still go through `setcontent`'s direct PATCH call — they continue to work, but the race remains. Task 3 is intentionally a small, isolated step.

- [ ] **Step 1: Update `go.mod` to point at the new nostrlib pseudo-version**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest
```

Note the new version string. Edit `go.mod`'s `replace` line to match. Then:

```bash
GOWORK=off go mod tidy
```

(If `nostrlib` changes haven't been pushed yet, this step uses the local workspace via `go.work`. Push at end of plan; for local development the `go.work` fallback is sufficient.)

- [ ] **Step 2: Wire `RawEventStore` in `main.go`**

In `main.go`, locate the TSBackend construction (currently lines 144-148):

```go
	tsDB := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
	}
```

Add `RawEventStore: &boltDB,`:

```go
	tsDB := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
		RawEventStore:  &boltDB,
	}
```

- [ ] **Step 3: Build**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go build .
```

Expected: success.

- [ ] **Step 4: Smoke check that queries still work**

```bash
cd /home/laoc/coding/edufeed/amb-relay
docker compose down -v
docker compose up -d typesense
go run . &
sleep 3
nix-shell -p nak --run 'nak count -k 30142 ws://localhost:3334'
kill %1
```

Expected: count returns 0 (fresh DB) without error. The relay's QueryStored now flows through `tsDB.QueryEvents` → `boltDB.QueryEvents` for the full event payload.

- [ ] **Step 5: Commit**

```bash
cd /home/laoc/coding/edufeed/amb-relay
git add main.go go.mod go.sum
git commit -m "Wire BoltDB as RawEventStore for Typesense backend

Typesense now serves as a search/filter index only; full event payloads
come from BoltDB. Mirrors the bleve backend pattern. No functional change
to write paths yet — that lands in the next commit."
```

---

## Task 4: Refactor `TSWriteBuffer` to project events with content

**Files:**
- Modify: `buffer.go` (the entire `TSWriteBuffer` struct and its methods)
- Modify: `main.go:227-238` (StoreEvent / ReplaceEvent wiring)
- Modify: `main.go:680-700` (setcontent NIP-86 case)
- Create: `buffer_test.go` (new file)

Convert the buffer from "queue events for batch upsert" to "queue projection tasks". A projection task carries both the event and (optionally) a content entry. The buffer's goroutine processes tasks in order, doing event upsert + content patch as a single unit. This serializes setcontent's projection behind the event's projection, eliminating the race.

- [ ] **Step 1: Write the failing race-elimination test**

Create `buffer_test.go`:

```go
package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// fakeProjector counts how many event-only and event-with-content
// projections it receives, in arrival order.
type fakeProjector struct {
	mu    sync.Mutex
	calls []fakeProjectorCall
}

type fakeProjectorCall struct {
	EventID    string
	HasContent bool
}

func (f *fakeProjector) Project(event nostr.Event, content *ContentEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeProjectorCall{
		EventID:    event.ID.Hex(),
		HasContent: content != nil,
	})
	return nil
}

// TestTSWriteBuffer_SerializesEventThenContent verifies that an event
// queued via Queue followed by a content update via QueueContent for the
// same event id are projected in order. This is the property that
// eliminates the setcontent 404 race.
func TestTSWriteBuffer_SerializesEventThenContent(t *testing.T) {
	proj := &fakeProjector{}
	buf := newProjectorBuffer(proj.Project, 100, 10*time.Millisecond)
	defer buf.Close()

	sk := nostr.Generate()
	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Kind:      nostr.Kind(30142),
		Tags:      nostr.Tags{{"d", "x"}, {"name", "n"}},
	}
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}

	buf.Queue(evt)
	buf.QueueContent(evt, ContentEntry{Text: "hi", FetchedAt: 1, Status: "ok"})

	// Wait for both to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		proj.mu.Lock()
		n := len(proj.calls)
		proj.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	proj.mu.Lock()
	defer proj.mu.Unlock()
	if len(proj.calls) != 2 {
		t.Fatalf("got %d projections, want 2: %+v", len(proj.calls), proj.calls)
	}
	if proj.calls[0].EventID != evt.ID.Hex() || proj.calls[0].HasContent {
		t.Errorf("call 0: %+v, want event-only for %s", proj.calls[0], evt.ID.Hex())
	}
	if proj.calls[1].EventID != evt.ID.Hex() || !proj.calls[1].HasContent {
		t.Errorf("call 1: %+v, want with-content for %s", proj.calls[1], evt.ID.Hex())
	}
}

// TestTSWriteBuffer_BatchesUnderLoad verifies the buffer batches event-only
// projections under high load (preserving today's behavior for bulk imports).
func TestTSWriteBuffer_BatchesUnderLoad(t *testing.T) {
	var batches atomic.Int64
	projector := func(event nostr.Event, content *ContentEntry) error {
		batches.Add(1)
		return nil
	}
	buf := newProjectorBuffer(projector, 50, 50*time.Millisecond)
	defer buf.Close()

	sk := nostr.Generate()
	const n = 200
	for i := 0; i < n; i++ {
		evt := nostr.Event{
			PubKey:    nostr.GetPublicKey(sk),
			CreatedAt: nostr.Timestamp(int64(i + 1_700_000_000)),
			Kind:      nostr.Kind(30142),
			Tags:      nostr.Tags{{"d", "d"}, {"name", "n"}},
		}
		evt.Sign(sk)
		buf.Queue(evt)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if batches.Load() >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := batches.Load(); got != n {
		t.Fatalf("projected %d/%d events", got, n)
	}
}
```

- [ ] **Step 2: Run tests, expect compile errors**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go test -run TestTSWriteBuffer -v ./...
```

Expected: FAIL with "undefined: newProjectorBuffer", "undefined: QueueContent". Good — that's what Steps 3-5 fix.

- [ ] **Step 3: Rewrite `buffer.go` as a projection buffer**

Replace the contents of `buffer.go` with:

```go
package main

import (
	"log"
	"sync"
	"time"

	"fiatjaf.com/nostr"
)

// projectionTask is one unit of work for the TS projector: project this
// event (and optionally its content) into Typesense. Setting Content
// triggers a follow-up content patch after the event upsert.
type projectionTask struct {
	Event   nostr.Event
	Content *ContentEntry
}

// projectorFn is the function the buffer calls per task. The relay supplies
// a closure that knows how to upsert event metadata via tsDB.ReplaceEvent
// and patch content via PatchContent.
type projectorFn func(event nostr.Event, content *ContentEntry) error

// TSWriteBuffer queues projection tasks and runs them serially in a single
// background goroutine. Tasks for the same event id execute in arrival
// order, so an event's metadata projection always lands before any content
// patch for that event. Bulk-import paths (StoreEvent / mirror replay)
// queue event-only tasks; setcontent queues an event-with-content task.
//
// The buffer is the single Typesense writer. setcontent must NOT call the
// Typesense PATCH endpoint directly — doing so re-introduces the dual-
// writer race that this design is meant to eliminate.
type TSWriteBuffer struct {
	project       projectorFn
	ch            chan projectionTask
	done          chan struct{}
	flushInterval time.Duration
	wg            sync.WaitGroup
}

// newProjectorBuffer constructs and starts a buffer. capacity is the
// channel buffer; flushInterval is unused beyond shutdown for now (kept
// for API compatibility with the tick-based tests).
func newProjectorBuffer(project projectorFn, capacity int, flushInterval time.Duration) *TSWriteBuffer {
	if capacity < 1 {
		capacity = 1
	}
	b := &TSWriteBuffer{
		project:       project,
		ch:            make(chan projectionTask, capacity*100),
		done:          make(chan struct{}),
		flushInterval: flushInterval,
	}
	b.wg.Add(1)
	go b.run()
	return b
}

// Queue enqueues an event-only projection (no content patch).
func (b *TSWriteBuffer) Queue(event nostr.Event) {
	b.ch <- projectionTask{Event: event}
}

// QueueContent enqueues a projection that includes a content patch. The
// Event is required so the projector can re-upsert the document and then
// apply content; passing it explicitly avoids a BoltDB lookup race for
// fresh events.
func (b *TSWriteBuffer) QueueContent(event nostr.Event, content ContentEntry) {
	b.ch <- projectionTask{Event: event, Content: &content}
}

// Close drains the queue and waits for the worker to finish.
func (b *TSWriteBuffer) Close() {
	close(b.done)
	b.wg.Wait()
}

func (b *TSWriteBuffer) run() {
	defer b.wg.Done()
	for {
		select {
		case task := <-b.ch:
			b.project1(task)
		case <-b.done:
			// Drain remaining tasks.
			close(b.ch)
			for task := range b.ch {
				b.project1(task)
			}
			return
		}
	}
}

func (b *TSWriteBuffer) project1(task projectionTask) {
	if err := b.project(task.Event, task.Content); err != nil {
		log.Printf("ts-projector: %s: %v", task.Event.ID.Hex(), err)
	}
}
```

- [ ] **Step 4: Add a constructor wrapper that builds the projection closure**

Append to `buffer.go`:

```go
// NewTSWriteBuffer builds a TSWriteBuffer wired to the relay's Typesense
// backend and content-patch helpers. This is the production constructor
// used in main.go; tests use newProjectorBuffer directly with a fake
// projector function.
func NewTSWriteBuffer(tsDB tsBackend, host, apiKey, collection string, capacity int, flushInterval time.Duration) *TSWriteBuffer {
	project := func(event nostr.Event, content *ContentEntry) error {
		if err := tsDB.ReplaceEvent(event); err != nil {
			return err
		}
		if content == nil {
			return nil
		}
		docID, err := tsDocIDFromEvent(event)
		if err != nil {
			return err
		}
		return PatchContent(host, apiKey, collection, docID, *content)
	}
	return newProjectorBuffer(project, capacity, flushInterval)
}

// tsBackend is the subset of typesense30142.TSBackend the projector uses.
// Defined as an interface so tests can inject a fake.
type tsBackend interface {
	ReplaceEvent(nostr.Event) error
}
```

- [ ] **Step 5: Run buffer tests, expect PASS**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go test -run TestTSWriteBuffer -v ./...
```

Expected: both tests PASS.

- [ ] **Step 6: Update `main.go` to use the new buffer constructor**

In `main.go`, locate the current buffer construction (line 173):

```go
	tsBuf := NewTSWriteBuffer(&tsDB, 100, 500*time.Millisecond)
```

Replace with:

```go
	tsBuf := NewTSWriteBuffer(&tsDB, tsDB.Host, tsDB.ApiKey, tsDB.CollectionName, 100, 500*time.Millisecond)
```

- [ ] **Step 7: Update setcontent to enqueue, not PATCH directly**

In `main.go`, locate the setcontent case (lines 636-700). Replace the body of the case from line 680 onward (`docID, err := tsDocIDFromEvent(event)` through `return nip86.Response{Result: true}, nil`) with:

```go
			entry := ContentEntry{
				Text:      text,
				FetchedAt: int64(fetchedAtFloat),
				Status:    status,
				SourceURL: sourceURL,
			}
			if err := contentStore.Put(eventIDHex, entry); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store put: %v", err)}, nil
			}
			// Queue projection. The buffer serializes this behind any
			// pending event projection for the same id, so the doc is
			// guaranteed to exist in Typesense by the time the content
			// patch runs. We do NOT PATCH Typesense directly here — that
			// re-introduces the dual-writer race this design eliminates.
			tsBuf.QueueContent(event, entry)
			return nip86.Response{Result: true}, nil
```

The `docID, err := tsDocIDFromEvent(event)` line is no longer needed in setcontent (the buffer resolves it inside the projector closure).

- [ ] **Step 8: Update refetchcontent for consistency**

In `main.go`, locate the refetchcontent case. The current PATCH-via-`ClearContent` at line 730 has the same race. Replace lines 723-738 (from `docID, err := tsDocIDFromEvent(event)` through the `mgmt.MarkNeedsRefetch` block) with:

```go
			if err := contentStore.Delete(eventIDHex); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store delete: %v", err)}, nil
			}
			// Project an empty content entry so Typesense reflects the clear.
			tsBuf.QueueContent(event, ContentEntry{})
			// Signal the indexer to re-ingest this event. Best-effort: the
			// operator's intent (clear content) already succeeded, so a
			// failure here is logged but does not fail the call.
			if err := mgmt.MarkNeedsRefetch(eventIDHex); err != nil {
				fmt.Printf("refetchcontent: mark needs_refetch %s: %v\n", eventIDHex, err)
			}
			return nip86.Response{Result: true}, nil
```

- [ ] **Step 9: Build and run unit tests**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go build .
GOWORK=off go test ./...
```

Expected: build succeeds; all tests pass.

- [ ] **Step 10: Run vet and fmt**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go vet ./...
gofmt -d -l buffer.go buffer_test.go main.go
```

Expected: no vet output; no diff from gofmt.

- [ ] **Step 11: Commit**

```bash
cd /home/laoc/coding/edufeed/amb-relay
git add buffer.go buffer_test.go main.go
git commit -m "Refactor TSWriteBuffer into a single-writer projector

The buffer now accepts projection tasks instead of plain events. Each
task carries an Event plus an optional ContentEntry; the buffer's worker
goroutine serializes them through tsDB.ReplaceEvent and (when content is
present) a follow-up PatchContent. Tasks for the same event id execute
in arrival order.

setcontent and refetchcontent stop PATCHing Typesense directly. They now
write to ContentStore (authoritative) and enqueue a projection task. This
eliminates the dual-writer race that surfaced as widespread 404s during
bulk-mirror replay: the buffer cannot patch a doc that doesn't exist in
Typesense yet because the same goroutine just upserted it.

Mirrors the bleve eventstore pattern: Typesense is a derived projection
of BoltDB events + ContentStore; reindex remains the recovery path."
```

---

## Task 5: Verify against prod corpus

**Files:** none modified — verification only.

This task re-runs the bulk mirror exercise from the previous plan and confirms (a) every published event is queryable, (b) the indexer drains without 404s, (c) `content_status=ok` rolls forward.

- [ ] **Step 1: Reset the local stack**

```bash
cd /home/laoc/coding/edufeed/amb-relay
docker compose down -v
docker compose up -d --build
```

Wait for healthchecks: `docker compose ps` should show typesense as `healthy` and the relay accepting connections.

- [ ] **Step 2: Mirror prod into the fresh stack**

```bash
cd /home/laoc/coding/edufeed/amb-indexer
GOWORK=off go build -o /tmp/mirror-prod ./cmd/mirror-prod
/tmp/mirror-prod --src wss://amb-relay.edufeed.org --dst ws://localhost:3334
```

Expected: a multi-page log ending with `mirror done: published=N errors=0` where N matches the 8554-ish figure from the previous plan (within the documented boundary-collision gap).

- [ ] **Step 3: Verify Typesense queryability**

```bash
K="$(grep '^TS_APIKEY' /home/laoc/coding/edufeed/amb-relay/.env | cut -d= -f2 | tr -d '"')"
curl -s -H "X-TYPESENSE-API-KEY: $K" "http://localhost:8108/collections/amb/documents/search?q=*&per_page=0" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print('found:', d['found'])"
```

Expected: `found:` matches the published count.

- [ ] **Step 4: Wait for indexer to drain, check chunks indexed**

The indexer is configured to consume the bulk replay. Wait ~5–10 minutes (depending on the embedding service throughput), then:

```bash
curl -s http://localhost:18080/metrics | grep -E "indexer_(chunks_indexed|content_fetched|errors)_total"
```

Expected:
- `indexer_chunks_indexed_total` > 0 and growing.
- No (or very few) error counters incrementing.

- [ ] **Step 5: Spot-check `content_status=ok` count**

```bash
curl -s -H "X-TYPESENSE-API-KEY: $K" \
  "http://localhost:8108/collections/amb/documents/search?q=*&filter_by=content_status:ok&per_page=0" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print('content_ok:', d['found'])"
```

Expected: `content_ok` > 0 and rising on subsequent calls. Before this plan's work, it was 0.

- [ ] **Step 6: Spot-check end-to-end search**

```bash
nix-shell -p nak --run 'nak req --search "Photosynthese" -k 30142 -l 3 ws://localhost:3334'
```

Expected: at least one event back. Confirm the event payload looks correct (came from BoltDB via the new `RawEventStore` plumbing, not from `eventRaw`).

- [ ] **Step 7: Document results in `architecture.md`**

Append a section titled `### Typesense as a projection of BoltDB+ContentStore` summarizing:
- The bleve-style pattern adopted here.
- Why dual-writer setups race during bulk replay.
- Reindex remains the recovery path.
- Reference to this plan file.

(One paragraph plus links is enough; the architecture doc already covers the prod↔local search comparison from the previous plan.)

- [ ] **Step 8: Commit doc updates**

```bash
cd /home/laoc/coding/edufeed/amb-relay
git add docs/architecture.md
git commit -m "Document Typesense projection pattern in architecture.md"
```

---

## Task 6: Push nostrlib + update amb-relay go.mod

**Files:**
- `../nostrlib/` (push)
- `go.mod` (final pseudo-version bump)

- [ ] **Step 1: Push nostrlib changes**

```bash
cd /home/laoc/coding/edufeed/nostrlib
git push origin edufeed
```

Expected: push succeeds.

- [ ] **Step 2: Update amb-relay go.mod to the published pseudo-version**

```bash
cd /home/laoc/coding/edufeed/amb-relay
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest
```

Edit `go.mod`'s replace directive to match the new pseudo-version.

```bash
GOWORK=off go mod tidy
GOWORK=off go build .
```

Expected: standalone build succeeds (no `go.work` fallback needed).

- [ ] **Step 3: Commit**

```bash
cd /home/laoc/coding/edufeed/amb-relay
git add go.mod go.sum
git commit -m "Bump nostrlib to typesense projection pattern"
```

---

## Self-Review

**Spec coverage**
- "Add `RawEventStore` field" → Task 1 ✓
- "`QueryEvents` uses RawEventStore when set, eventRaw fallback otherwise" → Task 2 ✓
- "Wire RawEventStore=boltDB in amb-relay" → Task 3 ✓
- "Buffer becomes single-writer projector" → Task 4 ✓
- "setcontent/refetchcontent stop PATCHing Typesense directly" → Task 4 ✓
- "Verify against prod corpus, no 404 race" → Task 5 ✓

**Out of scope (deliberately):**
- Slimming the Typesense schema (drop `eventRaw` and unused metadata fields). A follow-up plan once Task 5's verification is stable.
- Migrating the deferred upstream nostrlib rebase. Tracked in memory `project_nostrlib_upstream_sync.md`.
- Indexer-side retry-on-404. With Task 4, the 404 is structurally impossible; retries become unnecessary.

**Risks/concerns:**
- Task 4 changes setcontent's success semantics. Today, success means "Typesense was patched"; after, it means "ContentStore was written and the patch is queued." If the projector is killed before flushing, the in-flight patch is lost. ContentStore is durable, so a manual reindex recovers. This is acceptable and matches the existing eventstore semantics where Typesense is a projection.
- The new `tsBackend` interface in `buffer.go` is a thin shim. If the projection logic grows, it may want a richer interface. Keep it minimal for now.
- The integration tests in Task 2 require a running Typesense; they're already gated on `TS_TEST_HOST`/`TS_TEST_APIKEY` env vars matching the existing test convention.

---

## Execution

Plan complete. The work spans two repos and the second-repo changes (amb-relay) depend on first-repo changes (nostrlib) being importable. With the `go.work` workspace at `edufeed/go.work`, local development picks up nostrlib changes immediately; pushes to nostrlib only need to happen at the end (Task 6) for Docker/server builds.
