# Author Profile Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Index the kind-0 profiles of authors who publish content to this relay, served via NIP-50 `search` over `kinds:[0]`, so clients can resolve org/person names to pubkeys.

**Architecture:** A new `profilesDB` Typesense collection holds one document per author pubkey, populated by a background `ProfileManager` that fetches kind-0 events from configurable source relays. Authors are enqueued on every content write (and via a startup backfill scan of BoltDB) into a durable BoltDB queue; a drain loop fetches their kind-0 and upserts a projected document. kind-0 is registered in the content-type registry **read-only** (client writes rejected, reads served) — the manager writes to `profilesDB` directly, bypassing the relay event path.

**Tech Stack:** Go, khatru relay framework, Typesense (`typesense30142.TSBackend`), BoltDB (`bbolt` via `relaykit.Store`), `nostr.Pool` for outbound fetch, `sdk.ParseMetadata` for kind-0 parsing.

## Global Constraints

- Everything is gated behind `PROFILES_ENABLED` (default `false`); when off, no manager, no kind-0 registration, no queue bucket use, zero behavior change.
- The relay NEVER accepts client kind-0 writes — registry `validate` for kind 0 returns `true, "kind not accepted"`. The index is populated only by the internal `ProfileManager`.
- `profilesDB.RawEventStore` is **nil**: profiles are not relay events in BoltDB; `TSBackend.QueryEvents` reconstructs them from the stored `eventRaw` field. (Long-form/wiki/calendar set `RawEventStore: &boltDB`; profiles must NOT — there is no BoltDB row to read back.)
- Reuse existing helpers; do not reinvent: `sdk.ParseMetadata` (kind-0 parse), `structured.go` (`structuredEnvelope`, `structuredEnvelopeFields`, `storeStructured`), the `AllowlistManager` lifecycle shape (`Init`/`StartRefreshLoop`/`Stop`), and the `bucketNeedsRefetch` BoltDB-bucket method pattern in `management.go`.
- New env vars and their defaults: `TS_COLLECTION_PROFILES`=`profiles_0`, `PROFILE_RELAYS`=`wss://relay.edufeed.org`, `PROFILE_REFRESH_INTERVAL`=`6h`.
- Document id in `profilesDB` is the author pubkey hex (one doc per author; newer kind-0 upserts over older — newest profile wins).
- Tests are Go (`go test ./...` from the repo root) and must not require a live Typesense or live relays — use fakes/interfaces and temp BoltDB.

---

### Task 1: Profile document, projection, and schema

**Files:**
- Create: `/home/laoc/coding/edufeed/amb-relay/profiles.go`
- Test: `/home/laoc/coding/edufeed/amb-relay/profiles_test.go`

**Interfaces:**
- Consumes (existing): `structuredEnvelope`, `newStructuredEnvelope(*nostr.Event) (structuredEnvelope, error)`, `structuredEnvelopeFields() []typesense30142.Field`, `storeStructured[T any](enabled bool, ts *typesense30142.TSBackend, event nostr.Event, label string, project func(*nostr.Event) (*T, error))` — all in `structured.go`. Also `sdk.ParseMetadata(event nostr.Event) (sdk.ProfileMetadata, error)` from `fiatjaf.com/nostr/sdk`.
- Produces: `nostrToProfile(event *nostr.Event) (*ProfileDocument, error)`, `storeProfile(enabled bool, ts *typesense30142.TSBackend, event nostr.Event)`, `profileSchema(name string) typesense30142.CollectionSchema`, and the `ProfileDocument` type.

- [ ] **Step 1: Write the failing test**

Create `/home/laoc/coding/edufeed/amb-relay/profiles_test.go`:

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func mkKind0(t *testing.T, content string) nostr.Event {
	t.Helper()
	sk := nostr.Generate()
	ev := nostr.Event{Kind: 0, Content: content, CreatedAt: 1700000000}
	ev.Sign(sk)
	return ev
}

func TestNostrToProfileMapsFields(t *testing.T) {
	ev := mkKind0(t, `{"name":"e-teaching","display_name":"e-teaching.org","about":"Portal","nip05":"info@e-teaching.org"}`)
	doc, err := nostrToProfile(&ev)
	if err != nil {
		t.Fatalf("nostrToProfile: %v", err)
	}
	if doc.ID != ev.PubKey.Hex() {
		t.Errorf("ID = %q, want pubkey hex %q", doc.ID, ev.PubKey.Hex())
	}
	if doc.Name != "e-teaching" || doc.DisplayName != "e-teaching.org" || doc.About != "Portal" || doc.NIP05 != "info@e-teaching.org" {
		t.Errorf("fields not mapped: %+v", doc)
	}
	if doc.EventKind != 0 || doc.EventPubKey != ev.PubKey.Hex() || doc.EventRaw == "" {
		t.Errorf("envelope not filled: %+v", doc.structuredEnvelope)
	}
}

func TestNostrToProfileMissingOptionalFields(t *testing.T) {
	ev := mkKind0(t, `{"name":"solo"}`)
	doc, err := nostrToProfile(&ev)
	if err != nil {
		t.Fatalf("nostrToProfile: %v", err)
	}
	if doc.Name != "solo" || doc.DisplayName != "" || doc.About != "" || doc.NIP05 != "" {
		t.Errorf("unexpected fields: %+v", doc)
	}
}

func TestNostrToProfileRejectsNonKind0(t *testing.T) {
	sk := nostr.Generate()
	ev := nostr.Event{Kind: 1, Content: "{}", CreatedAt: 1700000000}
	ev.Sign(sk)
	if _, err := nostrToProfile(&ev); err == nil {
		t.Fatal("expected error for non-kind-0 event, got nil")
	}
}

func TestProfileSchemaHasSearchableAndEnvelopeFields(t *testing.T) {
	s := profileSchema("profiles_0")
	if s.Name != "profiles_0" || s.DefaultSortingField != "eventCreatedAt" {
		t.Fatalf("schema header wrong: %+v", s)
	}
	want := map[string]bool{"id": false, "name": false, "display_name": false, "about": false, "nip05": false, "eventRaw": false}
	for _, f := range s.Fields {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("schema missing field %q", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestNostrToProfile -v`
Expected: FAIL — `undefined: nostrToProfile` / `undefined: profileSchema`.

- [ ] **Step 3: Write minimal implementation**

Create `/home/laoc/coding/edufeed/amb-relay/profiles.go`:

```go
package main

import (
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/sdk"
)

// ProfileDocument is the Typesense document shape for a kind-0 profile stored
// in the profiles collection. One document per author pubkey (id = pubkey hex),
// so a newer kind-0 upserts over the older one. The structured envelope carries
// the full kind-0 event (eventRaw) so the query path can return the real signed
// event; profilesDB has no RawEventStore, so reconstruction is from eventRaw.
type ProfileDocument struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	About       string `json:"about,omitempty"`
	NIP05       string `json:"nip05,omitempty"`
	structuredEnvelope
}

// nostrToProfile projects a kind-0 event into a ProfileDocument using the pure
// sdk.ParseMetadata parser (errors on non-kind-0 or malformed JSON content).
func nostrToProfile(event *nostr.Event) (*ProfileDocument, error) {
	meta, err := sdk.ParseMetadata(*event)
	if err != nil {
		return nil, fmt.Errorf("parse kind-0 metadata: %w", err)
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	return &ProfileDocument{
		ID:                 event.PubKey.Hex(),
		Name:               meta.Name,
		DisplayName:        meta.DisplayName,
		About:              meta.About,
		NIP05:              meta.NIP05,
		structuredEnvelope: env,
	}, nil
}

// storeProfile projects and upserts a kind-0 event to the profiles collection.
func storeProfile(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "profile", nostrToProfile)
}

// profileSchema returns the Typesense collection schema for kind-0 profiles.
// Mirrors wikiSchema's envelope naming so the shared query path reconstructs
// events identically.
func profileSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "name", Type: "string", Optional: true},
			{Name: "display_name", Type: "string", Optional: true},
			{Name: "about", Type: "string", Optional: true},
			{Name: "nip05", Type: "string", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestNostrToProfile|TestProfileSchema' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add profiles.go profiles_test.go
git commit -m "feat(profiles): kind-0 projection, document, and Typesense schema"
```

---

### Task 2: Durable candidate queue on ManagementStore

**Files:**
- Modify: `/home/laoc/coding/edufeed/amb-relay/management.go` (var block ~`/home/laoc/coding/edufeed/amb-relay/management.go:14-23`, the `Init` bucket list ~`management.go:52-58`, and append new methods near the `NeedsRefetch` methods ~`management.go:270-318`)
- Test: `/home/laoc/coding/edufeed/amb-relay/profile_queue_test.go`

**Interfaces:**
- Consumes (existing): `ManagementStore` with embedded `*relaykit.Store` exposing `m.DB *bbolt.DB`; `ManagementStore.Init(db *bbolt.DB) error`.
- Produces: `(*ManagementStore).EnqueueProfileCandidate(pubkey string) error`, `(*ManagementStore).RemoveProfileCandidate(pubkey string) error`, `(*ManagementStore).ListProfileQueue() ([]string, error)`. These three methods form the `profileQueue` interface that Task 3 consumes.

- [ ] **Step 1: Write the failing test**

Create `/home/laoc/coding/edufeed/amb-relay/profile_queue_test.go`:

```go
package main

import (
	"os"
	"sort"
	"testing"

	"go.etcd.io/bbolt"
)

func newTestMgmt(t *testing.T) *ManagementStore {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "mgmt-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := bbolt.Open(f.Name(), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var m ManagementStore
	if err := m.Init(db); err != nil {
		t.Fatalf("mgmt init: %v", err)
	}
	return &m
}

func TestProfileQueueEnqueueDedupRemove(t *testing.T) {
	m := newTestMgmt(t)
	if err := m.EnqueueProfileCandidate("aa"); err != nil {
		t.Fatal(err)
	}
	if err := m.EnqueueProfileCandidate("aa"); err != nil { // dedup: idempotent Put
		t.Fatal(err)
	}
	if err := m.EnqueueProfileCandidate("bb"); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListProfileQueue()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "aa" || got[1] != "bb" {
		t.Fatalf("queue = %v, want [aa bb]", got)
	}
	if err := m.RemoveProfileCandidate("aa"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.ListProfileQueue()
	if len(got) != 1 || got[0] != "bb" {
		t.Fatalf("after remove, queue = %v, want [bb]", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestProfileQueue -v`
Expected: FAIL — `m.EnqueueProfileCandidate undefined` (and the bucket would not exist anyway).

- [ ] **Step 3: Write minimal implementation**

In `/home/laoc/coding/edufeed/amb-relay/management.go`, add the bucket name to the `var (...)` block (alongside `bucketNeedsRefetch`):

```go
	bucketProfileQueue    = []byte("profile_queue")    // author pubkeys awaiting kind-0 fetch
```

In `ManagementStore.Init`, add `bucketProfileQueue` to the slice passed to the bucket-creation loop:

```go
		for _, bucket := range [][]byte{bucketTypesenseSchema, bucketSemanticConfig, bucketAccessControl, bucketWriteAllowlist, bucketReadAllowlist, bucketListReferences, bucketFetchedContent, bucketNeedsRefetch, bucketProfileQueue} {
```

Append these methods (mirroring `MarkNeedsRefetch`/`RemoveNeedsRefetch`/`ListNeedsRefetch`):

```go
// EnqueueProfileCandidate queues an author pubkey (hex) for kind-0 fetch.
// Idempotent: queuing the same pubkey twice is a no-op (dedup).
func (m *ManagementStore) EnqueueProfileCandidate(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).Put([]byte(pubkey), []byte{})
	})
}

// RemoveProfileCandidate clears a pubkey from the profile fetch queue.
// Idempotent: removing a non-existent pubkey is a no-op.
func (m *ManagementStore) RemoveProfileCandidate(pubkey string) error {
	return m.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).Delete([]byte(pubkey))
	})
}

// ListProfileQueue returns all author pubkeys currently queued for kind-0 fetch.
func (m *ManagementStore) ListProfileQueue() ([]string, error) {
	var result []string
	err := m.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketProfileQueue).ForEach(func(k, _ []byte) error {
			result = append(result, string(k))
			return nil
		})
	})
	return result, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestProfileQueue -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add management.go profile_queue_test.go
git commit -m "feat(profiles): durable BoltDB candidate queue on ManagementStore"
```

---

### Task 3: ProfileManager — enqueue, drain, backfill, refresh

**Files:**
- Create: `/home/laoc/coding/edufeed/amb-relay/profile_manager.go`
- Test: `/home/laoc/coding/edufeed/amb-relay/profile_manager_test.go`

**Interfaces:**
- Consumes: the `profileQueue` methods from Task 2 (`EnqueueProfileCandidate`/`RemoveProfileCandidate`/`ListProfileQueue`); `sdk.ParseMetadata` (Task 1 already imports the package); `nostr.Pool.FetchManyReplaceable(ctx, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) *xsync.MapOf[nostr.ReplaceableKey, nostr.Event]`; `nostr.PubKeyFromHex(string) (nostr.PubKey, error)`.
- Produces:
  - `type profileQueue interface { EnqueueProfileCandidate(string) error; RemoveProfileCandidate(string) error; ListProfileQueue() ([]string, error) }`
  - `type profileSource interface { Fetch(ctx context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event }`
  - `type eventQuerier interface { QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] }`
  - `func backfillAuthors(q eventQuerier, kinds []nostr.Kind, maxLimit int) []nostr.PubKey`
  - `type poolSource struct { pool *nostr.Pool }` implementing `profileSource`
  - `type ProfileManager` with `NewProfileManager(q profileQueue, src profileSource, store func(nostr.Event), backfill func() []nostr.PubKey, relays []string, batchSize int) *ProfileManager`, methods `Enqueue(pk nostr.PubKey)`, `drainOnce(ctx context.Context)`, `Init()`, `StartRefreshLoop(interval time.Duration)`, `Stop()`.

- [ ] **Step 1: Write the failing test**

Create `/home/laoc/coding/edufeed/amb-relay/profile_manager_test.go`:

```go
package main

import (
	"context"
	"iter"
	"sort"
	"testing"

	"fiatjaf.com/nostr"
)

// --- fakes ---

type fakeQueue struct{ items map[string]bool }

func newFakeQueue() *fakeQueue { return &fakeQueue{items: map[string]bool{}} }
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
	mgr := NewProfileManager(q, src, func(e nostr.Event) { stored = append(stored, e) }, func() []nostr.PubKey { return nil }, []string{"wss://x"}, 50)

	mgr.drainOnce(context.Background())

	if len(stored) != 1 || stored[0].PubKey != pkA {
		t.Fatalf("stored = %v, want [A]", stored)
	}
	left, _ := q.ListProfileQueue()
	if len(left) != 1 || left[0] != pkB.Hex() {
		t.Fatalf("queue = %v, want [B] (A drained, B retried later)", left)
	}
}

func TestDrainDropsInvalidPubkey(t *testing.T) {
	q := newFakeQueue()
	q.EnqueueProfileCandidate("not-a-valid-hex-pubkey")

	var stored []nostr.Event
	src := fakeSource{byPubkey: map[nostr.PubKey]nostr.Event{}}
	mgr := NewProfileManager(q, src, func(e nostr.Event) { stored = append(stored, e) }, func() []nostr.PubKey { return nil }, nil, 50)

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
	mgr := NewProfileManager(q, fakeSource{}, func(nostr.Event) {}, func() []nostr.PubKey { return nil }, nil, 50)
	pk := nostr.Generate().Public()
	mgr.Enqueue(pk)
	mgr.Enqueue(pk) // dedup
	if left, _ := q.ListProfileQueue(); len(left) != 1 || left[0] != pk.Hex() {
		t.Fatalf("queue = %v, want one entry", left)
	}
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestDrain|TestEnqueueDelegates|TestBackfillAuthors' -v`
Expected: FAIL — `undefined: NewProfileManager` / `undefined: backfillAuthors`.

- [ ] **Step 3: Write minimal implementation**

Create `/home/laoc/coding/edufeed/amb-relay/profile_manager.go`:

```go
package main

import (
	"context"
	"fmt"
	"iter"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/sdk"
)

// profileDrainTimeout bounds a single drain's network fetch, matching the
// AllowlistManager refresh timeout.
const profileDrainTimeout = 30 * time.Second

// profileQueue is the durable candidate queue ProfileManager drains.
// *ManagementStore satisfies it.
type profileQueue interface {
	EnqueueProfileCandidate(pubkey string) error
	RemoveProfileCandidate(pubkey string) error
	ListProfileQueue() ([]string, error)
}

// profileSource fetches the latest kind-0 event for each requested pubkey.
type profileSource interface {
	Fetch(ctx context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event
}

// eventQuerier is the read surface ProfileManager needs for startup backfill.
// *boltdb.BoltBackend satisfies it.
type eventQuerier interface {
	QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]
}

// poolSource fetches kind-0 events from remote relays via a nostr.Pool.
// FetchManyReplaceable dedups to the latest kind-0 per pubkey (d is empty).
type poolSource struct {
	pool *nostr.Pool
}

func (s poolSource) Fetch(ctx context.Context, relays []string, pubkeys []nostr.PubKey) []nostr.Event {
	if len(pubkeys) == 0 {
		return nil
	}
	m := s.pool.FetchManyReplaceable(ctx, relays, nostr.Filter{
		Kinds:   []nostr.Kind{0},
		Authors: pubkeys,
	}, nostr.SubscriptionOptions{})
	var out []nostr.Event
	m.Range(func(_ nostr.ReplaceableKey, ev nostr.Event) bool {
		out = append(out, ev)
		return true
	})
	return out
}

// backfillAuthors enumerates the distinct authors of stored content events,
// preserving first-seen order.
func backfillAuthors(q eventQuerier, kinds []nostr.Kind, maxLimit int) []nostr.PubKey {
	seen := make(map[nostr.PubKey]bool)
	var out []nostr.PubKey
	for ev := range q.QueryEvents(nostr.Filter{Kinds: kinds}, maxLimit) {
		if !seen[ev.PubKey] {
			seen[ev.PubKey] = true
			out = append(out, ev.PubKey)
		}
	}
	return out
}

// ProfileManager owns the kind-0 fetch loop: enqueue candidates, drain them by
// fetching kind-0 from PROFILE_RELAYS, and periodically re-enqueue known
// authors so renamed profiles stay fresh. Modeled on AllowlistManager.
type ProfileManager struct {
	queue     profileQueue
	src       profileSource
	store     func(nostr.Event)
	backfill  func() []nostr.PubKey
	relays    []string
	batchSize int
	stopCh    chan struct{}
}

func NewProfileManager(q profileQueue, src profileSource, store func(nostr.Event), backfill func() []nostr.PubKey, relays []string, batchSize int) *ProfileManager {
	if batchSize <= 0 {
		batchSize = 50
	}
	return &ProfileManager{
		queue:     q,
		src:       src,
		store:     store,
		backfill:  backfill,
		relays:    relays,
		batchSize: batchSize,
		stopCh:    make(chan struct{}),
	}
}

// Enqueue queues an author pubkey for kind-0 fetch. Fire-and-forget: a queue
// error is logged but never propagated to the content write path.
func (p *ProfileManager) Enqueue(pk nostr.PubKey) {
	if err := p.queue.EnqueueProfileCandidate(pk.Hex()); err != nil {
		fmt.Printf("profile: enqueue %s: %v\n", pk.Hex(), err)
	}
}

// drainOnce fetches kind-0 for every queued pubkey in batches. A pubkey whose
// kind-0 is fetched+parsed is stored and removed; one with no returned event
// stays queued for the next drain. Invalid hex is dropped.
func (p *ProfileManager) drainOnce(ctx context.Context) {
	queued, err := p.queue.ListProfileQueue()
	if err != nil {
		fmt.Printf("profile: list queue: %v\n", err)
		return
	}
	for start := 0; start < len(queued); start += p.batchSize {
		end := start + p.batchSize
		if end > len(queued) {
			end = len(queued)
		}
		var authors []nostr.PubKey
		for _, hexpk := range queued[start:end] {
			pk, err := nostr.PubKeyFromHex(hexpk)
			if err != nil {
				_ = p.queue.RemoveProfileCandidate(hexpk) // drop garbage
				continue
			}
			authors = append(authors, pk)
		}
		if len(authors) == 0 {
			continue
		}
		for _, ev := range p.src.Fetch(ctx, p.relays, authors) {
			if _, err := sdk.ParseMetadata(ev); err != nil {
				continue // leave queued; don't store a malformed profile
			}
			p.store(ev)
			_ = p.queue.RemoveProfileCandidate(ev.PubKey.Hex())
		}
	}
}

// Init seeds the queue from a backfill scan, then drains in the background so
// relay startup is never blocked on the network fetch.
func (p *ProfileManager) Init() error {
	for _, pk := range p.backfill() {
		p.Enqueue(pk)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), profileDrainTimeout)
		defer cancel()
		p.drainOnce(ctx)
	}()
	return nil
}

// StartRefreshLoop periodically re-enqueues known content authors and drains,
// catching renamed/updated profiles.
func (p *ProfileManager) StartRefreshLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, pk := range p.backfill() {
					p.Enqueue(pk)
				}
				ctx, cancel := context.WithTimeout(context.Background(), profileDrainTimeout)
				p.drainOnce(ctx)
				cancel()
			case <-p.stopCh:
				return
			}
		}
	}()
}

func (p *ProfileManager) Stop() { close(p.stopCh) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestDrain|TestEnqueueDelegates|TestBackfillAuthors' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add profile_manager.go profile_manager_test.go
git commit -m "feat(profiles): ProfileManager fetch/drain/backfill/refresh loop"
```

---

### Task 4: Wire into main.go, registry kind-0 split, config, and docs

**Files:**
- Modify: `/home/laoc/coding/edufeed/amb-relay/main.go` (flag parse ~`main.go:63-65`; backend init after calendar ~`main.go:295`; registry assembly ~`main.go:332-385`; `StoreEvent`/`ReplaceEvent` ~`main.go:521-530`; manager lifecycle near `acl` wiring ~`main.go:155-160`)
- Modify: `/home/laoc/coding/edufeed/amb-relay/.env.example`
- Modify: `/home/laoc/coding/edufeed/amb-relay/CLAUDE.md`
- Test: `/home/laoc/coding/edufeed/amb-relay/content_registry_test.go` (create if absent; otherwise append)

**Interfaces:**
- Consumes: `profileSchema`, `storeProfile` (Task 1); `ManagementStore` queue methods (Task 2); `NewProfileManager`, `poolSource`, `backfillAuthors`, `ProfileManager` (Task 3); existing `newRegistry(types ...contentType) *registry`, the `contentType` struct (fields `kinds []nostr.Kind`, `validate func(nostr.Event) (bool, string)`, `store func(nostr.Event)`, `fetch func(nostr.Filter, int) iter.Seq[nostr.Event]`, `count func(nostr.Filter) (uint32, error)`, `deleteID func(nostr.ID) error`, `chunked bool`), and `registry` methods `validate`/`fetch`/`targetsChunked`.
- Produces: a running profiles subsystem behind `PROFILES_ENABLED`.

- [ ] **Step 1: Write the failing test (registry kind-0 read/write split)**

Create `/home/laoc/coding/edufeed/amb-relay/content_registry_test.go` (if a file with this name already exists, append the function and reuse its imports):

```go
package main

import (
	"iter"
	"testing"

	"fiatjaf.com/nostr"
)

func TestRegistryKind0ReadWriteSplit(t *testing.T) {
	var fetched bool
	profileType := contentType{
		kinds:    []nostr.Kind{0},
		validate: func(nostr.Event) (bool, string) { return true, "kind not accepted" },
		store:    func(nostr.Event) {},
		fetch: func(_ nostr.Filter, _ int) iter.Seq[nostr.Event] {
			return func(yield func(nostr.Event) bool) { fetched = true }
		},
		count:    func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(nostr.ID) error { return nil },
		chunked:  false,
	}
	reg := newRegistry(profileType)

	// Write path: client kind-0 submissions are rejected.
	reject, msg := reg.validate(nostr.Event{Kind: 0})
	if !reject || msg != "kind not accepted" {
		t.Fatalf("kind-0 write: reject=%v msg=%q, want reject + \"kind not accepted\"", reject, msg)
	}

	// Read path: a kind-0 REQ is routed to the profiles fetch.
	for range reg.fetch(nostr.Filter{Kinds: []nostr.Kind{0}}, 10) {
	}
	if !fetched {
		t.Fatal("kind-0 read was not routed to the profiles fetch func")
	}

	// kind-0 search must not be treated as chunked (no chunk-rerank).
	if reg.targetsChunked(nostr.Filter{Kinds: []nostr.Kind{0}}) {
		t.Fatal("kind-0 should not be chunked")
	}
}
```

- [ ] **Step 2: Run test to verify it fails or passes against current registry**

Run: `go test ./... -run TestRegistryKind0ReadWriteSplit -v`
Expected: PASS once it compiles — this test exercises the *existing* registry with a kind-0 contentType, locking in the contract Step 3's wiring depends on. (If it fails to compile because `content_registry_test.go` already declares conflicting imports, merge into the existing file.)

- [ ] **Step 3: Wire main.go, config, and docs**

3a. **Flag** — after `calendarEnabled := os.Getenv("CALENDAR_ENABLED") == "true"` (~`main.go:65`):

```go
	profilesEnabled := os.Getenv("PROFILES_ENABLED") == "true"
```

3b. **Backend init** — after the calendar block closes (~`main.go:295`), add:

```go
	// Profiles (kind-0) Typesense backend — gated behind PROFILES_ENABLED. One
	// document per author pubkey, populated by ProfileManager (not by clients).
	// RawEventStore is intentionally nil: profiles are not relay events in
	// BoltDB, so QueryEvents reconstructs them from the stored eventRaw field.
	var profilesDB *typesense30142.TSBackend
	if profilesEnabled {
		pColl := os.Getenv("TS_COLLECTION_PROFILES")
		if pColl == "" {
			pColl = "profiles_0"
		}
		pSchema := profileSchema(pColl)
		profilesDB = &typesense30142.TSBackend{
			ApiKey:          os.Getenv("TS_APIKEY"),
			Host:            os.Getenv("TS_HOST"),
			CollectionName:  pColl,
			Schema:          &pSchema,
			SearchFields:    "name,display_name,about,nip05",
			StopwordsSet:    stopwordsSet,
			StopwordsList:   stopwordsList,
			StopwordsLocale: "de",
		}
		if err := profilesDB.Init(); err != nil {
			panic(fmt.Sprintf("profiles TSBackend init: %v", err))
		}
		fmt.Printf("Profiles (kind 0) enabled — collection %s\n", pColl)
	}
```

3c. **Registry registration** — after the `calendarEnabled` append block and before `reg := newRegistry(contentTypes...)` (~`main.go:384`):

```go
	if profilesEnabled && profilesDB != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{0},
			validate: func(nostr.Event) (bool, string) { return true, "kind not accepted" }, // reject client kind-0 writes
			store:    func(nostr.Event) {},                                                  // never reached: validate rejects first
			fetch:    profilesDB.QueryEvents,
			count:    profilesDB.CountEvents,
			deleteID: profilesDB.DeleteEvent,
			chunked:  false,
		})
	}
```

3d. **Content kinds for backfill** — immediately after `reg := newRegistry(contentTypes...)` (~`main.go:385`), build the list of content kinds (everything the registry serves except kind 0):

```go
	var profileContentKinds []nostr.Kind
	for _, k := range reg.kinds() {
		if k != 0 {
			profileContentKinds = append(profileContentKinds, k)
		}
	}
```

3e. **ProfileManager lifecycle** — after the registry/content-kinds setup and after `profilesDB` exists (place near where other managers start; the manager needs `reg` only indirectly via `profileContentKinds`, `&boltDB`, and `&mgmt`). Add:

```go
	var profileMgr *ProfileManager
	if profilesEnabled && profilesDB != nil {
		profileRelays := []string{"wss://relay.edufeed.org"}
		if raw := os.Getenv("PROFILE_RELAYS"); raw != "" {
			profileRelays = nil
			for _, r := range strings.Split(raw, ",") {
				if r = strings.TrimSpace(r); r != "" {
					profileRelays = append(profileRelays, r)
				}
			}
		}
		refreshInterval := 6 * time.Hour
		if raw := os.Getenv("PROFILE_REFRESH_INTERVAL"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil {
				refreshInterval = d
			} else {
				fmt.Printf("profile: bad PROFILE_REFRESH_INTERVAL %q, using %s\n", raw, refreshInterval)
			}
		}
		profileMgr = NewProfileManager(
			&mgmt,
			poolSource{pool: nostr.NewPool()},
			func(e nostr.Event) { storeProfile(profilesEnabled, profilesDB, e) },
			func() []nostr.PubKey { return backfillAuthors(&boltDB, profileContentKinds, 1_000_000) },
			profileRelays,
			50,
		)
		if err := profileMgr.Init(); err != nil {
			fmt.Printf("profile: init: %v\n", err)
		}
		profileMgr.StartRefreshLoop(refreshInterval)
		defer profileMgr.Stop()
		fmt.Printf("Profiles: fetching from %v, refresh every %s\n", profileRelays, refreshInterval)
	}
```

3f. **Enqueue on write** — in `relay.StoreEvent` and `relay.ReplaceEvent` (~`main.go:521-530`), add the enqueue line after `reg.store(event)`:

```go
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, false)
		reg.store(event)
		if profileMgr != nil {
			profileMgr.Enqueue(event.PubKey)
		}
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, true)
		reg.store(event)
		if profileMgr != nil {
			profileMgr.Enqueue(event.PubKey)
		}
		return nil
	}
```

3g. **Imports** — ensure `main.go` imports `strings` and `time` (both almost certainly already present). If `strings` is missing, add it to the import block.

3h. **.env.example** — append:

```
# Profiles (kind-0) author index — resolve org/person names to pubkeys.
# When true, the relay indexes the kind-0 profiles of content authors and
# serves them via NIP-50 search over kinds:[0]. Client kind-0 writes are
# rejected; the index is populated by an internal fetcher.
PROFILES_ENABLED=false
# Typesense collection for kind-0 profiles.
TS_COLLECTION_PROFILES=profiles_0
# Comma-separated source relays the fetcher pulls kind-0 from.
# relay.edufeed.org is a general relay that holds kind-0; the AMB relays do not.
PROFILE_RELAYS=wss://relay.edufeed.org
# How often to re-fetch known authors' kind-0 (Go duration).
PROFILE_REFRESH_INTERVAL=6h
```

3i. **CLAUDE.md** — in the "Environment Variables" section add the four vars above (one bullet each), and add a feature paragraph after the Calendar section:

```markdown
**Profiles (kind-0 author index), gated behind `PROFILES_ENABLED`:**
- When `PROFILES_ENABLED=true`, the relay indexes the kind-0 profiles of authors who publish content here, so clients can resolve org/person names to pubkeys (e.g. for calendar `authors` filters). Served via NIP-50 `search` over `kinds:[0]`.
- Client kind-0 writes are **rejected** (`validate` returns "kind not accepted"); the index is populated only by the internal `ProfileManager` (`profile_manager.go`), which fetches kind-0 from `PROFILE_RELAYS` for every content author (enqueued on write into a durable BoltDB `profile_queue` bucket, plus a startup backfill scan and a `PROFILE_REFRESH_INTERVAL` refresh).
- kind-0 is registered as a read-only `contentType` (`fetch: profilesDB.QueryEvents`) in a SEPARATE Typesense collection (`profiles_0` / `TS_COLLECTION_PROFILES`); `profilesDB.RawEventStore` is nil, so events are reconstructed from the stored `eventRaw`. Profiles are NOT part of the BoltDB→Typesense reindex.
```

- [ ] **Step 4: Build, run the full test suite, and smoke-check the binary**

Run: `GOWORK=off go build .`
Expected: builds clean (verifies the standalone module build picks up the wiring).

Run: `go test ./...`
Expected: PASS, including the four prior tasks' tests and `TestRegistryKind0ReadWriteSplit`.

Manual smoke (optional, against the dev stack with `PROFILES_ENABLED=true`, `LONGFORM_ENABLED=true`): publish a kind-30142 event from a known author, wait for a drain, then:
`nak req -k 0 --search "<author name>" ws://localhost:3334`
Expected: the author's kind-0 event is returned. A client attempt to publish a kind-0 (`nak event -k 0 ...`) is rejected with "kind not accepted".

- [ ] **Step 5: Commit**

```bash
git add main.go .env.example CLAUDE.md content_registry_test.go
git commit -m "feat(profiles): wire ProfileManager + kind-0 registry split behind PROFILES_ENABLED"
```

---

## Self-Review

**1. Spec coverage:**
- Relay-side index, kind-0 + NIP-50 lookup → Task 1 (projection/schema) + Task 4 (registry registration, search via SearchFields). ✓
- On-write enqueue + refresh loop + startup backfill → Task 3 (`Enqueue`, `StartRefreshLoop`, `backfillAuthors`, `Init`) + Task 4 (`StoreEvent`/`ReplaceEvent` enqueue, lifecycle). ✓
- Registry read/write split (reject writes, serve reads) → Task 4 contentType + `TestRegistryKind0ReadWriteSplit`. ✓
- Durable dedup'd candidate queue → Task 2. ✓
- Configurable `PROFILE_RELAYS` default `relay.edufeed.org`, `PROFILES_ENABLED`, `TS_COLLECTION_PROFILES`, `PROFILE_REFRESH_INTERVAL` → Task 4 (3e, 3h, 3i). ✓
- Reuse `sdk.ParseMetadata`, `structured.go`, AllowlistManager shape → Tasks 1 & 3. ✓
- `RawEventStore` nil / reconstruct from `eventRaw` → Task 4 (3b) + Global Constraints. ✓
- Error handling (fetch fail leaves queued, ParseMetadata fail skips, disabled = noop) → Task 3 `drainOnce` + `TestDrainStoresFoundLeavesMissingQueued`. ✓
- Admission v1 = authored ≥1 content event → enqueue only from `StoreEvent`/`ReplaceEvent` + backfill of content kinds (Task 4 3d/3e/3f). ✓
- NIP-05 deferred → `nip05` field captured but not validated; no validation code. ✓
- Reindex excludes profiles → documented in CLAUDE.md (3i); no reindex change made. ✓
- Testing plan → unit tests in Tasks 1-4 with fakes/temp BoltDB, no live deps. ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows full code; no "similar to" references. ✓

**3. Type consistency:** `profileQueue` (Task 3) ↔ `ManagementStore` methods (Task 2) — names match (`EnqueueProfileCandidate`/`RemoveProfileCandidate`/`ListProfileQueue`). `profileSource.Fetch` signature matches `poolSource.Fetch` and `fakeSource.Fetch`. `backfillAuthors(eventQuerier, []nostr.Kind, int)` matches the main.go closure and the `sliceQuerier` test. `storeProfile(bool, *TSBackend, nostr.Event)` matches the Task 4 closure. `contentType` field names match the existing struct used in `main.go`. `nostrToProfile`/`profileSchema`/`ProfileDocument` consistent across Tasks 1 and 4. ✓
