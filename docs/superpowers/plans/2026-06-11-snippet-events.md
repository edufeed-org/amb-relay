# Ephemeral Search-Snippet Events (kind 21142) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a client opts in via `kinds:[30142,21142]` on a NIP-50 search, interleave a relay-signed ephemeral kind-21142 event after each chunk-reranked result, carrying the best matching passage, score, and source locators.

**Architecture:** A new pure function `buildSnippetEvent` (chunk_snippet.go) turns a `ChunkHit` into a signed event. `ChunkHit` grows snippet/locator fields, parsed from the existing `/search_chunks` response (zero indexer changes). `chunkRerankQuery` keeps the best *hit* (not just score) per parent and, when the client opted in, yields the snippet event directly after its parent. `main.go` loads `RELAY_SECKEY` (or generates a per-boot key) and passes it through.

**Tech Stack:** Go, nostrlib (`fiatjaf.com/nostr`), khatru relay framework. Spec: `docs/superpowers/specs/2026-06-11-snippet-events-design.md`.

---

## Context for workers with zero codebase knowledge

- **Build/test commands** (sandbox needs an explicit GOCACHE):
  ```bash
  cd /home/laoc/coding/edufeed/amb-relay
  GOCACHE=/tmp/claude-1000/go-cache go test .          # full suite
  GOCACHE=/tmp/claude-1000/go-cache go test -run <Name> -v .   # one test
  ```
  The parent `go.work` resolves `fiatjaf.com/nostr` to the local `../nostrlib` checkout automatically.
- **Relevant nostrlib API** (already verified in source):
  - `nostr.Generate() SecretKey`, `nostr.SecretKeyFromHex(string) (SecretKey, error)`
  - `nostr.GetPublicKey(sk) PubKey`, `(PubKey).Hex() string`, `(ID).Hex() string`
  - `(*Event).Sign(sk) error`, `(Event).VerifySignature() bool`
  - `nostr.Now() Timestamp`, `nostr.Kind(21142)`, `nostr.Tags{ {"e", "..."} }` (a `Tag` is `[]string`)
  - `(Tags).Find(key) Tag` — returns first tag with ≥2 items, or `nil`
  - `(Filter).Matches(Event) bool` — checks kinds/authors/tags/since/until, **ignores Search**
- **Existing test fakes to reuse** (in `chunk_rerank_test.go`): `fakeChunkSearcher`, `fakeStore` (its `fetch` applies `filter.Matches`), `collectEvents`, `idsOf`. `mkEvent(t, sk, d, ts)` lives in `buffer_test.go` and builds a signed kind-30142 event.
- **Fallback safety note:** the Typesense eventstore ORs `filter.Kinds` into its query, so a fallback fetch with `kinds:[30142,21142]` returns the same 30142 docs as today — no special handling needed on fallback paths.
- **Commit style:** conventional commits (`feat:`, `test:`, `docs:`), imperative mood.

## File structure

| File | Responsibility |
|------|----------------|
| `chunk_snippet.go` (new) | Pure leaf: `kindSearchSnippet` constant + `buildSnippetEvent(sk, hit)`. No relay machinery. |
| `chunk_snippet_test.go` (new) | Tests 1–4 (event shape, signature, omitted locators, empty-snippet skip). |
| `chunk_rerank.go` (modify) | `ChunkHit` gains `EventCoord, Snippet, Page, Heading, SourceURL`; best-score map becomes best-hit map; `chunkRerankQuery` gains a signing-key param and interleaves snippets. |
| `chunk_rerank_test.go` (modify) | Tests 5–8 (interleaving, opt-in gate, fallback gate, limit semantics). |
| `chunk_searcher_http.go` (modify) | Parse `event_coord, snippet, page, heading, source_url` from the response. |
| `chunk_searcher_http_test.go` (modify) | Test 9 (extend happy path for new fields). |
| `main.go` (modify) | Load `RELAY_SECKEY` / generate fallback, log signer pubkey, pass key to `chunkRerankQuery`. |
| `README.md`, `CLAUDE.md`, `.env.example` (modify) | Document kind 21142, opt-in, `RELAY_SECKEY`. |

---

### Task 1: `buildSnippetEvent` (chunk_snippet.go)

**Files:**
- Modify: `chunk_rerank.go:15-18` (extend `ChunkHit` — needed by the new tests; existing tests use field names and keep compiling)
- Create: `chunk_snippet.go`
- Create: `chunk_snippet_test.go`

- [ ] **Step 1: Extend `ChunkHit` with snippet/locator fields**

In `chunk_rerank.go`, replace the `ChunkHit` struct:

```go
// ChunkHit is one passage-level match from the indexer's chunk collection:
// which event it belongs to, how well it matched, and the passage itself
// with its source locators (used for kind-21142 snippet events).
type ChunkHit struct {
	EventID    string
	EventCoord string // "30142:<pubkey>:<d-tag>", verbatim from the indexer
	Score      float64
	Snippet    string
	Page       int // 0 = unknown
	Heading    string
	SourceURL  string
}
```

- [ ] **Step 2: Write the failing tests (tests 1–4)**

Create `chunk_snippet_test.go`:

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func fullHit() ChunkHit {
	return ChunkHit{
		EventID:    "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233",
		EventCoord: "30142:deadbeef:photosynthese-skript",
		Score:      0.9312,
		Snippet:    "…bei stärkerem Licht verdoppelt sich die Photosyntheserate…",
		Page:       12,
		Heading:    "Lichtabhängigkeit",
		SourceURL:  "https://example.org/skript.pdf",
	}
}

func tagValue(t *testing.T, e nostr.Event, key string) string {
	t.Helper()
	tag := e.Tags.Find(key)
	if tag == nil {
		t.Fatalf("missing %q tag in %v", key, e.Tags)
	}
	return tag[1]
}

func TestBuildSnippetEvent_ValidSignature(t *testing.T) {
	sk := nostr.Generate()

	evt, ok := buildSnippetEvent(sk, fullHit())
	if !ok {
		t.Fatal("buildSnippetEvent returned ok=false for a valid hit")
	}
	if evt.Kind != kindSearchSnippet {
		t.Errorf("kind = %d, want 21142", evt.Kind)
	}
	if evt.PubKey != nostr.GetPublicKey(sk) {
		t.Errorf("pubkey = %s, want signer pubkey", evt.PubKey.Hex())
	}
	if !evt.VerifySignature() {
		t.Error("snippet event signature does not verify")
	}
}

func TestBuildSnippetEvent_TagsCorrect(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()

	evt, ok := buildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("ok=false")
	}
	if evt.Content != hit.Snippet {
		t.Errorf("content = %q, want snippet verbatim", evt.Content)
	}
	if got := tagValue(t, evt, "e"); got != hit.EventID {
		t.Errorf("e tag = %q, want %q", got, hit.EventID)
	}
	if got := tagValue(t, evt, "a"); got != hit.EventCoord {
		t.Errorf("a tag = %q, want %q", got, hit.EventCoord)
	}
	if got := tagValue(t, evt, "k"); got != "30142" {
		t.Errorf("k tag = %q, want 30142", got)
	}
	if got := tagValue(t, evt, "score"); got != "0.9312" {
		t.Errorf("score tag = %q, want 0.9312 (%%.4f)", got)
	}
	if got := tagValue(t, evt, "page"); got != "12" {
		t.Errorf("page tag = %q, want 12", got)
	}
	if got := tagValue(t, evt, "heading"); got != hit.Heading {
		t.Errorf("heading tag = %q, want %q", got, hit.Heading)
	}
	if got := tagValue(t, evt, "source_url"); got != hit.SourceURL {
		t.Errorf("source_url tag = %q, want %q", got, hit.SourceURL)
	}
}

func TestBuildSnippetEvent_OmitsEmptyLocators(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()
	hit.Page = 0
	hit.Heading = ""
	hit.SourceURL = ""

	evt, ok := buildSnippetEvent(sk, hit)
	if !ok {
		t.Fatal("ok=false")
	}
	for _, key := range []string{"page", "heading", "source_url"} {
		if evt.Tags.Find(key) != nil {
			t.Errorf("%q tag present despite empty locator", key)
		}
	}
}

func TestBuildSnippetEvent_EmptySnippet_NotEmitted(t *testing.T) {
	sk := nostr.Generate()
	hit := fullHit()
	hit.Snippet = ""

	if _, ok := buildSnippetEvent(sk, hit); ok {
		t.Error("ok=true for empty snippet, want false")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test -run TestBuildSnippetEvent -v .`
Expected: FAIL to compile — `undefined: buildSnippetEvent` and `undefined: kindSearchSnippet`.

- [ ] **Step 4: Implement `buildSnippetEvent`**

Create `chunk_snippet.go`:

```go
package main

import (
	"fmt"
	"log"
	"strconv"

	"fiatjaf.com/nostr"
)

// kindSearchSnippet is the ephemeral (never stored) event kind carrying the
// best matching fulltext passage for a chunk-reranked search result. See
// docs/superpowers/specs/2026-06-11-snippet-events-design.md.
const kindSearchSnippet = nostr.Kind(21142)

// buildSnippetEvent turns a chunk hit into a signed kind-21142 event
// annotating its parent 30142. Returns ok=false (and the parent is simply
// delivered without a snippet) when the hit has no snippet text or signing
// fails — snippets are best-effort decoration and must never break search.
func buildSnippetEvent(sk nostr.SecretKey, hit ChunkHit) (nostr.Event, bool) {
	if hit.Snippet == "" {
		return nostr.Event{}, false
	}

	tags := nostr.Tags{
		{"e", hit.EventID},
		{"a", hit.EventCoord},
		{"k", "30142"},
		{"score", fmt.Sprintf("%.4f", hit.Score)},
	}
	if hit.Page > 0 {
		tags = append(tags, nostr.Tag{"page", strconv.Itoa(hit.Page)})
	}
	if hit.Heading != "" {
		tags = append(tags, nostr.Tag{"heading", hit.Heading})
	}
	if hit.SourceURL != "" {
		tags = append(tags, nostr.Tag{"source_url", hit.SourceURL})
	}

	evt := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: nostr.Now(),
		Kind:      kindSearchSnippet,
		Tags:      tags,
		Content:   hit.Snippet,
	}
	if err := evt.Sign(sk); err != nil {
		log.Printf("snippet: signing failed for parent %s: %v", hit.EventID, err)
		return nostr.Event{}, false
	}
	return evt, true
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test -run TestBuildSnippetEvent -v .`
Expected: PASS (4 tests).

Then run the full suite to confirm the `ChunkHit` extension broke nothing:
`GOCACHE=/tmp/claude-1000/go-cache go test .`
Expected: ok (all existing tests still pass — they only set `EventID`/`Score`, which still exist).

- [ ] **Step 6: Commit**

```bash
git add chunk_snippet.go chunk_snippet_test.go chunk_rerank.go
git commit -m "feat(snippet): buildSnippetEvent for ephemeral kind-21142 search snippets"
```

---

### Task 2: Parse snippet fields from `/search_chunks` (chunk_searcher_http.go)

**Files:**
- Modify: `chunk_searcher_http_test.go:13-48` (extend `TestHTTPChunkSearcher_HappyPath`)
- Modify: `chunk_searcher_http.go:55-69` (response parsing)

- [ ] **Step 1: Extend the happy-path test (test 9)**

In `chunk_searcher_http_test.go`, replace the canned response and the final assertion block of `TestHTTPChunkSearcher_HappyPath`.

Replace the `json.NewEncoder(w).Encode(...)` call (lines 20–26) with:

```go
		json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{
					"event_id":    "aa11",
					"event_coord": "30142:pk1:doc-1",
					"score":       0.9,
					"snippet":     "…Photosyntheserate…",
					"page":        12,
					"heading":     "Lichtabhängigkeit",
					"source_url":  "https://example.org/skript.pdf",
				},
				{"event_id": "bb22", "score": 0.5},
			},
			"total": 2,
		})
```

Replace the final `if len(hits) != 2 || ...` block (lines 45–47) with:

```go
	if len(hits) != 2 || hits[0].EventID != "aa11" || hits[0].Score != 0.9 || hits[1].EventID != "bb22" {
		t.Errorf("hits = %+v", hits)
	}
	want := ChunkHit{
		EventID:    "aa11",
		EventCoord: "30142:pk1:doc-1",
		Score:      0.9,
		Snippet:    "…Photosyntheserate…",
		Page:       12,
		Heading:    "Lichtabhängigkeit",
		SourceURL:  "https://example.org/skript.pdf",
	}
	if hits[0] != want {
		t.Errorf("hits[0] = %+v, want %+v", hits[0], want)
	}
	if hits[1].Snippet != "" || hits[1].Page != 0 || hits[1].Heading != "" || hits[1].SourceURL != "" {
		t.Errorf("hits[1] locators should be zero-valued: %+v", hits[1])
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test -run TestHTTPChunkSearcher_HappyPath -v .`
Expected: FAIL — `hits[0]` has empty `EventCoord`/`Snippet` because the parser drops those fields.

- [ ] **Step 3: Parse the new fields**

In `chunk_searcher_http.go`, replace the `var parsed struct {...}` declaration and the `hits := ...` loop (lines 55–69) with:

```go
	var parsed struct {
		Hits []struct {
			EventID    string  `json:"event_id"`
			EventCoord string  `json:"event_coord"`
			Score      float64 `json:"score"`
			Snippet    string  `json:"snippet"`
			Page       int     `json:"page"`
			Heading    string  `json:"heading"`
			SourceURL  string  `json:"source_url"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode search_chunks response: %w", err)
	}

	hits := make([]ChunkHit, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		hits = append(hits, ChunkHit{
			EventID:    h.EventID,
			EventCoord: h.EventCoord,
			Score:      h.Score,
			Snippet:    h.Snippet,
			Page:       h.Page,
			Heading:    h.Heading,
			SourceURL:  h.SourceURL,
		})
	}
	return hits, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test -run TestHTTPChunkSearcher -v .`
Expected: PASS (all 4 HTTP searcher tests).

- [ ] **Step 5: Commit**

```bash
git add chunk_searcher_http.go chunk_searcher_http_test.go
git commit -m "feat(snippet): parse snippet and locator fields from /search_chunks"
```

---

### Task 3: Interleave snippets in `chunkRerankQuery` (chunk_rerank.go)

**Files:**
- Modify: `chunk_rerank_test.go` (append tests 5–8; update 9 existing call sites for the new parameter)
- Modify: `chunk_rerank.go:43-118` (`chunkRerankQuery` signature + best-hit map + interleaving)
- Modify: `main.go:267` (call site — temporary key so the build compiles; real wiring in Task 4)

- [ ] **Step 1: Write the failing tests (tests 5–8)**

Append to `chunk_rerank_test.go`:

```go
// optInFilter asks for parents and snippets, the kind-21142 opt-in shape.
func optInFilter(limit int) nostr.Filter {
	return nostr.Filter{
		Search: "mathematik",
		Kinds:  []nostr.Kind{30142, kindSearchSnippet},
		Limit:  limit,
	}
}

func hitFor(e nostr.Event, score float64, snippet string) ChunkHit {
	return ChunkHit{
		EventID:    e.ID.Hex(),
		EventCoord: "30142:" + e.PubKey.Hex() + ":" + e.Tags.GetD(),
		Score:      score,
		Snippet:    snippet,
	}
}

func TestRerank_InterleavesSnippetsWhenOptedIn(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-a", 1_700_000_001)
	e2 := mkEvent(t, sk, "sn-b", 1_700_000_002)
	store := &fakeStore{events: []nostr.Event{e1, e2}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		hitFor(e2, 0.9, "passage about b"),
		hitFor(e1, 0.5, "passage about a"),
	}}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(10), searcher, store.fetch, 250, relaySK))

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (parent, snippet, parent, snippet): %v", len(got), idsOf(got))
	}
	// Order: best parent, its snippet, next parent, its snippet.
	if got[0].ID != e2.ID || got[2].ID != e1.ID {
		t.Errorf("parent order wrong: %v", idsOf(got))
	}
	for i, parent := range []nostr.Event{e2, e1} {
		snip := got[i*2+1]
		if snip.Kind != kindSearchSnippet {
			t.Fatalf("event %d kind = %d, want 21142", i*2+1, snip.Kind)
		}
		if snip.PubKey != nostr.GetPublicKey(relaySK) {
			t.Errorf("snippet %d signed by %s, want relay key", i, snip.PubKey.Hex())
		}
		if got := tagValue(t, snip, "e"); got != parent.ID.Hex() {
			t.Errorf("snippet %d e tag = %q, want parent %q", i, got, parent.ID.Hex())
		}
	}
	if got[1].Content != "passage about b" || got[3].Content != "passage about a" {
		t.Errorf("snippet contents wrong: %q / %q", got[1].Content, got[3].Content)
	}
}

func TestRerank_NoSnippetsWithoutKindOptIn(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-no", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{hitFor(e1, 0.9, "a passage")}}

	filter := nostr.Filter{Search: "mathematik", Kinds: []nostr.Kind{30142}, Limit: 10}
	got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, relaySK))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Fatalf("got %v, want only the parent", idsOf(got))
	}
	for _, e := range got {
		if e.Kind == kindSearchSnippet {
			t.Error("snippet emitted without kind opt-in")
		}
	}
}

func TestRerank_NoSnippetsOnFallback(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-fb", 1_700_000_001)
	store := &fakeStore{events: []nostr.Event{e1}}
	searcher := &fakeChunkSearcher{err: errors.New("indexer down")}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(10), searcher, store.fetch, 250, relaySK))

	if len(got) != 1 || got[0].ID != e1.ID {
		t.Fatalf("fallback must deliver plain results, got %v", idsOf(got))
	}
	if got[0].Kind == kindSearchSnippet {
		t.Error("snippet emitted on fallback path")
	}
}

func TestRerank_LimitCountsParents(t *testing.T) {
	sk := nostr.Generate()
	relaySK := nostr.Generate()
	e1 := mkEvent(t, sk, "sn-l1", 1_700_000_001)
	e2 := mkEvent(t, sk, "sn-l2", 1_700_000_002)
	e3 := mkEvent(t, sk, "sn-l3", 1_700_000_003)
	store := &fakeStore{events: []nostr.Event{e1, e2, e3}}
	searcher := &fakeChunkSearcher{hits: []ChunkHit{
		hitFor(e1, 0.9, "p1"),
		hitFor(e2, 0.8, "p2"),
		hitFor(e3, 0.7, "p3"),
	}}

	got := collectEvents(chunkRerankQuery(context.Background(), optInFilter(2), searcher, store.fetch, 250, relaySK))

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (limit=2 parents + 2 snippets)", len(got))
	}
	if got[0].ID != e1.ID || got[2].ID != e2.ID {
		t.Errorf("wrong parents under limit: %v", idsOf(got))
	}
	if got[1].Kind != kindSearchSnippet || got[3].Kind != kindSearchSnippet {
		t.Error("expected snippet after each parent")
	}
}
```

Note: `tagValue` was defined in `chunk_snippet_test.go` (Task 1) and is reusable here — same package.

- [ ] **Step 2: Update the 9 existing `chunkRerankQuery` call sites in `chunk_rerank_test.go`**

Every existing test calls `chunkRerankQuery(context.Background(), filter, <searcher>, store.fetch, 250)`. Append `, nostr.Generate()` as the final argument in all 9 calls, e.g.:

```go
got := collectEvents(chunkRerankQuery(context.Background(), filter, searcher, store.fetch, 250, nostr.Generate()))
```

(For `TestChunkRerank_NilSearcher_BypassesChunks` the searcher argument stays `nil`.)

- [ ] **Step 3: Run tests to verify the new ones fail**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test -run 'TestRerank_|TestChunkRerank_' -v .`
Expected: FAIL to compile — `chunkRerankQuery` does not accept 6 arguments yet.

- [ ] **Step 4: Implement interleaving in `chunk_rerank.go`**

Replace `chunkRerankQuery` (lines 43–118) with:

```go
// chunkRerankQuery serves NIP-50 search queries ranked by chunk-level
// relevance: it asks the indexer's chunk index for the best passages, then
// returns the parent events in best-chunk-score order. When the client opted
// in by including kind 21142 in the filter, each parent is followed by an
// ephemeral snippet event carrying its best matching passage (signed with
// sk). Any condition that prevents re-ranking (no search term, no searcher,
// indexer error, zero chunk hits) falls back to the plain event-level search
// — which never emits snippets — so recall is never worse than today.
func chunkRerankQuery(ctx context.Context, filter nostr.Filter, searcher ChunkSearcher, fetch fetchFunc, maxLimit int, sk nostr.SecretKey) iter.Seq[nostr.Event] {
	if filter.Search == "" || searcher == nil {
		return fetch(filter, maxLimit)
	}

	limit := filter.Limit
	if limit <= 0 || limit > maxLimit {
		limit = maxLimit
	}

	k := limit * chunkSearchKMultiplier
	if k > chunkSearchKMax {
		k = chunkSearchKMax
	}

	hits, err := searcher.SearchChunks(ctx, filter.Search, k)
	if err != nil {
		log.Printf("chunk-rerank: search_chunks failed, falling back to plain search: %v", err)
		return fetch(filter, maxLimit)
	}
	if len(hits) == 0 {
		return fetch(filter, maxLimit)
	}

	// Rank parent events by their best chunk hit.
	best := make(map[string]ChunkHit, len(hits))
	for _, h := range hits {
		if b, ok := best[h.EventID]; !ok || h.Score > b.Score {
			best[h.EventID] = h
		}
	}
	ranked := make([]string, 0, len(best))
	for id := range best {
		ranked = append(ranked, id)
	}
	sort.Slice(ranked, func(i, j int) bool { return best[ranked[i]].Score > best[ranked[j]].Score })

	parentIDs := make([]nostr.ID, 0, len(ranked))
	bestByID := make(map[nostr.ID]ChunkHit, len(ranked))
	for _, hex := range ranked {
		id, err := nostr.IDFromHex(hex)
		if err != nil {
			continue
		}
		parentIDs = append(parentIDs, id)
		bestByID[id] = best[hex]
	}
	if len(parentIDs) == 0 {
		return fetch(filter, maxLimit)
	}

	byID := make(map[nostr.ID]nostr.Event, len(parentIDs))
	for e := range fetch(nostr.Filter{IDs: parentIDs}, len(parentIDs)) {
		byID[e.ID] = e
	}

	wantSnippets := slices.Contains(filter.Kinds, kindSearchSnippet)

	return func(yield func(nostr.Event) bool) {
		n := 0
		for _, id := range parentIDs {
			e, ok := byID[id]
			if !ok {
				continue
			}
			// filter.Matches ignores the Search field, so this applies only
			// the remaining constraints (kinds, authors, tags, since/until).
			if !filter.Matches(e) {
				continue
			}
			if !yield(e) {
				return
			}
			if wantSnippets {
				if snip, ok := buildSnippetEvent(sk, bestByID[id]); ok {
					if !yield(snip) {
						return
					}
				}
			}
			n++
			if n >= limit {
				return
			}
		}
	}
}
```

Add `"slices"` to the import block in `chunk_rerank.go`:

```go
import (
	"context"
	"iter"
	"log"
	"slices"
	"sort"

	"fiatjaf.com/nostr"
)
```

- [ ] **Step 5: Update the call site in `main.go` so the package compiles**

In `main.go` (around line 267), change:

```go
		return chunkRerankQuery(ctx, filter, chunkSearcher, tsDB.QueryEvents, maxLimit)
```

to:

```go
		return chunkRerankQuery(ctx, filter, chunkSearcher, tsDB.QueryEvents, maxLimit, relaySK)
```

and immediately above the `var chunkSearcher ChunkSearcher` line (≈246), add a placeholder that Task 4 replaces with real env loading:

```go
	relaySK := nostr.Generate() // replaced with RELAY_SECKEY wiring in the next commit
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `GOCACHE=/tmp/claude-1000/go-cache go test .`
Expected: ok — all tests pass (4 snippet + 13 rerank/new + 4 HTTP + existing buffer/management suites).

- [ ] **Step 7: Commit**

```bash
git add chunk_rerank.go chunk_rerank_test.go main.go
git commit -m "feat(snippet): interleave kind-21142 snippet events for opted-in searches"
```

---

### Task 4: `RELAY_SECKEY` wiring + documentation

**Files:**
- Modify: `main.go` (replace the Task 3 placeholder with env loading)
- Modify: `.env.example`, `docker-compose.yml`, `CLAUDE.md`, `README.md`

No new unit test: this is env plumbing of the exact pattern already used for `CHUNK_RERANK_ENABLED`, and `main()` has no test harness in this repo. Verification is a compile + full suite + manual startup-log check.

- [ ] **Step 1: Replace the placeholder with real key loading in `main.go`**

Replace the Task 3 placeholder line:

```go
	relaySK := nostr.Generate() // replaced with RELAY_SECKEY wiring in the next commit
```

with:

```go
	// Signing identity for relay-originated kind-21142 snippet events.
	// RELAY_SECKEY must be a dedicated key — never the operator's. Without
	// it, a fresh key is generated per boot: snippets stay validly signed,
	// the identity just isn't stable across restarts.
	var relaySK nostr.SecretKey
	if hexKey := os.Getenv("RELAY_SECKEY"); hexKey != "" {
		var err error
		relaySK, err = nostr.SecretKeyFromHex(hexKey)
		if err != nil {
			panic(fmt.Sprintf("invalid RELAY_SECKEY: %v", err))
		}
	} else {
		relaySK = nostr.Generate()
		fmt.Println("RELAY_SECKEY not set — generated ephemeral snippet-signing key for this boot")
	}
	fmt.Printf("Snippet signer pubkey: %s\n", nostr.GetPublicKey(relaySK).Hex())
```

- [ ] **Step 2: Document in `.env.example`**

Append after the chunk re-ranking block:

```bash
# Snippet signing key (optional, hex secret key). Signs the ephemeral
# kind-21142 search-snippet events that opted-in clients receive alongside
# chunk-reranked results (kinds:[30142,21142] + search). MUST be a dedicated
# key for this relay — never an operator/personal key. When unset, a fresh
# key is generated at startup (snippets stay validly signed, the signer
# pubkey just changes across restarts; it is printed in the startup log).
# RELAY_SECKEY=""
```

- [ ] **Step 3: Pass through `docker-compose.yml`**

In the `amb-relay` service `environment:` list, after the `INDEXER_API_TOKEN` line, add:

```yaml
      # Dedicated key signing ephemeral kind-21142 search snippets. Optional;
      # unset means a per-boot generated key (see .env.example).
      - RELAY_SECKEY=${RELAY_SECKEY:-}
```

- [ ] **Step 4: Document in `CLAUDE.md`**

In `CLAUDE.md`, extend the chunk re-ranking paragraph under "## Environment Variables" (the one beginning "Optional chunk re-ranking (`chunk_rerank.go`)") by appending:

```markdown
Searches that additionally opt in with `kinds:[30142,21142]` receive an
ephemeral kind-21142 snippet event after each result, carrying the best
matching passage (`content`), `e`/`a`/`k` references to the parent, a
`score` tag, and `page`/`heading`/`source_url` locators when known
(`chunk_snippet.go`). Snippets are relay-signed with `RELAY_SECKEY` (a
dedicated key; per-boot generated when unset), never stored, and never
emitted on fallback paths. Clients that only ask for kind 30142 see exactly
the pre-snippet behavior.
```

- [ ] **Step 5: Document in `README.md`**

In the "Chunk Re-Ranking (Optional)" section of `README.md`, append a subsection:

````markdown
### Search snippets (kind 21142)

With chunk re-ranking active, clients can opt in to ephemeral snippet
events by adding kind `21142` to their search REQ:

```json
{"kinds": [30142, 21142], "search": "photosynthese", "limit": 5}
```

Each result is then followed by a relay-signed kind-21142 event (ephemeral
range — never stored) carrying the best matching passage:

```json
{
  "kind": 21142,
  "content": "…bei stärkerem Licht verdoppelt sich die Photosyntheserate…",
  "tags": [
    ["e", "<parent event id>"],
    ["a", "30142:<author pubkey>:<d-tag>"],
    ["k", "30142"],
    ["score", "0.9312"],
    ["page", "12"],
    ["heading", "Lichtabhängigkeit"],
    ["source_url", "https://…/skript.pdf"]
  ]
}
```

`page`, `heading` and `source_url` appear only when the indexer extracted
them. `limit` counts parent events, so an opted-in client receives at most
`2×limit` events before EOSE. Snippets are best-effort decoration: on any
re-rank fallback (indexer down, no chunk hits, feature disabled) clients
get plain results without snippets, and clients that only request kind
30142 are completely unaffected.

Snippet events are signed with `RELAY_SECKEY` (a dedicated relay key — set
it in `.env` for a stable signer identity; when unset a fresh key is
generated per boot and its pubkey printed in the startup log).
````

Test with nak:

```bash
nak req --search "photosynthese" -k 30142 -k 21142 --limit 5 ws://localhost:3334
```

- [ ] **Step 6: Verify build (both resolution modes) + full suite**

```bash
GOCACHE=/tmp/claude-1000/go-cache go build .
GOCACHE=/tmp/claude-1000/go-cache GOWORK=off go build .   # Docker/server path
GOCACHE=/tmp/claude-1000/go-cache go test .
```
Expected: both builds succeed; full suite ok.

- [ ] **Step 7: Manual smoke test (requires local stack)**

```bash
docker compose up -d typesense
go run .   # check startup log prints "Snippet signer pubkey: <64-hex>"
```
Expected log line present; Ctrl-C afterwards. (Full end-to-end snippet delivery needs the amb-indexer chunk index — verify on the dev relays after deployment, same as the chunk-rerank rollout.)

- [ ] **Step 8: Commit**

```bash
git add main.go .env.example docker-compose.yml CLAUDE.md README.md
git commit -m "feat(snippet): RELAY_SECKEY signer wiring + kind-21142 docs"
```

---

## Out of scope (per spec)

- Top-N passages per parent (single best only)
- NIP-11 signer-pubkey exposure
- Caching of chunk responses
- Any amb-indexer changes (response already carries all needed fields)
- The NIP-AMB spec paragraph (out of repo; operator publishes)

## Post-implementation (not part of this plan's tasks)

- Push `dev`, let Forgejo CI build the image, deploy to both dev stacks via the homelab playbook (`--tags amb-relay-dev,amb-relay-oersi-dev`), then live-test the nak command above against `wss://dev.amb-relay.edufeed.org`. Decide whether to set a persistent `RELAY_SECKEY` in the homelab vault at that point.
