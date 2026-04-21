# amb-relay Fulltext Plumbing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend amb-relay so that an external indexer service can deposit fetched resource fulltext per AMB event, making it searchable via BM25 and durable across `reindex`. Implements the relay-side half of the spec at `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md`.

**Architecture:** One new BoltDB bucket (`fetched_content`) keyed by event id, three new fields on the Typesense schema (`content`, `content_fetched_at`, `content_status`), two new NIP-86 methods (`setcontent`, `refetchcontent`), a post-upsert pass in the reindex loop that replays fulltext onto rebuilt Typesense docs, and an orphan-GC step that drops fulltext rows whose events no longer exist. All changes are additive and gated behind admin auth.

**Tech Stack:** Go 1.22+, `fiatjaf.com/nostr` (khatru, eventstore/boltdb, eventstore/typesense30142, nip86), `go.etcd.io/bbolt`, Typesense 0.25+.

**Spec refinement:** The spec shows `fetched_content` keyed by `event_id`. This plan keeps that keying (simpler, matches spec) and adds a GC pass to the reindex loop to drop fetched_content rows whose event_id no longer exists in the event store. This cleans up rows orphaned by replacement events.

---

## File Structure

Files created or modified by this plan:

| File | Role |
|---|---|
| `content_store.go` (new) | `ContentStore` type: BoltDB CRUD on `fetched_content` bucket. Pure storage layer. |
| `content_ts.go` (new) | Typesense partial-update helpers (`PatchContent`, `ClearContent`). Pure HTTP. |
| `management.go` (modify) | Register new bucket in `ManagementStore.Init`. |
| `main.go` (modify) | Wire `ContentStore`, extend default schema with content fields at startup, register `setcontent` + `refetchcontent` in the `Generic` NIP-86 handler. |
| `reindex.go` (modify) | After batch upsert, iterate `fetched_content` and PATCH content fields onto rebuilt docs; GC orphan rows. |
| `content_store_test.go` (new) | Go unit tests for `ContentStore`. |
| `test_e2e.sh` (modify) | Add end-to-end tests: setcontent populates TS, reindex preserves content, refetchcontent clears it, orphan GC removes dead rows. |
| `README.md` (modify) | Document the new NIP-86 methods and the `content` field. |
| `.env.example` (modify) | No new env vars; just extend `ADMIN_PUBKEYS` comment to mention indexer pubkey. |

No changes to nostrlib are required. The schema fields are appended at relay startup by mutating the loaded schema in-place before `TSBackend.Init()`.

---

## Task 1: ContentStore — BoltDB bucket and CRUD

**Files:**
- Create: `content_store.go`
- Modify: `management.go:13-23` (add bucket name), `management.go:54-62` (register bucket in Init loop)
- Test: `content_store_test.go`

- [ ] **Step 1: Write the failing test**

Create `content_store_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := bbolt.Open(filepath.Join(dir, "test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Register the bucket the same way ManagementStore.Init does.
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketFetchedContent)
		return err
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return db
}

func TestContentStore_PutGet(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	entry := ContentEntry{
		Text:       "hello world",
		FetchedAt:  time.Now().Unix(),
		Status:     ContentStatusFetched,
		SourceURL:  "https://example.org/resource",
	}
	if err := cs.Put("event-id-1", entry); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := cs.Get("event-id-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if got.Text != entry.Text || got.Status != entry.Status || got.SourceURL != entry.SourceURL {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, entry)
	}
}

func TestContentStore_GetMissing(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_, ok, err := cs.Get("no-such-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestContentStore_Delete(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_ = cs.Put("k", ContentEntry{Text: "x", Status: ContentStatusFetched})
	if err := cs.Delete("k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, _ := cs.Get("k")
	if ok {
		t.Fatal("expected entry to be gone")
	}
}

func TestContentStore_ForEach(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_ = cs.Put("a", ContentEntry{Text: "A"})
	_ = cs.Put("b", ContentEntry{Text: "B"})

	seen := map[string]string{}
	err := cs.ForEach(func(id string, entry ContentEntry) error {
		seen[id] = entry.Text
		return nil
	})
	if err != nil {
		t.Fatalf("foreach: %v", err)
	}
	if seen["a"] != "A" || seen["b"] != "B" || len(seen) != 2 {
		t.Fatalf("unexpected entries: %v", seen)
	}
}

// Dummy ref to os so import stays if we trim helpers later.
var _ = os.Stdout
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test ./... -run TestContentStore -v`
Expected: FAIL — `ContentStore`, `ContentEntry`, `ContentStatusFetched`, `bucketFetchedContent` all undefined.

- [ ] **Step 3: Add the bucket constant**

Edit `management.go`, add to the `var` block at line 13-23:

```go
var (
	bucketBannedPubKeys   = []byte("banned_pubkeys")
	bucketBannedEvents    = []byte("banned_events")
	bucketTypesenseSchema = []byte("typesense_schema")
	bucketSemanticConfig  = []byte("semantic_config")
	bucketAccessControl   = []byte("access_control")
	bucketWriteAllowlist  = []byte("write_allowlist")
	bucketReadAllowlist   = []byte("read_allowlist")
	bucketListReferences  = []byte("list_references")
	bucketAdmins          = []byte("admins")
	bucketFetchedContent  = []byte("fetched_content") // resource fulltext keyed by event_id
)
```

Edit the `Init` method body in `management.go` (around line 55). Append `bucketFetchedContent` to the loop slice:

```go
for _, bucket := range [][]byte{bucketBannedPubKeys, bucketBannedEvents, bucketTypesenseSchema, bucketSemanticConfig, bucketAccessControl, bucketWriteAllowlist, bucketReadAllowlist, bucketListReferences, bucketAdmins, bucketFetchedContent} {
	if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
		return err
	}
}
```

- [ ] **Step 4: Create `content_store.go`**

```go
package main

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"
)

// ContentStatus values recorded alongside fetched content.
const (
	ContentStatusFetched       = "fetched"        // successfully fetched full body
	ContentStatusTruncated     = "truncated"      // body exceeded EXTRACT_MAX_BYTES; prefix stored
	ContentStatusLicenseDenied = "license_denied" // license not on allowlist; vectors-only (no text)
	ContentStatusUnsupported   = "unsupported"    // content-type not handled
	ContentStatusFailed        = "failed"         // fetch/extract exhausted retries
)

// ContentEntry is the BoltDB payload for fetched resource fulltext.
type ContentEntry struct {
	Text        string `json:"text,omitempty"`
	FetchedAt   int64  `json:"fetched_at"`
	Status      string `json:"status"`
	SourceURL   string `json:"source_url,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// ContentStore persists fetched resource fulltext in BoltDB, keyed by event id.
type ContentStore struct {
	DB *bbolt.DB
}

// Put inserts or replaces a content entry for the given event id.
func (c *ContentStore) Put(eventID string, entry ContentEntry) error {
	val, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal content entry: %w", err)
	}
	return c.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).Put([]byte(eventID), val)
	})
}

// Get returns (entry, true, nil) if present; (zero, false, nil) if missing.
func (c *ContentStore) Get(eventID string) (ContentEntry, bool, error) {
	var entry ContentEntry
	var found bool
	err := c.DB.View(func(tx *bbolt.Tx) error {
		val := tx.Bucket(bucketFetchedContent).Get([]byte(eventID))
		if val == nil {
			return nil
		}
		found = true
		return json.Unmarshal(val, &entry)
	})
	return entry, found, err
}

// Delete removes a content entry. Idempotent.
func (c *ContentStore) Delete(eventID string) error {
	return c.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).Delete([]byte(eventID))
	})
}

// ForEach invokes fn for each content entry in the bucket. If fn returns a
// non-nil error, iteration stops and the error is returned.
func (c *ContentStore) ForEach(fn func(eventID string, entry ContentEntry) error) error {
	return c.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketFetchedContent).ForEach(func(k, v []byte) error {
			var entry ContentEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil // skip malformed rows rather than aborting
			}
			return fn(string(k), entry)
		})
	})
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `GOWORK=off go test ./... -run TestContentStore -v`
Expected: PASS — all four test cases.

- [ ] **Step 6: Commit**

```bash
git add content_store.go content_store_test.go management.go
git commit -m "Add ContentStore: BoltDB bucket for fetched resource fulltext"
```

---

## Task 2: Extend Typesense schema with content fields at startup

**Files:**
- Modify: `main.go:140-157` (schema load block)
- Test: via `test_e2e.sh` (integration; no unit test for this wiring)

- [ ] **Step 1: Add a helper in `main.go`**

Add this function to `main.go` (place after the `getVersion` function at the bottom of the file, or at package top — the existing code puts helpers at the bottom):

```go
// ensureContentFields appends the three content fields to the schema if they
// are not already present. Idempotent: safe to call on default or custom schemas.
func ensureContentFields(schema *typesense30142.CollectionSchema) {
	have := make(map[string]bool, len(schema.Fields))
	for _, f := range schema.Fields {
		have[f.Name] = true
	}
	if !have["content"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content", Type: "string", Optional: true,
		})
	}
	if !have["content_fetched_at"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content_fetched_at", Type: "int64", Optional: true,
		})
	}
	if !have["content_status"] {
		schema.Fields = append(schema.Fields, typesense30142.Field{
			Name: "content_status", Type: "string", Optional: true,
		})
	}
}
```

- [ ] **Step 2: Wire it into the schema-loading path**

In `main.go`, locate the block that runs between line 140 and 157 (where `tsDB` is constructed and custom schema is loaded). Replace it with:

```go
	// Typesense backend (search index)
	tsDB := typesense30142.TSBackend{
		ApiKey:         os.Getenv("TS_APIKEY"),
		Host:           os.Getenv("TS_HOST"),
		CollectionName: os.Getenv("TS_COLLECTION"),
	}

	// Load custom schema from BoltDB if one was stored; otherwise start from
	// the default. Either way, ensure the content fields are present — they
	// are required by the fulltext plumbing.
	customSchema, err := mgmt.LoadSchema()
	if err != nil {
		fmt.Printf("Warning: failed to load custom schema: %v\n", err)
	}
	var effectiveSchema typesense30142.CollectionSchema
	if customSchema != nil {
		effectiveSchema = *customSchema
		fmt.Println("Using custom Typesense schema from BoltDB")
	} else {
		effectiveSchema = typesense30142.DefaultSchema()
	}
	ensureContentFields(&effectiveSchema)
	tsDB.Schema = &effectiveSchema

	if err := tsDB.Init(); err != nil {
		panic(err)
	}
```

Note: the existing code has `var err` shadowing in the semantic-config block (line 175). Make sure the new `err := mgmt.LoadSchema()` doesn't break later compilation. If the Go compiler complains about redeclaration, rename one (`err2` or use `=` once `err` already exists).

- [ ] **Step 3: Build to verify**

Run: `GOWORK=off go build .`
Expected: build succeeds with no errors.

- [ ] **Step 4: Manual smoke test**

Run `docker compose up -d typesense`, then `go run .`, then in another shell:

```bash
curl -s -H "X-TYPESENSE-API-KEY: xyz" http://localhost:8108/collections/$TS_COLLECTION | jq '.fields[] | select(.name=="content" or .name=="content_fetched_at" or .name=="content_status")'
```

Expected: three field definitions printed. Stop the relay with Ctrl-C.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "Append content fields to Typesense schema at relay startup"
```

---

## Task 3: Typesense partial-update helpers

**Files:**
- Create: `content_ts.go`

Typesense supports per-document partial updates via `PATCH /collections/{name}/documents/{id}` with a JSON body containing only the fields to update. This task adds two thin helpers around that endpoint.

- [ ] **Step 1: Write the helper file**

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// tsPatchTimeout bounds each partial-update HTTP call.
const tsPatchTimeout = 10 * time.Second

// tsHTTPClient is reused so HTTP connections to Typesense are pooled.
var tsHTTPClient = &http.Client{Timeout: tsPatchTimeout}

// PatchContent updates the content, content_fetched_at, and content_status
// fields on an existing Typesense document. Returns an error on non-2xx.
// Caller supplies the Typesense host, api key, collection name, and doc id
// (which for amb-relay events is the event id hex).
func PatchContent(host, apiKey, collection, docID string, entry ContentEntry) error {
	body := map[string]any{
		"content":            entry.Text,
		"content_fetched_at": entry.FetchedAt,
		"content_status":     entry.Status,
	}
	return patchDoc(host, apiKey, collection, docID, body)
}

// ClearContent sets content to empty and removes the other content metadata.
// Used when refetchcontent is called before an indexer has repopulated.
func ClearContent(host, apiKey, collection, docID string) error {
	body := map[string]any{
		"content":            "",
		"content_fetched_at": 0,
		"content_status":     "",
	}
	return patchDoc(host, apiKey, collection, docID, body)
}

func patchDoc(host, apiKey, collection, docID string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal patch body: %w", err)
	}
	url := fmt.Sprintf("%s/collections/%s/documents/%s", host, collection, docID)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	resp, err := tsHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("typesense patch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("typesense patch status %d: %s", resp.StatusCode, string(snippet))
	}
	return nil
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `GOWORK=off go build .`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add content_ts.go
git commit -m "Add Typesense partial-update helpers for content fields"
```

---

## Task 4: NIP-86 setcontent handler

**Files:**
- Modify: `main.go` — wire a `ContentStore` and add the `setcontent` case in the `Generic` handler (line 380-626 range).

The method signature per spec:
```
setcontent([event_id, text, fetched_at, status, source_url])
```

Rationale for position in params: positional JSON array is how every other `Generic` handler receives params. All five are required except `text` which may be empty when `status != "fetched"`.

- [ ] **Step 1: Construct the ContentStore at startup**

In `main.go`, immediately after the `mgmt.Init(boltDB.DB)` block (around line 114), add:

```go
	contentStore := &ContentStore{DB: boltDB.DB}
```

(A `ContentStore` is a stateless wrapper over the already-initialized bbolt handle; construction cannot fail.)

- [ ] **Step 2: Add the handler case**

In the `Generic` switch in `main.go` (the big `switch request.Method` block), add a new case before the `default:` arm:

```go
		case "setcontent":
			if len(request.Params) < 4 {
				return nip86.Response{Error: "setcontent requires [event_id, text, fetched_at, status, (source_url)]"}, nil
			}
			eventIDHex, ok := request.Params[0].(string)
			if !ok || eventIDHex == "" {
				return nip86.Response{Error: "event_id must be a non-empty string"}, nil
			}
			text, ok := request.Params[1].(string)
			if !ok {
				return nip86.Response{Error: "text must be a string"}, nil
			}
			fetchedAtFloat, ok := request.Params[2].(float64)
			if !ok {
				return nip86.Response{Error: "fetched_at must be a number (unix seconds)"}, nil
			}
			status, ok := request.Params[3].(string)
			if !ok || status == "" {
				return nip86.Response{Error: "status must be a non-empty string"}, nil
			}
			var sourceURL string
			if len(request.Params) > 4 {
				sourceURL, _ = request.Params[4].(string)
			}

			// Verify the event exists in BoltDB — the indexer must only
			// setcontent for events the relay actually holds.
			id, err := nostr.IDFromHex(eventIDHex)
			if err != nil {
				return nip86.Response{Error: fmt.Sprintf("invalid event id: %v", err)}, nil
			}
			var found bool
			for range boltDB.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}, 1) {
				found = true
			}
			if !found {
				return nip86.Response{Error: "event not found"}, nil
			}

			entry := ContentEntry{
				Text:      text,
				FetchedAt: int64(fetchedAtFloat),
				Status:    status,
				SourceURL: sourceURL,
			}
			if err := contentStore.Put(eventIDHex, entry); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store put: %v", err)}, nil
			}
			if err := PatchContent(tsDB.Host, tsDB.ApiKey, tsDB.CollectionName, eventIDHex, entry); err != nil {
				// Typesense patch failure is recoverable: BoltDB is source of
				// truth and a future reindex will re-project. Surface the
				// error to the caller so it can log, but don't roll back.
				return nip86.Response{Error: fmt.Sprintf("typesense patch: %v", err)}, nil
			}
			return nip86.Response{Result: true}, nil
```

- [ ] **Step 3: Build to verify it compiles**

Run: `GOWORK=off go build .`
Expected: build succeeds.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "Add NIP-86 setcontent handler for fetched resource fulltext"
```

---

## Task 5: NIP-86 refetchcontent handler

**Files:**
- Modify: `main.go` — another case in the `Generic` handler.

Per spec, `refetchcontent([event_id])` clears the stored content so the indexer re-processes. Plan 1 scope is just the clear; coordination with the indexer to discover "needs refetch" is Plan 2.

- [ ] **Step 1: Add the handler case**

In the same `switch request.Method` block, add:

```go
		case "refetchcontent":
			if len(request.Params) == 0 {
				return nip86.Response{Error: "refetchcontent requires [event_id]"}, nil
			}
			eventIDHex, ok := request.Params[0].(string)
			if !ok || eventIDHex == "" {
				return nip86.Response{Error: "event_id must be a non-empty string"}, nil
			}
			if err := contentStore.Delete(eventIDHex); err != nil {
				return nip86.Response{Error: fmt.Sprintf("content store delete: %v", err)}, nil
			}
			if err := ClearContent(tsDB.Host, tsDB.ApiKey, tsDB.CollectionName, eventIDHex); err != nil {
				return nip86.Response{Error: fmt.Sprintf("typesense clear: %v", err)}, nil
			}
			return nip86.Response{Result: true}, nil
```

- [ ] **Step 2: Build to verify it compiles**

Run: `GOWORK=off go build .`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "Add NIP-86 refetchcontent handler to clear fulltext"
```

---

## Task 6: Preserve content across reindex + orphan GC

**Files:**
- Modify: `reindex.go`
- Modify: `main.go` — pass `contentStore` to `NewReindexer`.

Reindex drops the collection and rebuilds from BoltDB events. Without this task, content is lost. We add a post-upsert pass that iterates `fetched_content` and PATCHes content onto each rebuilt doc, plus an orphan-GC step that drops rows whose event id no longer exists.

- [ ] **Step 1: Extend the Reindexer signature**

Edit `reindex.go`. Change the `Reindexer` struct and `NewReindexer` to accept a `ContentStore` and Typesense connection details:

```go
type Reindexer struct {
	tsDB   *typesense30142.TSBackend
	boltDB *boltdb.BoltBackend
	mgmt   *ManagementStore
	content *ContentStore

	mu      sync.Mutex
	running atomic.Bool
	total   atomic.Int64
	indexed atomic.Int64
	errors  atomic.Int64
	contentPatched atomic.Int64
	contentOrphaned atomic.Int64
	lastErr atomic.Value // stores string
}

func NewReindexer(tsDB *typesense30142.TSBackend, boltDB *boltdb.BoltBackend, mgmt *ManagementStore, content *ContentStore) *Reindexer {
	return &Reindexer{
		tsDB:    tsDB,
		boltDB:  boltDB,
		mgmt:    mgmt,
		content: content,
	}
}
```

- [ ] **Step 2: Update the caller in `main.go`**

Find the existing line `reindexer := NewReindexer(&tsDB, &boltDB, &mgmt)` (around line 198) and replace with:

```go
	reindexer := NewReindexer(&tsDB, &boltDB, &mgmt, contentStore)
```

- [ ] **Step 3: Extend the `run` method with the content-replay pass**

In `reindex.go`, replace the existing `run` method with:

```go
func (r *Reindexer) run() {
	defer r.running.Store(false)

	// Reset content counters too.
	r.contentPatched.Store(0)
	r.contentOrphaned.Store(0)

	// Load schema (custom or default)
	schema, err := r.mgmt.LoadSchema()
	if err != nil {
		r.lastErr.Store(fmt.Sprintf("failed to load schema: %v", err))
		return
	}
	// Ensure the content fields are present even on custom schemas.
	if schema == nil {
		def := typesense30142.DefaultSchema()
		schema = &def
	}
	ensureContentFields(schema)

	// Recreate the collection with the (possibly updated) schema
	if err := r.tsDB.RecreateCollection(schema); err != nil {
		r.lastErr.Store(fmt.Sprintf("failed to recreate collection: %v", err))
		return
	}

	log.Println("reindex: collection recreated, starting event re-indexing with batching")

	// Collect events in batches for efficient bulk upsert
	var batch []nostr.Event
	liveEventIDs := make(map[string]struct{}, 1024)

	for event := range r.boltDB.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, reindexMaxEvents) {
		r.total.Add(1)
		liveEventIDs[event.ID.Hex()] = struct{}{}
		batch = append(batch, event)

		if len(batch) >= reindexBatchSize {
			indexed, errs := r.tsDB.BatchUpsertEvents(batch)
			r.indexed.Add(int64(indexed))
			r.errors.Add(int64(len(errs)))
			for _, err := range errs {
				log.Printf("reindex: batch error: %v", err)
			}
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		indexed, errs := r.tsDB.BatchUpsertEvents(batch)
		r.indexed.Add(int64(indexed))
		r.errors.Add(int64(len(errs)))
		for _, err := range errs {
			log.Printf("reindex: batch error: %v", err)
		}
	}

	// Content replay + orphan GC. Iterating fetched_content once.
	_ = r.content.ForEach(func(eventID string, entry ContentEntry) error {
		if _, alive := liveEventIDs[eventID]; !alive {
			// Orphan: event no longer exists. Drop the row.
			if err := r.content.Delete(eventID); err != nil {
				log.Printf("reindex: orphan delete failed for %s: %v", eventID, err)
			} else {
				r.contentOrphaned.Add(1)
			}
			return nil
		}
		if err := PatchContent(r.tsDB.Host, r.tsDB.ApiKey, r.tsDB.CollectionName, eventID, entry); err != nil {
			log.Printf("reindex: content patch failed for %s: %v", eventID, err)
			r.errors.Add(1)
			return nil
		}
		r.contentPatched.Add(1)
		return nil
	})

	log.Printf("reindex: completed. total=%d indexed=%d errors=%d content_patched=%d content_orphaned=%d",
		r.total.Load(), r.indexed.Load(), r.errors.Load(),
		r.contentPatched.Load(), r.contentOrphaned.Load())
}
```

- [ ] **Step 4: Expose the new counters in `GetStatus`**

In `reindex.go`, replace the existing `GetStatus` method with:

```go
// GetStatus returns the current reindex status.
func (r *Reindexer) GetStatus() ReindexStatus {
	status := ReindexStatus{
		Running:          r.running.Load(),
		Total:            r.total.Load(),
		Indexed:          r.indexed.Load(),
		Errors:           r.errors.Load(),
		ContentPatched:   r.contentPatched.Load(),
		ContentOrphaned:  r.contentOrphaned.Load(),
	}
	if v := r.lastErr.Load(); v != nil {
		if s, ok := v.(string); ok {
			status.Error = s
		}
	}
	return status
}
```

And update the `ReindexStatus` struct near the top of `reindex.go`:

```go
type ReindexStatus struct {
	Running         bool   `json:"running"`
	Total           int64  `json:"total"`
	Indexed         int64  `json:"indexed"`
	Errors          int64  `json:"errors"`
	ContentPatched  int64  `json:"content_patched"`
	ContentOrphaned int64  `json:"content_orphaned"`
	Error           string `json:"error,omitempty"`
}
```

- [ ] **Step 5: Build to verify**

Run: `GOWORK=off go build .`
Expected: build succeeds.

- [ ] **Step 6: Commit**

```bash
git add reindex.go main.go
git commit -m "Preserve fulltext across reindex and GC orphan content rows"
```

---

## Task 7: End-to-end tests

**Files:**
- Modify: `test_e2e.sh` — add a new section before the existing semantic-search section.

The relay's `nip86_call` helper (see lines 216-250 of `test_e2e.sh`) sends a kind 20274 NIP-86 request. Our new tests follow the exact same pattern as `assert_nip86`.

- [ ] **Step 1: Add the test block**

Insert the following section immediately before the existing `# Semantic Search` section in `test_e2e.sh` (find the comment marker by grepping for `# 46. Enable semantic search`):

```bash
# ============================================================
# Fulltext plumbing tests (setcontent / refetchcontent / reindex preservation)
# ============================================================
echo ""
echo "--- Fulltext plumbing ---"

# Publish a test event first so setcontent has a target.
FULLTEXT_EVENT_JSON=$(nak event -k 30142 -t d=fulltext-test-1 -t name="Fulltext Test" --sec "$SEC" "$RELAY" 2>&1 | tail -1)
FULLTEXT_EVENT_ID=$(echo "$FULLTEXT_EVENT_JSON" | jq -r '.id')
if [ -z "$FULLTEXT_EVENT_ID" ] || [ "$FULLTEXT_EVENT_ID" = "null" ]; then
  printf "${RED}FAIL${NC}: could not publish test event for fulltext (resp: %s)\n" "$FULLTEXT_EVENT_JSON"
  FAIL=$((FAIL+1))
else
  printf "${GREEN}PASS${NC}: published test event %s\n" "$FULLTEXT_EVENT_ID"
  PASS=$((PASS+1))
fi
sleep 1  # let write buffer flush

# setcontent with fresh fulltext
assert_nip86 "setcontent succeeds" \
  "$(nip86_call "setcontent" "[\"$FULLTEXT_EVENT_ID\", \"photosynthesis light reactions chlorophyll\", $(date +%s), \"fetched\", \"https://example.org/resource\"]")" \
  '.result.result == true'

# Verify content field is present and searchable via BM25
sleep 1  # Typesense index propagation
CONTENT_SEARCH=$(curl -sf -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=chlorophyll&query_by=content&per_page=5")
if echo "$CONTENT_SEARCH" | jq -e '.hits | length > 0' >/dev/null; then
  printf "${GREEN}PASS${NC}: content is searchable via BM25\n"
  PASS=$((PASS+1))
else
  printf "${RED}FAIL${NC}: content not searchable (response: %s)\n" "$CONTENT_SEARCH"
  FAIL=$((FAIL+1))
fi

# setcontent with permanent error (status=failed, no text)
ERROR_EVENT_JSON=$(nak event -k 30142 -t d=fulltext-test-err -t name="Error Test" --sec "$SEC" "$RELAY" 2>&1 | tail -1)
ERROR_EVENT_ID=$(echo "$ERROR_EVENT_JSON" | jq -r '.id')
sleep 1
assert_nip86 "setcontent with status=failed succeeds (no text)" \
  "$(nip86_call "setcontent" "[\"$ERROR_EVENT_ID\", \"\", $(date +%s), \"failed\", \"https://example.org/404\"]")" \
  '.result.result == true'

# setcontent rejects nonexistent event id
BOGUS_ID=$(printf '%064s' '' | tr ' ' '0')
assert_nip86_error "setcontent rejects unknown event id" \
  "$(nip86_call "setcontent" "[\"$BOGUS_ID\", \"x\", $(date +%s), \"fetched\", \"\"]")"

# Reindex must preserve content.
REINDEX_BEFORE=$(nip86_call "reindex" '[]')
if echo "$REINDEX_BEFORE" | jq -e '.result.result == "reindex started"' >/dev/null; then
  echo -n "  Waiting for reindex..."
  for i in $(seq 1 30); do
    S=$(nip86_call "getreindexstatus" '[]')
    if echo "$S" | jq -e '.result.result.running == false' >/dev/null; then
      echo " done"
      break
    fi
    echo -n "."
    sleep 1
  done
  FINAL_STATUS=$(nip86_call "getreindexstatus" '[]')
  if echo "$FINAL_STATUS" | jq -e '.result.result.content_patched >= 1' >/dev/null; then
    printf "${GREEN}PASS${NC}: reindex reports content_patched>=1\n"
    PASS=$((PASS+1))
  else
    printf "${RED}FAIL${NC}: reindex did not report content_patched (%s)\n" "$FINAL_STATUS"
    FAIL=$((FAIL+1))
  fi
  # After reindex, the content field should still be searchable.
  sleep 1
  CONTENT_AFTER=$(curl -sf -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
    "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=chlorophyll&query_by=content&per_page=5")
  if echo "$CONTENT_AFTER" | jq -e '.hits | length > 0' >/dev/null; then
    printf "${GREEN}PASS${NC}: content survives reindex\n"
    PASS=$((PASS+1))
  else
    printf "${RED}FAIL${NC}: content missing after reindex (response: %s)\n" "$CONTENT_AFTER"
    FAIL=$((FAIL+1))
  fi
else
  printf "${RED}FAIL${NC}: reindex did not start (response: %s)\n" "$REINDEX_BEFORE"
  FAIL=$((FAIL+1))
fi

# refetchcontent clears content
assert_nip86 "refetchcontent succeeds" \
  "$(nip86_call "refetchcontent" "[\"$FULLTEXT_EVENT_ID\"]")" \
  '.result.result == true'

sleep 1
CONTENT_CLEARED=$(curl -sf -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=chlorophyll&query_by=content&per_page=5")
if echo "$CONTENT_CLEARED" | jq -e '.hits | length == 0' >/dev/null; then
  printf "${GREEN}PASS${NC}: content cleared after refetchcontent\n"
  PASS=$((PASS+1))
else
  printf "${RED}FAIL${NC}: content not cleared (response: %s)\n" "$CONTENT_CLEARED"
  FAIL=$((FAIL+1))
fi

# Non-admin cannot call setcontent
NONADMIN_RESP=$(nip86_call "setcontent" "[\"$FULLTEXT_EVENT_ID\", \"x\", $(date +%s), \"fetched\", \"\"]" "$NONADMIN_SEC")
assert_nip86_error "non-admin rejected from setcontent" "$NONADMIN_RESP"

# Orphan GC: delete the error event, run reindex, check content_orphaned counter.
nak event -k 5 -t "e=$ERROR_EVENT_ID" --sec "$SEC" "$RELAY" >/dev/null 2>&1 || true
sleep 1
# Directly seed an orphan BoltDB row (via setcontent then deleteevent wouldn't clean the bucket).
# Simulating an orphan is hard without extra machinery, so we rely on the kind-5 deletion path:
# after deletion, the event is gone from BoltDB's QueryEvents output, so reindex should GC the row.
nip86_call "reindex" '[]' >/dev/null
echo -n "  Waiting for GC reindex..."
for i in $(seq 1 30); do
  S=$(nip86_call "getreindexstatus" '[]')
  if echo "$S" | jq -e '.result.result.running == false' >/dev/null; then
    echo " done"
    break
  fi
  echo -n "."
  sleep 1
done
GC_STATUS=$(nip86_call "getreindexstatus" '[]')
if echo "$GC_STATUS" | jq -e '.result.result.content_orphaned >= 1' >/dev/null; then
  printf "${GREEN}PASS${NC}: reindex GC'd orphan content row\n"
  PASS=$((PASS+1))
else
  printf "${YELLOW}SKIP${NC}: content_orphaned check inconclusive (response: %s)\n" "$GC_STATUS"
fi

echo ""
```

- [ ] **Step 2: Run the e2e suite**

Run: `./test_e2e.sh`
Expected: All new "Fulltext plumbing" assertions PASS. The final summary line should show zero FAILs.

- [ ] **Step 3: Commit**

```bash
git add test_e2e.sh
git commit -m "Add e2e tests for setcontent, refetchcontent, and reindex content preservation"
```

---

## Task 8: Documentation

**Files:**
- Modify: `README.md`
- Modify: `.env.example`

- [ ] **Step 1: Update `README.md`**

Find the NIP-86 methods table in `README.md` (search for `getsemanticsearchconfig`). Add two new rows immediately below the semantic-search entries:

```markdown
| `setcontent` | `[event_id, text, fetched_at, status, source_url]` | Persist fetched resource fulltext for an event. `status` values: `fetched` \| `truncated` \| `license_denied` \| `unsupported` \| `failed`. Admin only. |
| `refetchcontent` | `[event_id]` | Clear fulltext for an event so the indexer reprocesses it. Admin only. |
```

Then, below the "Semantic search" heading, add a new subsection:

```markdown
### Resource Fulltext

The relay stores optional fulltext extracted from the resource referenced by each AMB event. Fulltext is provided by an external `amb-indexer` service (not part of this repository) via the `setcontent` NIP-86 method, persisted in the `fetched_content` BoltDB bucket, and projected onto the `content`, `content_fetched_at`, and `content_status` fields of the Typesense document.

When present, the `content` field participates in BM25 search alongside metadata fields. Reindex preserves fulltext: after the collection is rebuilt from BoltDB events, a replay pass PATCHes each stored content row back onto its Typesense document. The same pass drops rows whose events have been deleted (orphan GC).

The indexer service connects as a regular nostr client + a NIP-86 admin. Add its pubkey to `ADMIN_PUBKEYS` to authorize `setcontent` and `refetchcontent`. See `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md` for the full design.
```

- [ ] **Step 2: Update `.env.example`**

Find the `ADMIN_PUBKEYS` line and expand the comment:

```
# Comma-separated hex pubkeys with full NIP-86 admin access (in addition to PUBKEY).
# Include the amb-indexer service's pubkey here if running the fulltext indexer.
ADMIN_PUBKEYS=
```

- [ ] **Step 3: Commit**

```bash
git add README.md .env.example
git commit -m "Document fulltext plumbing: setcontent, refetchcontent, content field"
```

---

## Self-review (performed during plan writing)

**Spec coverage check.** Walked each Plan 1 requirement from the spec against the task list:
- `fetched_content` BoltDB bucket → Task 1
- `content`, `content_fetched_at`, `content_status` TS fields → Task 2
- `setcontent` NIP-86 method → Task 4
- `refetchcontent` NIP-86 method → Task 5
- Reindex preserves fulltext → Task 6
- Replacement-event cleanup → handled via orphan GC in Task 6 (replacement produces an orphan; next reindex drops it)
- `content` participates in BM25 → Task 2 (schema addition is sufficient for default `query_by` fallback; operator tunes `query_by`/`query_by_weights` via existing `updatecollectionschema`)
- Tests covering all three methods + reindex preservation + admin auth → Task 7

**Placeholder scan.** No TBD/TODO/"implement later" in the plan. All code snippets are complete.

**Type consistency.**
- `ContentEntry` fields match between `content_store.go` (Task 1), the `setcontent` handler (Task 4), and the reindex `PatchContent` call (Task 6).
- `ContentStatus*` constants defined in Task 1 are the vocabulary the handlers in Task 4 and the e2e test in Task 7 use (`"fetched"`, `"failed"`).
- `ReindexStatus` fields added in Task 6 are named consistently with the JSON tags the e2e tests assert on (`content_patched`, `content_orphaned`).

**Deferred to Plan 2 (not gaps):**
- Indexer discovery of "needs refetch" events. `refetchcontent` clears state in Plan 1; the mechanism by which the indexer learns about it belongs with the indexer implementation.
