# Phase 3: Content-Type Registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the per-kind `switch event.Kind` / `if longformEnabled` branches scattered through `amb-relay/main.go`'s event path with a single content-type registry, so the AMB (30142) and long-form (30023) paths dispatch through one kind-agnostic mechanism and adding a content type becomes one registration.

**Architecture:** A `registry` of `contentType` records, each binding the per-kind variation the event path actually switches on today — `kinds`, `validate`, `store`, `fetch`, `count`, `deleteID`. The registry lives in `package main` (NOT promoted to nostrlib). `main.go` builds it once (AMB always; long-form appended only when `LONGFORM_ENABLED`) and the event-path closures call registry methods instead of branching on kind.

**Tech Stack:** Go, khatru relay framework (`fiatjaf.com/nostr`), Typesense (`eventstore/typesense30142`), BoltDB.

---

## Why this design deviates from the Phase-3 sketch (read before starting)

The umbrella plan (`2026-06-16-multi-content-search-platform.md`, Phase 3) sketched a *fat* `Projector` interface: `Kinds() / Validate() / Project() / Schema() / QueryMap() / ChunkSource()`. That sketch was written **before** long-form landed, with the explicit note that it "will be re-planned once Phase 1 + long-form expose the real registry interface" — i.e. discovered, not guessed.

Having now built the long-form path, the real duplication is narrower than the sketch assumed:

- **`Project` is NOT duplicated in `main.go`.** AMB projection lives in the lib (`typesense30142.PrepareBatch` → `NostrToAMB`, called inside the write buffer). Long-form projection lives in `longform.go` (`nostrToLongform`, called inside `storeLongform`). `main.go` never calls a projector directly — it calls a *store* path that projects internally. So the registry's variation point is `store func(nostr.Event)`, not `Project`.
- **`Schema` is NOT in the event path.** Each schema is set once at backend `Init` (`tsDB.Schema`, `tsDB2.Schema`). No per-event branch.
- **`QueryMap` is NOT in the event path.** Filter→Typesense mapping is inside `TSBackend.QueryEvents`. The event path only chooses *which backend* to query — captured by `fetch fetchFunc`.
- **`ChunkSource` belongs to amb-indexer, a different process.** The relay does not chunk. Putting it in the relay registry would be inventing surface for the wrong binary.

What *is* duplicated in `main.go` and what the registry therefore captures: validation, the write path, the search backend, per-collection delete, and per-collection count — all keyed by kind. That is the honest extraction from the two real examples. Phase 4 (wiki, 30818) adds a third registration of the same shape.

**Behavior-preservation contract:** When `LONGFORM_ENABLED=false`, only AMB is registered, so every registry method degenerates to the single-backend AMB call it replaces — provably identical. `golden_test.go` (AMB projection + snippet byte output) MUST stay green throughout, and the cross-content integration test MUST keep passing with the flag on.

**Two intentional, documented micro-changes (flag-ON only, long-form is pre-production):**
1. A disabled-kind 30023 event is rejected with `"kind not accepted"` (the generic registry message) instead of `"kind 30023 not enabled on this relay"`. Acceptable: still a rejection; removing the literal is the point. Verify no test asserts the old string.
2. `deleteEverywhere` returns the first error across collections (AMB first, since registered first) and logs the rest. The only divergence from today is the AMB-ok/long-form-delete-fails case, where today returns nil (long-form error logged) and the registry returns the long-form error. Strictly safer; flag-on only.

---

## File Structure

- **Create:** `content_registry.go` — `contentType` struct, `registry` type, `newRegistry`, and methods `kinds() / validate() / store() / selected() / fetch() / count() / deleteEverywhere()`.
- **Create:** `content_registry_test.go` — dispatch, fan-out, de-dup, early-exit, count, delete tests using fakes.
- **Create:** `validate.go` — `validateAMB`, `validateLongform` (pure functions extracted from `main.go`'s `OnEvent` switch).
- **Create:** `validate_test.go` — table-driven validation tests.
- **Modify:** `main.go` — build the registry; replace retention build, `OnEvent` kind switch, `StoreEvent`, `ReplaceEvent`, `DeleteEvent`, `BanEvent`, `Count`, and `QueryStored` fetch wiring with registry calls.
- **Delete:** `query_combined.go` (its `combinedFetch` is subsumed by `registry.fetch`).
- **Modify:** `query_combined_test.go` → fold into `content_registry_test.go` (or delete after porting coverage).
- **Modify:** `query_combined_integration_test.go` — drive `registry.fetch` instead of `combinedFetch`.

No nostrlib changes. No amb-indexer changes.

---

## Reference: exact current code being replaced

`main.go` event-path sites (as of commit `bb88ac1`):

- L60–69: `longformEnabled` + retention build.
- L309–319: `QueryStored` → `fetch = tsDB.QueryEvents`; if flag, `combinedFetch(tsDB.QueryEvents, tsDB2.QueryEvents)`; then `chunkRerankQuery(ctx, filter, chunkSearcher, fetch, maxLimit, relaySK)`.
- L320–339: `Count` fan-out; `StoreEvent` switch (`30142`→`tsBuf.Queue`, `30023`→`storeLongform`); `ReplaceEvent` switch (identical).
- L343–355: `DeleteEvent` — `boltDB.DeleteEvent` + `contentStore.Delete` + (flag) `tsDB2.DeleteEvent` logged + `return tsDB.DeleteEvent(id)`.
- L360–395: `OnEvent` — pre-checks (ban pubkey/event, ACL), kind-5 early return, then `switch event.Kind { case 30142: d+name; case 30023: flag+d+title; default: reject }`.
- L465–475 (now ~L483–501 after Phase-2 ban fix): `BanEvent` — same delete pattern + `mgmt.BanEvent`.

`fetchFunc` (chunk_rerank.go:35): `type fetchFunc func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]`.
`TSBackend` methods used: `QueryEvents(filter, maxLimit) iter.Seq[nostr.Event]`, `CountEvents(filter) (uint32, error)`, `DeleteEvent(id) error`.
`combinedFetch(ambFetch, lfFetch fetchFunc) fetchFunc` (query_combined.go:14) — routes by `filter.Kinds` (30142→AMB, 30023→LF, empty→both), de-dups by `nostr.ID`, respects `yield` early-exit.

---

## Task 1: registry type + single-event dispatch

**Files:**
- Create: `content_registry.go`
- Test: `content_registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
// content_registry_test.go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func mkEvent(kind nostr.Kind, tags nostr.Tags) nostr.Event {
	return nostr.Event{Kind: kind, Tags: tags}
}

func TestRegistryValidateAndStoreDispatch(t *testing.T) {
	var storedAMB, storedLF int
	reg := newRegistry(
		contentType{
			kinds:    []nostr.Kind{30142},
			validate: func(nostr.Event) (bool, string) { return false, "" },
			store:    func(nostr.Event) { storedAMB++ },
		},
		contentType{
			kinds:    []nostr.Kind{30023},
			validate: func(nostr.Event) (bool, string) { return true, "lf-rejected" },
			store:    func(nostr.Event) { storedLF++ },
		},
	)

	if reject, _ := reg.validate(mkEvent(30142, nil)); reject {
		t.Fatal("30142 should pass validation")
	}
	if reject, msg := reg.validate(mkEvent(30023, nil)); !reject || msg != "lf-rejected" {
		t.Fatalf("30023 validate = %v %q, want true lf-rejected", reject, msg)
	}
	if reject, msg := reg.validate(mkEvent(31337, nil)); !reject || msg != "kind not accepted" {
		t.Fatalf("unregistered kind = %v %q, want true 'kind not accepted'", reject, msg)
	}

	reg.store(mkEvent(30142, nil))
	reg.store(mkEvent(30023, nil))
	reg.store(mkEvent(31337, nil)) // no-op
	if storedAMB != 1 || storedLF != 1 {
		t.Fatalf("store counts = %d %d, want 1 1", storedAMB, storedLF)
	}
}

func TestRegistryKindsUnion(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}},
		contentType{kinds: []nostr.Kind{30023}},
	)
	got := reg.kinds()
	if len(got) != 2 || got[0] != 30142 || got[1] != 30023 {
		t.Fatalf("kinds() = %v, want [30142 30023]", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (sandbox disabled — Go cache is read-only in sandbox): `go test ./ -run 'TestRegistryValidateAndStoreDispatch|TestRegistryKindsUnion' -v`
Expected: FAIL — `undefined: newRegistry`, `undefined: contentType`.

- [ ] **Step 3: Write minimal implementation**

```go
// content_registry.go
package main

import (
	"iter"

	"fiatjaf.com/nostr"
)

// contentType is one content shape the relay serves. Its fields are exactly the
// per-kind variation the event path switched on before the registry existed —
// discovered from the AMB (30142) and long-form (30023) duplication. Projection
// and schema are deliberately absent: projection lives inside each store closure
// (AMB: the write buffer's PrepareBatch; long-form: storeLongform), schema is set
// once at backend Init, and neither was ever branched on per-event in main.go.
type contentType struct {
	kinds    []nostr.Kind
	validate func(nostr.Event) (reject bool, msg string)
	store    func(nostr.Event)
	fetch    fetchFunc
	count    func(nostr.Filter) (uint32, error)
	deleteID func(nostr.ID) error
}

// registry dispatches the event path across registered content types by kind.
// types is kept in registration order so fan-out (fetch/count/delete) is
// deterministic; byKind indexes into it for O(1) single-event dispatch.
type registry struct {
	types  []contentType
	byKind map[nostr.Kind]int
}

func newRegistry(types ...contentType) *registry {
	r := &registry{types: types, byKind: make(map[nostr.Kind]int, len(types))}
	for i, ct := range types {
		for _, k := range ct.kinds {
			r.byKind[k] = i
		}
	}
	return r
}

// kinds returns every registered kind, for NIP-11 retention advertisement.
func (r *registry) kinds() []nostr.Kind {
	var out []nostr.Kind
	for _, ct := range r.types {
		out = append(out, ct.kinds...)
	}
	return out
}

// validate dispatches to the owning content type. Unregistered kinds are the
// single source of "kind not accepted".
func (r *registry) validate(event nostr.Event) (reject bool, msg string) {
	i, ok := r.byKind[event.Kind]
	if !ok {
		return true, "kind not accepted"
	}
	return r.types[i].validate(event)
}

// store routes a validated event to the owning content type's write path.
func (r *registry) store(event nostr.Event) {
	if i, ok := r.byKind[event.Kind]; ok {
		r.types[i].store(event)
	}
}

var _ = iter.Seq[nostr.Event](nil) // iter used by fetch (Task 2)
```

> Note: remove the `var _ = iter.Seq...` line once `fetch` (Task 2) uses the `iter` import. It only keeps Task 1 compiling in isolation.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./ -run 'TestRegistryValidateAndStoreDispatch|TestRegistryKindsUnion' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add content_registry.go content_registry_test.go
git commit -m "feat(registry): contentType + single-event dispatch"
```

---

## Task 2: registry fan-out fetch (subsumes combinedFetch)

**Files:**
- Modify: `content_registry.go`
- Test: `content_registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
func mkFetch(events ...nostr.Event) fetchFunc {
	return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for _, e := range events {
				if !yield(e) {
					return
				}
			}
		}
	}
}

func collect(seq iter.Seq[nostr.Event]) []nostr.Event {
	var out []nostr.Event
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func TestRegistryFetchRoutingAndDedup(t *testing.T) {
	amb := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	lf := nostr.Event{ID: nostr.ID{2}, Kind: 30023}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(amb)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(lf)},
	)

	// kind-scoped: only the AMB backend.
	got := collect(reg.fetch(nostr.Filter{Kinds: []nostr.Kind{30142}}, 10))
	if len(got) != 1 || got[0].ID != amb.ID {
		t.Fatalf("30142 filter = %v, want [amb]", got)
	}
	// no kinds: both, in registration order.
	got = collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 2 || got[0].ID != amb.ID || got[1].ID != lf.ID {
		t.Fatalf("no-kind filter = %v, want [amb lf]", got)
	}
}

func TestRegistryFetchDedupAcrossBackends(t *testing.T) {
	dup := nostr.Event{ID: nostr.ID{7}, Kind: 30142}
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(dup)},
		contentType{kinds: []nostr.Kind{30023}, fetch: mkFetch(dup)}, // same id leaks in
	)
	got := collect(reg.fetch(nostr.Filter{}, 10))
	if len(got) != 1 {
		t.Fatalf("expected de-dup to 1 event, got %d", len(got))
	}
}

func TestRegistryFetchEarlyExit(t *testing.T) {
	a := nostr.Event{ID: nostr.ID{1}, Kind: 30142}
	b := nostr.Event{ID: nostr.ID{2}, Kind: 30142}
	reg := newRegistry(contentType{kinds: []nostr.Kind{30142}, fetch: mkFetch(a, b)})
	var seen int
	for range reg.fetch(nostr.Filter{}, 10) {
		seen++
		break // caller early-exit
	}
	if seen != 1 {
		t.Fatalf("early exit yielded %d, want 1", seen)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ -run 'TestRegistryFetch' -v`
Expected: FAIL — `reg.fetch undefined`.

- [ ] **Step 3: Write minimal implementation**

Delete the `var _ = iter.Seq...` placeholder line from Task 1, then add:

```go
// selected returns indices of the content types a filter targets: those owning
// any kind in filter.Kinds, or all types when filter.Kinds is empty (a
// kind-agnostic query, e.g. the chunk-rerank parent fetch or Negentropy).
func (r *registry) selected(filter nostr.Filter) []int {
	if len(filter.Kinds) == 0 {
		idx := make([]int, len(r.types))
		for i := range r.types {
			idx[i] = i
		}
		return idx
	}
	seen := make(map[int]bool)
	var idx []int
	for _, k := range filter.Kinds {
		if i, ok := r.byKind[k]; ok && !seen[i] {
			seen[i] = true
			idx = append(idx, i)
		}
	}
	return idx
}

// fetch is a fetchFunc fanning a filter out to every selected content type,
// merging events in registration order and de-duplicating by id. Caller
// early-exit is preserved: once yield returns false it stops pulling. With a
// single registered type this is an exact passthrough of that type's fetch.
func (r *registry) fetch(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		seen := make(map[nostr.ID]bool)
		for _, i := range r.selected(filter) {
			for ev := range r.types[i].fetch(filter, maxLimit) {
				if seen[ev.ID] {
					continue
				}
				seen[ev.ID] = true
				if !yield(ev) {
					return
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./ -run 'TestRegistryFetch' -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add content_registry.go content_registry_test.go
git commit -m "feat(registry): N-way fan-out fetch with de-dup and early-exit"
```

---

## Task 3: registry fan-out count + deleteEverywhere

**Files:**
- Modify: `content_registry.go`
- Test: `content_registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
import "errors"

func TestRegistryCountFanOut(t *testing.T) {
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, count: func(nostr.Filter) (uint32, error) { return 3, nil }},
		contentType{kinds: []nostr.Kind{30023}, count: func(nostr.Filter) (uint32, error) { return 5, nil }},
	)
	// no kinds: sum both.
	n, err := reg.count(nostr.Filter{})
	if err != nil || n != 8 {
		t.Fatalf("count(all) = %d %v, want 8 nil", n, err)
	}
	// kind-scoped: only AMB.
	n, err = reg.count(nostr.Filter{Kinds: []nostr.Kind{30142}})
	if err != nil || n != 3 {
		t.Fatalf("count(30142) = %d %v, want 3 nil", n, err)
	}
	// unowned kind: zero.
	n, err = reg.count(nostr.Filter{Kinds: []nostr.Kind{40000}})
	if err != nil || n != 0 {
		t.Fatalf("count(unowned) = %d %v, want 0 nil", n, err)
	}
}

func TestRegistryDeleteEverywhere(t *testing.T) {
	var hitAMB, hitLF int
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, deleteID: func(nostr.ID) error { hitAMB++; return nil }},
		contentType{kinds: []nostr.Kind{30023}, deleteID: func(nostr.ID) error { hitLF++; return nil }},
	)
	if err := reg.deleteEverywhere(nostr.ID{9}, nil); err != nil {
		t.Fatalf("deleteEverywhere err = %v", err)
	}
	if hitAMB != 1 || hitLF != 1 {
		t.Fatalf("delete hits = %d %d, want 1 1 (id carries no kind, so all collections tried)", hitAMB, hitLF)
	}
}

func TestRegistryDeleteEverywhereReturnsFirstErrorLogsRest(t *testing.T) {
	errAMB := errors.New("amb boom")
	errLF := errors.New("lf boom")
	var logged []error
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, deleteID: func(nostr.ID) error { return errAMB }},
		contentType{kinds: []nostr.Kind{30023}, deleteID: func(nostr.ID) error { return errLF }},
	)
	err := reg.deleteEverywhere(nostr.ID{9}, func(e error) { logged = append(logged, e) })
	if err != errAMB {
		t.Fatalf("first error = %v, want amb boom", err)
	}
	if len(logged) != 1 || logged[0] != errLF {
		t.Fatalf("logged = %v, want [lf boom]", logged)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ -run 'TestRegistryCount|TestRegistryDelete' -v`
Expected: FAIL — `reg.count undefined`, `reg.deleteEverywhere undefined`.

- [ ] **Step 3: Write minimal implementation**

```go
// count sums event counts across the content types a filter targets.
func (r *registry) count(filter nostr.Filter) (uint32, error) {
	var total uint32
	for _, i := range r.selected(filter) {
		n, err := r.types[i].count(filter)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// deleteEverywhere removes an id from every content type's search collection. A
// NIP-09/ban delete carries only the id, not the kind, so all collections are
// tried; per-collection delete-by-filter is idempotent (a miss is a 200/no-op).
// Returns the first error (types are tried in registration order, AMB first);
// any later errors go to logErr.
func (r *registry) deleteEverywhere(id nostr.ID, logErr func(error)) error {
	var firstErr error
	for i := range r.types {
		if err := r.types[i].deleteID(id); err != nil {
			if firstErr == nil {
				firstErr = err
			} else if logErr != nil {
				logErr(err)
			}
		}
	}
	return firstErr
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./ -run 'TestRegistryCount|TestRegistryDelete' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add content_registry.go content_registry_test.go
git commit -m "feat(registry): count fan-out + deleteEverywhere"
```

---

## Task 4: extract validateAMB / validateLongform

**Files:**
- Create: `validate.go`
- Test: `validate_test.go`

- [ ] **Step 1: Write the failing test**

```go
// validate_test.go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestValidateAMB(t *testing.T) {
	ok := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}, {"name", "Res"}}}
	if reject, _ := validateAMB(ok); reject {
		t.Fatal("valid AMB event rejected")
	}
	noD := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"name", "Res"}}}
	if reject, msg := validateAMB(noD); !reject || msg != "missing required 'd' tag" {
		t.Fatalf("missing d = %v %q", reject, msg)
	}
	noName := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}}}
	if reject, msg := validateAMB(noName); !reject || msg != "missing required 'name' tag" {
		t.Fatalf("missing name = %v %q", reject, msg)
	}
}

func TestValidateLongform(t *testing.T) {
	ok := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"d", "x"}, {"title", "T"}}}
	if reject, _ := validateLongform(ok); reject {
		t.Fatal("valid long-form event rejected")
	}
	noD := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"title", "T"}}}
	if reject, msg := validateLongform(noD); !reject || msg != "missing required 'd' tag" {
		t.Fatalf("missing d = %v %q", reject, msg)
	}
	noTitle := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"d", "x"}}}
	if reject, msg := validateLongform(noTitle); !reject || msg != "missing required 'title' tag" {
		t.Fatalf("missing title = %v %q", reject, msg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ -run 'TestValidateAMB|TestValidateLongform' -v`
Expected: FAIL — `undefined: validateAMB`, `undefined: validateLongform`.

- [ ] **Step 3: Write minimal implementation**

Copy the validation arms verbatim from `main.go`'s `OnEvent` switch (the `case 30142` and the d/title checks from `case 30023`, dropping the `!longformEnabled` guard — registration now gates enablement).

```go
// validate.go
package main

import "fiatjaf.com/nostr"

func validateAMB(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("name") {
		return true, "missing required 'name' tag"
	}
	return false, ""
}

func validateLongform(event nostr.Event) (reject bool, msg string) {
	if event.Tags.GetD() == "" {
		return true, "missing required 'd' tag"
	}
	if !event.Tags.Has("title") {
		return true, "missing required 'title' tag"
	}
	return false, ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./ -run 'TestValidateAMB|TestValidateLongform' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add validate.go validate_test.go
git commit -m "refactor: extract validateAMB/validateLongform as pure functions"
```

---

## Task 5: wire the registry into main.go's event path

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Confirm baseline green (records the regression set)**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS (note the set; golden + integration tests included).

- [ ] **Step 2: Build the registry after both backends are initialized**

Insert immediately after the `tsDB2` init block (`main.go` ~L201, after the `if longformEnabled { ... }` that sets up `tsDB2`) AND after `tsBuf` is constructed (it is currently at ~L229; move the registry build to after `tsBuf := NewTSWriteBuffer(...)` at ~L229 so the AMB `store` closure can capture `tsBuf`). Concretely, place this just below the `boltBuf` defer at ~L232:

```go
	// Content-type registry: one registration per kind the relay serves. AMB
	// is always present; long-form is appended only when enabled, so every
	// event-path dispatch below is kind-agnostic and flag-free. Adding a
	// content type = appending one contentType here.
	contentTypes := []contentType{
		{
			kinds:    []nostr.Kind{30142},
			validate: validateAMB,
			store:    func(e nostr.Event) { tsBuf.Queue(e) },
			fetch:    tsDB.QueryEvents,
			count:    tsDB.CountEvents,
			deleteID: tsDB.DeleteEvent,
		},
	}
	if longformEnabled && tsDB2 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{30023},
			validate: validateLongform,
			store:    func(e nostr.Event) { storeLongform(true, tsDB2, e) },
			fetch:    tsDB2.QueryEvents,
			count:    tsDB2.CountEvents,
			deleteID: tsDB2.DeleteEvent,
		})
	}
	reg := newRegistry(contentTypes...)
```

- [ ] **Step 3: Replace the retention build (L62–69)**

Replace:

```go
	// NIP-11: Retention
	retentionKinds := [][]int{{5}, {30142}}
	if longformEnabled {
		retentionKinds = append(retentionKinds, []int{30023})
	}
	relay.Info.Retention = []*nip11.RelayRetentionDocument{
		{Kinds: retentionKinds},
	}
```

This block is at L62–69, *before* the registry exists, so it cannot use `reg`. Leave the retention build where it is but keep it correct — it already produces `{5}` + `{30142}` (+ `{30023}` when flagged), which equals `{5}` ∪ `reg.kinds()`. **Do not change retention in this task** (the registry isn't built yet at L62). Leaving it is behavior-identical. (A later cleanup could move retention after the registry and derive it from `reg.kinds()`; out of scope here to keep the diff focused.)

> Rationale: retention is advertised in NIP-11 built at the top of `main`, long before backends/buffers exist. Deriving it from `reg` would require reordering a large amount of startup. The existing literal is already correct and is not duplication in the *event path* — it is one-time metadata. Skip it.

- [ ] **Step 4: Replace QueryStored fetch wiring (L309–319)**

Replace:

```go
		var fetch fetchFunc = tsDB.QueryEvents
		if longformEnabled && tsDB2 != nil {
			fetch = combinedFetch(tsDB.QueryEvents, tsDB2.QueryEvents)
		}
		return chunkRerankQuery(ctx, filter, chunkSearcher, fetch, maxLimit, relaySK)
```

with:

```go
		return chunkRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)
```

- [ ] **Step 5: Replace Count (L320–339)**

Replace the whole `relay.Count = func(...) { ... }` body with:

```go
	relay.Count = func(ctx context.Context, filter nostr.Filter) (uint32, error) {
		return reg.count(filter)
	}
```

- [ ] **Step 6: Replace StoreEvent + ReplaceEvent switches**

Replace:

```go
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, false)
		switch event.Kind {
		case 30142:
			tsBuf.Queue(event)
		case 30023:
			storeLongform(longformEnabled, tsDB2, event)
		}
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, true)
		switch event.Kind {
		case 30142:
			tsBuf.Queue(event)
		case 30023:
			storeLongform(longformEnabled, tsDB2, event)
		}
		return nil
	}
```

with:

```go
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, false)
		reg.store(event)
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		boltBuf.Queue(event, true)
		reg.store(event)
		return nil
	}
```

- [ ] **Step 7: Replace DeleteEvent (L343–355)**

Replace the `tsDB2`/`tsDB` delete tail with:

```go
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		boltDB.DeleteEvent(id)
		_ = contentStore.Delete(id.Hex()) // idempotent; safe when no content row existed
		// The id carries no kind, so try every collection (each delete is a
		// no-op when the id lives elsewhere).
		return reg.deleteEverywhere(id, func(err error) {
			fmt.Printf("delete %s (secondary collection): %v\n", id.Hex(), err)
		})
	}
```

- [ ] **Step 8: Replace the OnEvent kind switch (L373–393)**

Keep the pre-checks (ban pubkey/event, ACL) and the kind-5 early return exactly as-is. Replace only the trailing `switch event.Kind { ... }` block with:

```go
		return reg.validate(event)
```

So the tail of `OnEvent` reads:

```go
		if event.Kind == nostr.KindDeletion {
			return false, ""
		}
		return reg.validate(event)
	}
```

- [ ] **Step 9: Replace BanEvent delete tail**

In `relay.ManagementAPI.BanEvent`, replace the `tsDB2`/`tsDB` delete pair with the registry call, preserving the `mgmt.BanEvent` recording:

```go
	relay.ManagementAPI.BanEvent = func(ctx context.Context, id nostr.ID, reason string) error {
		boltDB.DeleteEvent(id)
		_ = contentStore.Delete(id.Hex()) // idempotent
		if err := reg.deleteEverywhere(id, func(e error) {
			fmt.Printf("ban-delete %s (secondary collection): %v\n", id.Hex(), e)
		}); err != nil {
			return err
		}
		return mgmt.BanEvent(id, reason)
	}
```

- [ ] **Step 10: Build, vet, and verify the regression set**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -20`
Expected: build clean, vet clean, **same** PASS set as Step 1 — `golden_test.go` (flag-off byte identity) and the cross-content integration test both green.

If the integration test fails to compile because it references `combinedFetch`, that is fixed in Task 6 — but to keep Step 10 green, temporarily leave `combinedFetch` in place (Task 6 removes it). If you prefer, complete Task 6's test edits before re-running Step 10.

- [ ] **Step 11: Confirm no kind literals remain in the event path**

Run: `grep -n '30142\|30023\|longformEnabled' main.go`
Expected: only the early retention block (L62–69), the backend-init blocks (`tsDB` collection env, `tsDB2` init ~L180–201), the NIP-AMB naddr (L49–51), the schema-status count (`tsDB.CountEvents(nostr.Filter{Kinds: []nostr.Kind{30142}})`, AMB-specific admin metric), and `ensureContentFields` (AMB-specific). **No `switch event.Kind` / per-kind `if` in StoreEvent/ReplaceEvent/DeleteEvent/Count/OnEvent/BanEvent/QueryStored.**

- [ ] **Step 12: Commit**

```bash
git add main.go
git commit -m "refactor(main): dispatch event path through content registry"
```

---

## Task 6: remove combinedFetch and adapt its tests

**Files:**
- Delete: `query_combined.go`
- Modify/Delete: `query_combined_test.go`
- Modify: `query_combined_integration_test.go`

- [ ] **Step 1: Point the integration test at the registry**

In `query_combined_integration_test.go`, replace the `combinedFetch(ambFetch, lfFetch)` construction with a registry built from the same fakes:

```go
	reg := newRegistry(
		contentType{kinds: []nostr.Kind{30142}, fetch: ambFetch},
		contentType{kinds: []nostr.Kind{30023}, fetch: lfFetch},
	)
	// ... drive chunkRerankQuery(ctx, filter, fakeSearcher, reg.fetch, maxLimit, relaySK)
```

Keep all assertions (both kinds returned, ordered by chunk score, each followed by its kind-21142 snippet with the correct `k` tag) unchanged.

- [ ] **Step 2: Fold or delete the combinedFetch unit test**

`query_combined_test.go` tests `combinedFetch`'s routing/de-dup/early-exit. That coverage now lives in `content_registry_test.go` (Task 2). Delete `query_combined_test.go`. (Its scenarios are already covered by `TestRegistryFetchRoutingAndDedup`, `TestRegistryFetchDedupAcrossBackends`, `TestRegistryFetchEarlyExit`.)

- [ ] **Step 3: Delete combinedFetch**

```bash
git rm query_combined.go query_combined_test.go
```

- [ ] **Step 4: Build, vet, full suite**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -20`
Expected: clean build/vet; full suite green including golden + integration.

- [ ] **Step 5: Verify standalone (Docker-path) build**

Run: `GOWORK=off go build .`
Expected: success against the pinned nostrlib pseudo-version.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "refactor: remove combinedFetch (subsumed by registry.fetch)"
```

---

## Phase 3 exit criteria

- No `switch event.Kind` / per-kind `if` branches remain in `main.go`'s event path (StoreEvent, ReplaceEvent, DeleteEvent, Count, OnEvent, BanEvent, QueryStored). Verified by Task 5 Step 11.
- Adding a content type = appending one `contentType` to the registry slice.
- `golden_test.go` (AMB projection + snippet byte output) unchanged and green — flag-off behavior provably identical (single registration → passthrough).
- Cross-content integration test green with the flag on.
- `combinedFetch` removed; its behavior preserved by `registry.fetch`.
- Standalone (`GOWORK=off`) build passes.

## Out of scope (later phases)

- Promoting the registry to nostrlib (stays in `package main` until ≥3 content types prove the shape — Phase 4 adds wiki).
- Generalizing schema/projection/embedding/reindex/NIP-86-schema machinery (AMB-specific, not duplicated; not part of this extraction).
- Retention build reorder (one-time metadata, not event-path duplication).
- amb-indexer changes (none — chunking is the indexer's concern).

## Self-review notes

- **Spec coverage:** The sketch's Phase-3 deliverables map as follows: "interface bundling what varies" → `contentType` struct (narrowed to the *actual* event-path variation; rationale documented above); "registry replacing the `if event.Kind` branches" → Tasks 5–6; "AMB projector #1, golden-locked" → golden test held green throughout. Deliverables `Project/Schema/QueryMap/ChunkSource` are intentionally excluded with written justification (not duplicated in the relay event path; `ChunkSource` is amb-indexer's concern).
- **Placeholder scan:** every step carries real code or an exact source-line reference; no TODO/TBD.
- **Type consistency:** `contentType`, `registry`, `newRegistry`, `reg.kinds/validate/store/selected/fetch/count/deleteEverywhere`, `validateAMB`, `validateLongform`, `fetchFunc` used identically across Tasks 1–6.
