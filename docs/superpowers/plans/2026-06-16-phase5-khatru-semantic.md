# Phase 5: Extract `khatru/semantic` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Promote the chunk-rerank + kind-21142 snippet layer (`chunk_rerank.go`, `chunk_snippet.go`, `chunk_searcher_http.go`) out of `amb-relay/package main` into a reusable `nostrlib/khatru/semantic` package, now that three content types (AMB 30142, long-form 30023, wiki 30818) consume it — the rule-of-three on the semantic layer is met.

**Architecture:** The three files form a self-contained unit depending only on `fiatjaf.com/nostr` + stdlib. The seam is already clean: `ChunkSearcher` is an interface, the parent fetch is passed as a function, snippet signing takes a `nostr.SecretKey`. Move the files verbatim (changing only `package main` → `package semantic` and exporting the symbols amb-relay calls across the new package boundary), then rewire amb-relay's `main.go` + tests to import `fiatjaf.com/nostr/khatru/semantic`. The one entanglement: `fetchFunc` is also used by `content_registry.go`, so it STAYS in amb-relay; `semantic.ChunkRerankQuery` takes the bare function-literal type instead of a second named type (avoids cross-package named-type assignability friction). This mirrors the Phase 1 `relaykit` extraction exactly.

**Tech Stack:** Go, `fiatjaf.com/nostr` (module path of nostrlib), khatru, go.work (local dev resolves `fiatjaf.com/nostr v0.0.0 => ./nostrlib`), pinned pseudo-version in `amb-relay/go.mod` for Docker.

**Scope / constraints:**
- **Local verification only.** go.work makes amb-relay build/test against `./nostrlib` immediately after committing in nostrlib — NO push needed to verify. The nostrlib push + `bump-nostrlib.sh` go.mod pin update (Docker/server path) is the FINAL task and **requires explicit user authorization** (mirrors Phase 1 Task 7, which was an authorized push). Do all local work and local commits; STOP before pushing/bumping and ask.
- **Golden byte-identity:** `golden_test.go` (in amb-relay) locks the kind-21142 snippet output via `buildSnippetEvent`. After the move it must call `semantic.BuildSnippetEvent` and STAY byte-identical. This is the regression guard for the whole extraction.
- Do NOT use `--no-verify`. Do NOT commit `.env`/secrets. Branches `dev` (amb-relay), the nostrlib branch — local commits only until authorized.
- **Every `go`/`git` command runs with `dangerouslyDisableSandbox: true`** (sandbox is read-only). Use `GOWORK=off` only for the standalone-build checks; local dev/test uses the default workspace (go.work) so it sees `./nostrlib`.

---

## Symbol export map (the only API surface crossing the new boundary)

| amb-relay (package main, today) | semantic package (after) | Why it must export |
|---|---|---|
| `chunkRerankQuery` | `ChunkRerankQuery` | called in `main.go:376` + integration test |
| `newHTTPChunkSearcher` | `NewHTTPChunkSearcher` | called in `main.go:365` |
| `ChunkSearcher` (iface) | `ChunkSearcher` (already exported) | `main.go:355` var type |
| `ChunkHit` (struct) | `ChunkHit` (already exported) | `golden_test.go`, integration test |
| `buildSnippetEvent` | `BuildSnippetEvent` | `golden_test.go:57` |
| `kindSearchSnippet` const | `KindSearchSnippet` | integration test references kind 21142 |
| `httpChunkSearcher` (struct) | stays unexported | only the constructor + iface escape |
| `parentKind` | stays unexported | only used inside `snippet.go` |
| `fetchFunc` (type) | **stays in amb-relay** | also used by `content_registry.go` |

`semantic.ChunkRerankQuery`'s fetch parameter is the bare literal type `func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]` — NOT a named `semantic.FetchFunc`. A method value (`reg.fetch`) and amb-relay's named `fetchFunc` are both assignable to a literal-typed parameter; a second named type would reject the named `fetchFunc` across packages.

---

## File Structure

**nostrlib (new package `fiatjaf.com/nostr/khatru/semantic`):**
- Create: `nostrlib/khatru/semantic/rerank.go` ← from `amb-relay/chunk_rerank.go` (minus the `fetchFunc` type def, which stays in amb-relay).
- Create: `nostrlib/khatru/semantic/snippet.go` ← from `amb-relay/chunk_snippet.go`.
- Create: `nostrlib/khatru/semantic/searcher_http.go` ← from `amb-relay/chunk_searcher_http.go`.
- Create: `nostrlib/khatru/semantic/rerank_test.go` ← from `amb-relay/chunk_rerank_test.go`.
- Create: `nostrlib/khatru/semantic/snippet_test.go` ← from `amb-relay/chunk_snippet_test.go`.
- Create: `nostrlib/khatru/semantic/searcher_http_test.go` ← from `amb-relay/chunk_searcher_http_test.go`.

**amb-relay:**
- Delete: `chunk_rerank.go`, `chunk_snippet.go`, `chunk_searcher_http.go`, `chunk_rerank_test.go`, `chunk_snippet_test.go`, `chunk_searcher_http_test.go`.
- Modify: `content_registry.go` — add the relocated `fetchFunc` type definition.
- Modify: `main.go` — import semantic; `semantic.ChunkSearcher`, `semantic.NewHTTPChunkSearcher`, `semantic.ChunkRerankQuery`.
- Modify: `golden_test.go` — `semantic.ChunkHit`, `semantic.BuildSnippetEvent`.
- Modify: `query_combined_integration_test.go` — `semantic.ChunkHit`, `semantic.KindSearchSnippet`, `semantic.ChunkRerankQuery`; ensure its `fakeChunkSearcher` returns `[]semantic.ChunkHit` and its fetch is assignable.
- Modify: `go.mod`/`go.sum` — (FINAL, authorized step only) pin bump for Docker.

---

## Task 1: Stand up the `semantic` package in nostrlib (move the three files + tests)

**Files:** the 6 creates under `nostrlib/khatru/semantic/`; the 6 deletes under `amb-relay/`.

- [ ] **Step 1: Create the package directory and move the three source files**

Copy each source file to its new home, then in each: change `package main` → `package semantic`, and apply the export renames from the table. Run these as shell copies (then edit), or recreate via the editor:

- `nostrlib/khatru/semantic/rerank.go`: body of `chunk_rerank.go` EXCEPT delete the `fetchFunc` type declaration (lines 33-35). Rename `func chunkRerankQuery` → `func ChunkRerankQuery`; change its `fetch fetchFunc` parameter to `fetch func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]`. Internal references `kindSearchSnippet` → `KindSearchSnippet`, `buildSnippetEvent` → `BuildSnippetEvent`. `ChunkHit`/`ChunkSearcher` keep their names (already exported).
- `nostrlib/khatru/semantic/snippet.go`: body of `chunk_snippet.go`. Rename `kindSearchSnippet` → `KindSearchSnippet`, `buildSnippetEvent` → `BuildSnippetEvent`. `parentKind` stays lowercase.
- `nostrlib/khatru/semantic/searcher_http.go`: body of `chunk_searcher_http.go`. Rename `newHTTPChunkSearcher` → `NewHTTPChunkSearcher`. `httpChunkSearcher` stays lowercase (exported constructor returns `*httpChunkSearcher`, consumed via the `ChunkSearcher` interface).

- [ ] **Step 2: Move the three test files**

Copy each `_test.go` to the new package, change `package main` → `package semantic`, and update every reference to the renamed symbols (`ChunkRerankQuery`, `BuildSnippetEvent`, `KindSearchSnippet`, `NewHTTPChunkSearcher`). If a moved test passes a fetch into `ChunkRerankQuery`, it must be an inline closure or a value whose type is assignable to the literal func param (a `nostr.Filter`→`iter.Seq` closure is unnamed → fine). Watch for any test helper (e.g. a `fakeChunkSearcher`) that ALSO exists in `query_combined_integration_test.go`; the copy in the semantic package is independent (different package, no collision), but make sure the one you move actually has its dependencies in-package.

- [ ] **Step 3: Delete the six amb-relay originals**

```bash
git -C /home/laoc/coding/edufeed/amb-relay rm chunk_rerank.go chunk_snippet.go chunk_searcher_http.go chunk_rerank_test.go chunk_snippet_test.go chunk_searcher_http_test.go
```

(amb-relay will not compile until Task 2 — that's expected.)

- [ ] **Step 4: Verify the semantic package builds and its tests pass in isolation**

Run: `cd /home/laoc/coding/edufeed/nostrlib && go test ./khatru/semantic/ -v 2>&1 | tail -40` (dangerouslyDisableSandbox:true)
Expected: the moved rerank/snippet/searcher tests PASS in their new package. Fix compile errors (missing exports, leftover `fetchFunc` references) until green.

- [ ] **Step 5: Commit the new package in nostrlib**

```bash
git -C /home/laoc/coding/edufeed/nostrlib add khatru/semantic/
git -C /home/laoc/coding/edufeed/nostrlib commit -m "feat(semantic): extract chunk-rerank + snippet layer from amb-relay"
```

---

## Task 2: Rewire amb-relay onto the semantic package

**Files:** `content_registry.go`, `main.go`, `golden_test.go`, `query_combined_integration_test.go`.

- [ ] **Step 1: Relocate `fetchFunc` into amb-relay's registry file**

`fetchFunc` left with `chunk_rerank.go` but `content_registry.go` still needs it. Add to `content_registry.go` (near the top, after imports — it already imports `iter` and `nostr`):

```go
// fetchFunc abstracts a content type's event query (e.g. tsDB.QueryEvents) so
// the registry and the semantic rerank layer can consume it without Typesense.
type fetchFunc func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]
```

- [ ] **Step 2: Add the semantic import and update `main.go` call sites**

Add `"fiatjaf.com/nostr/khatru/semantic"` to `main.go` imports. Then:
- `main.go:355`: `var chunkSearcher semantic.ChunkSearcher`
- `main.go:365`: `chunkSearcher = semantic.NewHTTPChunkSearcher(indexerURL, token)`
- `main.go:376`: `return semantic.ChunkRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)`

`reg.fetch` is a method value (unnamed type) → assignable to the literal fetch param. No other main.go changes.

- [ ] **Step 3: Update `golden_test.go`**

`golden_test.go:48` `ChunkHit{...}` → `semantic.ChunkHit{...}`; `golden_test.go:57` `buildSnippetEvent(sk, hit)` → `semantic.BuildSnippetEvent(sk, hit)`. Add the semantic import. The asserted byte output must be UNCHANGED — do not touch the expected-bytes literal.

- [ ] **Step 4: Update `query_combined_integration_test.go`**

`ChunkHit` → `semantic.ChunkHit` (line 59), `kindSearchSnippet` → `semantic.KindSearchSnippet` (lines 66, 102), `chunkRerankQuery(...)` → `semantic.ChunkRerankQuery(...)` (line 69). Ensure the local `fakeChunkSearcher` returns `[]semantic.ChunkHit` and satisfies `semantic.ChunkSearcher`, and that the `fetch` passed at line 69 is assignable to the literal func param (it is constructed locally — keep it an unnamed closure or `fetchFunc`-typed value; both assign to a literal param). Add the semantic import.

- [ ] **Step 5: Build + full suite + vet (local, via go.work → ./nostrlib)**

Run: `cd /home/laoc/coding/edufeed/amb-relay && go build . && go test ./... 2>&1 | tail -25 && go vet ./... 2>&1 | tail -10` (dangerouslyDisableSandbox:true)
Expected: build clean, ALL tests pass — crucially `TestGoldenSnippetEvent`/`TestGoldenAMBProjection` stay green (byte-identity through the moved `semantic.BuildSnippetEvent`), and the cross-content integration test passes against `semantic.ChunkRerankQuery`. vet clean.

- [ ] **Step 6: Commit amb-relay rewire (local)**

```bash
git -C /home/laoc/coding/edufeed/amb-relay add content_registry.go main.go golden_test.go query_combined_integration_test.go chunk_rerank.go chunk_snippet.go chunk_searcher_http.go chunk_rerank_test.go chunk_snippet_test.go chunk_searcher_http_test.go
git -C /home/laoc/coding/edufeed/amb-relay commit -m "refactor(amb-relay): consume khatru/semantic for chunk-rerank + snippets"
```

(The deleted files are staged via `git rm` in Task 1 Step 3; the add list above re-confirms the deletions are part of this commit.)

---

## Task 3: Pin nostrlib for Docker builds — AUTHORIZED STEP ONLY

**Do NOT run this task without explicit user authorization to push nostrlib.** Local dev already works via go.work; this task only updates the Docker/server pin. STOP and ask before proceeding.

**Files:** `amb-relay/go.mod`, `amb-relay/go.sum` (and any other sibling consumer the bump script touches — though only amb-relay consumes `semantic`).

- [ ] **Step 1 (authorized): Push nostrlib and bump consumers**

```bash
git -C /home/laoc/coding/edufeed/nostrlib push   # to the edufeed branch
cd /home/laoc/coding/edufeed && ./bump-nostrlib.sh
```

The script resolves the new pseudo-version against the remote `edufeed` branch tip, updates each consumer's `replace` pin, and verifies `GOWORK=off go build ./...`. amb-relay must build green against the new pin (it now references `fiatjaf.com/nostr/khatru/semantic`, which only exists in the pushed nostrlib).

- [ ] **Step 2 (authorized): Verify the standalone (Docker-path) build**

Run: `cd /home/laoc/coding/edufeed/amb-relay && GOWORK=off go build . && GOWORK=off go test ./... 2>&1 | tail -10` (dangerouslyDisableSandbox:true)
Expected: builds and tests pass against the pinned pseudo-version (not go.work).

- [ ] **Step 3 (authorized): Commit the pin bump**

```bash
git -C /home/laoc/coding/edufeed/amb-relay add go.mod go.sum
git -C /home/laoc/coding/edufeed/amb-relay commit -m "chore(deps): bump nostrlib for khatru/semantic"
```

---

## Phase 5 exit criteria

- `chunk_rerank.go` / `chunk_snippet.go` / `chunk_searcher_http.go` no longer exist in amb-relay; their logic lives in `fiatjaf.com/nostr/khatru/semantic`.
- amb-relay builds (go.work locally; standalone after authorized pin bump) and the full suite passes, including the golden snippet byte-identity test through `semantic.BuildSnippetEvent`.
- The semantic package's own tests pass in nostrlib.
- The nostrlib push + go.mod pin bump are done ONLY after explicit user authorization.

## Self-Review Checklist

1. **Spec coverage:** all three semantic source files + their tests moved (6 files); every cross-boundary symbol in the export-map table is exported and rewired at its amb-relay call site; `fetchFunc` correctly retained in amb-relay for the registry.
2. **Type consistency:** `ChunkRerankQuery` fetch param is the literal func type (not a named `FetchFunc`); `reg.fetch` (method value) and amb-relay `fetchFunc` both assignable to it; `fakeChunkSearcher` returns `[]semantic.ChunkHit` and satisfies `semantic.ChunkSearcher`.
3. **Byte-identity guard:** `golden_test.go` snippet assertion unchanged; only the symbol qualifier (`semantic.`) added.
4. **Authorization gate:** Task 3 (push + bump) is fenced behind explicit user go-ahead; Tasks 1-2 are fully local and reversible.
