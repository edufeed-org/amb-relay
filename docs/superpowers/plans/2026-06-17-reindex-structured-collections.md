# Generalize Reindex Across All Content Types Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the NIP-86 `reindex` rebuild the long-form (`longform_30023`) and wiki (`wiki_30818`) structured collections from BoltDB truth — not just the AMB (`30142`) collection — closing the known desync that currently makes a reindex silently leave those collections stale.

**Architecture:** The reindexer keeps its existing AMB-specific path (schema recreate → batch upsert → content patch/orphan GC) untouched. After it finishes AMB, it loops over a new slice of `structuredReindexTarget` values (one per enabled structured type, built in `main.go` where `tsDB2`/`tsDB3` + their schemas already live). Each target drops+recreates its collection and reprojects every BoltDB event of its kinds via a synchronous, error-returning helper. When `LONGFORM_ENABLED`/`WIKI_ENABLED` are off the slice is empty, so reindex behaves byte-for-byte as today.

**Tech Stack:** Go, `fiatjaf.com/nostr` (khatru relay + `eventstore/typesense30142` + `eventstore/boltdb`), Typesense, BoltDB.

**Why this is correct:** Structured docs are keyed by `GenerateDocumentID(pubkey, d)` (stable per addressable coordinate), identical to how AMB keys docs — so drop+reproject yields exactly the live-write state. Reprojection reuses the SAME projection funcs (`nostrToLongform`/`nostrToWiki`) and upsert path (`upsertStructuredDoc`) as the live write path, so a reindexed doc is byte-identical to a freshly-stored one. Recreating the collection first clears orphans (parity with AMB's orphan GC).

**Scope / constraints:**
- **Local work only.** No nostrlib change needed (everything here is amb-relay `package main`). No push, no deploy, no `.env`. Local commits only on branch `dev`.
- **Every `go`/`git` command runs with `dangerouslyDisableSandbox: true`** (sandbox is read-only).
- Backward-compat invariant: with both flags off, `Reindexer.run()` does exactly what it does today (the new loop iterates an empty slice).

---

## File Structure

- **`structured.go`** (modify) — add `reprojectStructured[T]` (error-returning project+upsert); refactor the existing fire-and-forget `storeStructured` to delegate to it (DRY: both do project→upsert; the live path swallows the error, the reindex path propagates it).
- **`reindex.go`** (modify) — add `structuredReindexTarget` type; add a `structured []structuredReindexTarget` field to `Reindexer`; extend `NewReindexer` to accept it; add the pure, unit-testable `reindexStructuredEvents` loop + a `reindexStructured` method; call the loop at the end of `run()`.
- **`main.go`** (modify) — build `structuredTargets` from `tsDB2`/`tsDB3` (guarded by the existing `longformEnabled`/`wikiEnabled` + non-nil checks) and pass it into `NewReindexer`.
- **`reindex_test.go`** (create) — TDD unit tests for `reindexStructuredEvents` (counts + continue-on-error + order) and `reprojectStructured` (project-error path without HTTP; happy path via `httptest`, mirroring `structured_test.go`).

---

## Task 1: Error-returning structured reproject helper

**Files:**
- Modify: `structured.go`
- Test: `reindex_test.go` (create)

- [ ] **Step 1: Write the failing tests**

Create `reindex_test.go`:

```go
package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// reprojectStructured returns the project error (no HTTP attempted) when the
// event cannot be projected — a long-form event without a d-tag.
func TestReprojectStructured_ProjectErrorNoHTTP(t *testing.T) {
	evt := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"title", "x"}}}
	evt.ID = evt.GetID()
	if err := reprojectStructured(nil, evt, nostrToLongform); err == nil {
		t.Fatal("expected project error for missing d-tag, got nil")
	}
}

// reprojectStructured upserts the projected doc and returns nil on a 200.
func TestReprojectStructured_HappyPath(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "c", ApiKey: "k"}

	evt := nostr.Event{
		Kind:   30023,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"),
		Tags:   nostr.Tags{{"d", "a"}, {"title", "T"}},
	}
	evt.ID = evt.GetID()
	if err := reprojectStructured(ts, evt, nostrToLongform); err != nil {
		t.Fatalf("reprojectStructured: %v", err)
	}
	if !strings.Contains(string(gotBody), `"d":"a"`) {
		t.Errorf("upsert body missing projected d: %s", gotBody)
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail to compile**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go test ./ -run TestReprojectStructured 2>&1 | tail -15` (dangerouslyDisableSandbox:true)
Expected: FAIL — `undefined: reprojectStructured`.

- [ ] **Step 3: Add `reprojectStructured` and refactor `storeStructured` in `structured.go`**

Add after `upsertStructuredDoc`:

```go
// reprojectStructured projects an addressable event and synchronously upserts
// it, returning any error. Unlike storeStructured (fire-and-forget for the live
// write path), the error is propagated so the reindexer can count failures.
func reprojectStructured[T any](ts *typesense30142.TSBackend, event nostr.Event, project func(*nostr.Event) (*T, error)) error {
	doc, err := project(&event)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := upsertStructuredDoc(ts, doc); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}
```

Replace the body of `storeStructured` to delegate (keeps the enabled/nil guard and fire-and-forget logging):

```go
func storeStructured[T any](enabled bool, ts *typesense30142.TSBackend, event nostr.Event, label string, project func(*nostr.Event) (*T, error)) {
	if !enabled || ts == nil {
		return
	}
	if err := reprojectStructured(ts, event, project); err != nil {
		fmt.Printf("%s %s: %v\n", label, event.ID.Hex(), err)
	}
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go test ./ -run TestReprojectStructured -v 2>&1 | tail -15` (dangerouslyDisableSandbox:true)
Expected: PASS for both `TestReprojectStructured_ProjectErrorNoHTTP` and `TestReprojectStructured_HappyPath`.

- [ ] **Step 5: Confirm the structured-collection live-path tests still pass**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go test ./ -run 'TestStructured|TestUpsertStructured|TestLongform|TestWiki|TestNewStructured' 2>&1 | tail -10` (dangerouslyDisableSandbox:true)
Expected: PASS (the `storeStructured` refactor preserves behavior; `TestLongformDocumentJSONStable` stays green).

- [ ] **Step 6: Commit**

```bash
git -C /home/laoc/coding/edufeed/amb-relay add structured.go reindex_test.go
git -C /home/laoc/coding/edufeed/amb-relay commit -m "refactor(structured): add error-returning reprojectStructured helper"
```

---

## Task 2: Pure structured-reindex loop + reindexer wiring

**Files:**
- Modify: `reindex.go`
- Test: `reindex_test.go` (append)

- [ ] **Step 1: Write the failing test for the pure loop**

Append to `reindex_test.go`:

```go
// reindexStructuredEvents reprojects every event, counts results, and continues
// past a failing event (parity with the AMB batch path's per-error counting).
func TestReindexStructuredEvents_CountsAndContinues(t *testing.T) {
	events := []nostr.Event{
		{Kind: 30023, Tags: nostr.Tags{{"d", "a"}}},
		{Kind: 30023, Tags: nostr.Tags{{"d", "bad"}}},
		{Kind: 30023, Tags: nostr.Tags{{"d", "c"}}},
	}
	for i := range events {
		events[i].ID = events[i].GetID()
	}
	seq := func(yield func(nostr.Event) bool) {
		for _, e := range events {
			if !yield(e) {
				return
			}
		}
	}
	var seen []string
	reproject := func(e nostr.Event) error {
		d := e.Tags.GetD()
		seen = append(seen, d)
		if d == "bad" {
			return errors.New("boom")
		}
		return nil
	}
	total, indexed, errs := reindexStructuredEvents("longform", seq, reproject)
	if total != 3 || indexed != 2 || errs != 1 {
		t.Fatalf("total=%d indexed=%d errs=%d, want 3/2/1", total, indexed, errs)
	}
	if !slices.Equal(seen, []string{"a", "bad", "c"}) {
		t.Errorf("reproject call order = %v, want [a bad c]", seen)
	}
}
```

- [ ] **Step 2: Run it, verify it fails to compile**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go test ./ -run TestReindexStructuredEvents 2>&1 | tail -15` (dangerouslyDisableSandbox:true)
Expected: FAIL — `undefined: reindexStructuredEvents`.

- [ ] **Step 3: Add the type, loop, method, and wiring in `reindex.go`**

Add `iter` to the import block. Add the target type near the top (after the `const` block):

```go
// structuredReindexTarget describes one structured (long-form/wiki) collection
// to rebuild during a reindex: drop+recreate it, then reproject every BoltDB
// event of its kinds. recreate/reproject are closures so reindex.go stays free
// of per-kind schema/projection detail (they are built in main.go beside the
// backends).
type structuredReindexTarget struct {
	label     string
	kinds     []nostr.Kind
	recreate  func() error
	reproject func(nostr.Event) error
}
```

Add a field to `Reindexer` (after `content *ContentStore`):

```go
	structured []structuredReindexTarget
```

Extend `NewReindexer` to accept and set it:

```go
func NewReindexer(tsDB *typesense30142.TSBackend, boltDB *boltdb.BoltBackend, mgmt *ManagementStore, content *ContentStore, structured []structuredReindexTarget) *Reindexer {
	return &Reindexer{
		tsDB:       tsDB,
		boltDB:     boltDB,
		mgmt:       mgmt,
		content:    content,
		structured: structured,
	}
}
```

Add the pure loop + method (place after `run()`):

```go
// reindexStructuredEvents reprojects each event, returning (total, indexed,
// errs). Pure over its inputs so it is unit-testable without live Typesense or
// BoltDB. Continues past a failing event, mirroring the AMB batch path.
func reindexStructuredEvents(label string, events iter.Seq[nostr.Event], reproject func(nostr.Event) error) (total, indexed, errs int64) {
	for event := range events {
		total++
		if err := reproject(event); err != nil {
			log.Printf("reindex: %s reproject %s failed: %v", label, event.ID.Hex(), err)
			errs++
			continue
		}
		indexed++
	}
	return
}

// reindexStructured drops+recreates a structured collection then reprojects all
// its BoltDB events into the shared reindex counters.
func (r *Reindexer) reindexStructured(t structuredReindexTarget) {
	if err := t.recreate(); err != nil {
		log.Printf("reindex: recreate %s collection failed: %v", t.label, err)
		r.errors.Add(1)
		return
	}
	total, indexed, errs := reindexStructuredEvents(
		t.label,
		r.boltDB.QueryEvents(nostr.Filter{Kinds: t.kinds}, reindexMaxEvents),
		t.reproject,
	)
	r.total.Add(total)
	r.indexed.Add(indexed)
	r.errors.Add(errs)
}
```

In `run()`, immediately BEFORE the final `log.Printf("reindex: completed...")`, insert:

```go
	// Rebuild every enabled structured (long-form/wiki) collection from BoltDB
	// truth. Empty when LONGFORM/WIKI are disabled, so reindex is byte-for-byte
	// unchanged in that case.
	for _, t := range r.structured {
		r.reindexStructured(t)
	}
```

- [ ] **Step 4: Run the loop test, verify it passes**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go test ./ -run TestReindexStructuredEvents -v 2>&1 | tail -10` (dangerouslyDisableSandbox:true)
Expected: PASS.

- [ ] **Step 5: Update the `NewReindexer` call in `main.go`**

Just before the `reindexer := NewReindexer(...)` line (~`main.go:329`), build the targets (the `longformEnabled`/`wikiEnabled` vars and `tsDB2`/`tsDB3` are already in scope):

```go
	var structuredTargets []structuredReindexTarget
	if longformEnabled && tsDB2 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "longform",
			kinds:     []nostr.Kind{30023},
			recreate:  func() error { return tsDB2.RecreateCollection(tsDB2.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB2, e, nostrToLongform) },
		})
	}
	if wikiEnabled && tsDB3 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:     "wiki",
			kinds:     []nostr.Kind{30818},
			recreate:  func() error { return tsDB3.RecreateCollection(tsDB3.Schema) },
			reproject: func(e nostr.Event) error { return reprojectStructured(tsDB3, e, nostrToWiki) },
		})
	}
```

Change the call to:

```go
	reindexer := NewReindexer(&tsDB, &boltDB, &mgmt, contentStore, structuredTargets)
```

- [ ] **Step 6: Build, full suite, vet**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go build . && go test ./... 2>&1 | tail -15 && go vet ./... 2>&1 | tail -10` (dangerouslyDisableSandbox:true)
Expected: build clean; ALL tests pass (including the new reindex tests and the untouched golden tests); vet clean.

- [ ] **Step 7: Commit**

```bash
git -C /home/laoc/coding/edufeed/amb-relay add reindex.go reindex_test.go main.go
git -C /home/laoc/coding/edufeed/amb-relay commit -m "feat(reindex): rebuild longform + wiki collections from BoltDB, not just AMB"
```

---

## Task 3: Docs + memory

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update the reindex description in `CLAUDE.md`**

In the "Reindexer" bullet (under Architecture) and the Typesense Schema Management section, replace any "rebuilds only AMB" framing with: reindex now drops+recreates and reprojects every enabled content type's collection (AMB `30142`, long-form `longform_30023`, wiki `wiki_30818`) from BoltDB. Note the AMB path keeps its content-patch/orphan-GC step; structured types are drop+reproject.

- [ ] **Step 2: Commit the docs**

```bash
git -C /home/laoc/coding/edufeed/amb-relay add CLAUDE.md
git -C /home/laoc/coding/edufeed/amb-relay commit -m "docs(reindex): document multi-collection reindex"
```

- [ ] **Step 3: Retire the deferred-follow-up memory**

The memory `project_reindex_followup.md` recorded this as a deferred blocker. Once implemented, update it to "resolved on 2026-06-17" (or remove it and its MEMORY.md index line), so future sessions do not treat it as outstanding.

---

## Exit criteria

- A NIP-86 `reindex` with `LONGFORM_ENABLED`/`WIKI_ENABLED` on rebuilds `longform_30023` and `wiki_30818` from BoltDB, keyed identically to the live write path.
- With both flags off, `Reindexer.run()` is behaviorally unchanged (empty target slice).
- `go build`, full `go test ./...`, and `go vet ./...` are green; the golden snippet/AMB tests are untouched and pass.
- The deferred-follow-up memory is marked resolved.

## Self-Review Checklist

1. **Backward-compat:** both flags off → `structuredTargets` empty → new `run()` loop is a no-op; existing AMB path (recreate→batch→content patch/orphan GC) is untouched. ✓
2. **Type consistency:** `reprojectStructured[T]` reused by both `storeStructured` (live) and `main.go` target closures; `reindexStructuredEvents` returns `int64` matching the `atomic.Int64` counters; `RecreateCollection(*CollectionSchema)` fed `tsDB2.Schema`/`tsDB3.Schema` (both `*CollectionSchema`). ✓
3. **Correctness of keying:** structured docs keyed by `GenerateDocumentID(pubkey,d)` — drop+reproject reproduces live state; no multi-version concern beyond what AMB already relies on (BoltDB holds the latest addressable version). ✓
4. **Placeholder scan:** every step has concrete code/commands. ✓
