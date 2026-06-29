# Calendar Semantic Search (Chunk Embeddings) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give NIP-52 calendar kinds 31922/31923 the same semantic chunk-rerank search as long-form/wiki, so a conceptual topic query (with or without a time window) ranks events by meaning while staying correctly windowed.

**Architecture:** amb-indexer subscribes to 31922/31923 and routes them through the existing long-form content-direct chunk path (no new doc-builder). amb-relay opts calendar into rerank (`chunked:true`) and adds a thin window-aware dispatcher that strips the synthetic range params before calling the shared `ChunkRerankQuery`, then post-windows the reranked results relay-side. No `nostrlib` change.

**Tech Stack:** Go 1.25, khatru relay, Typesense, BoltDB, `fiatjaf.com/nostr` (nostrlib fork: khatru/calendar, khatru/semantic, nip52).

## Global Constraints

- **Dev only.** Never touch prod relays. Both services rebuilt to dev only.
- **Two repos, two worktrees.** Relay tasks (2–4) run in the current worktree `/home/laoc/coding/edufeed/amb-relay/.claude/worktrees/profile-fallback`. The indexer task (1) is a DIFFERENT repo (`/home/laoc/coding/edufeed/amb-indexer`) and MUST be done in its own isolated git worktree.
- **Build/test faithfully.** Relay worktree is NOT in `edufeed/go.work`, so ALL relay Go commands MUST use `GOWORK=off` (e.g. `GOWORK=off go test ./...`, `GOWORK=off go build .`) to resolve the go.mod-pinned nostrlib. The amb-indexer worktree uses plain `go test ./...` / `go build .`.
- **No nostrlib change.** Reuse exported helpers only: `calendar.ExtractCalendarFilter`, `calendar.RemoveCalendarTags`, `calendar.HasCalendarKinds`, `calendar.IsCalendarEventKind`, `nip52.ParseCalendarEvent`, `semantic.ChunkRerankQuery`, `semantic.KindSearchSnippet`.
- **Do NOT reuse `matchesCalendarFilter`** (unexported, divergent missing-field semantics — a latent parity bug vs. the Typesense numeric path). Hand-roll the window predicate with explicit missing-field-fails-its-bound semantics.
- **Commits:** stage files by name (never `git add -A`), never skip hooks, create new commits (don't amend).
- Calendar semantic engages only when `CHUNK_RERANK_ENABLED=true` and `INDEXER_API_TOKEN` is set on the relay; a backfill reindex on the indexer is needed for existing events.

---

### Task 1: amb-indexer — subscribe + route calendar through content-direct chunk path

**Repo/Worktree:** `amb-indexer` — create a fresh isolated worktree for this repo (per the worktrees skill). All commands below run from that worktree.

**Files:**
- Modify: `amb-indexer/main.go:165` (source `Kinds`)
- Modify: `amb-indexer/worker.go:66-74` (content-direct gate)
- Test: `amb-indexer/worker_test.go` (add `TestWorker_ProcessContentDirect_Kind31923`)

**Interfaces:**
- Consumes: existing `(*Worker).processContentDirect(ctx, ev)` → `docFromLongformEvent(&ev, sourceURL, maxBytes)` (worker.go:313); coord built as `fmt.Sprintf("%d:%s:%s", event.Kind, pubkeyHex, dValue)` (schema.go:143); `BuildChunkDocs` always sets `Embedding` + `Snippet`, elides only `Text` when `!permissive` (schema.go:147-171).
- Produces: 31922/31923 events chunked into the shared `amb_chunks_30142` collection with coord `31923:<pk>:<d>` / `31922:<pk>:<d>`.

- [ ] **Step 1: Write the failing test**

Add to `amb-indexer/worker_test.go`, modeled on `TestWorker_ProcessContentDirect_Kind30818` (worker_test.go:427-501). Build a 31923 time-based event with NO `license` tag and a German description carrying the semantic signal but not the literal query token.

```go
func TestWorker_ProcessContentDirect_Kind31923(t *testing.T) {
	fx := newWorkerFixture(t, 0, 0)

	ev := nostr.Event{
		Kind:      31923,
		PubKey:    testPubKey(t),
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags: nostr.Tags{
			{"d", "scale-up-2026"},
			{"title", "SCALE-UP – Lehren und Lernen und der Raum hilft mit"},
			{"start", "1718600000"},
			{"end", "1718603600"},
		},
		Content: "Wir erproben aktivierendes und kooperatives Lernen sowie studentisches Lernen im Raum.",
	}
	ev.ID = ev.GetID()

	if err := fx.worker.Process(context.Background(), ev); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got := fx.relay.calls.Load(); got != 0 {
		t.Errorf("relay SetContent calls = %d, want 0 (content-direct path)", got)
	}
	if got := fx.ts.upserts.Load(); got != 1 {
		t.Errorf("ts upserts = %d, want 1", got)
	}
	if fx.dead.Load() != 0 {
		t.Errorf("dead-letter = %d, want 0", fx.dead.Load())
	}
	docs := fx.ts.lastDocs
	if len(docs) == 0 {
		t.Fatal("no chunk docs upserted")
	}
	wantCoord := "31923:" + ev.PubKey.Hex() + ":scale-up-2026"
	if docs[0].EventCoord != wantCoord {
		t.Errorf("EventCoord = %q, want %q", docs[0].EventCoord, wantCoord)
	}
	if docs[0].LicensePermissive {
		t.Errorf("LicensePermissive = true, want false (no license tag)")
	}
	if docs[0].Text != "" {
		t.Errorf("Text = %q, want empty (non-permissive elides text)", docs[0].Text)
	}
	if docs[0].Snippet == "" {
		t.Errorf("Snippet empty, want present even when non-permissive")
	}
	if len(docs[0].Embedding) == 0 {
		t.Errorf("Embedding empty, want present even when non-permissive")
	}
}
```

> NOTE: match the fixture field/helper names to the actual 30818 test (`fx.relay.calls`, `fx.ts.upserts`, `fx.ts.lastDocs`, `fx.dead`, `testPubKey`/`newWorkerFixture`). Read worker_test.go:427-501 first and mirror it exactly; adjust any helper name that differs.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd <amb-indexer-worktree> && go test ./... -run TestWorker_ProcessContentDirect_Kind31923 -v`
Expected: FAIL — 31923 is not routed to `processContentDirect` yet (gate only matches 30023/30818), so it takes the default path (relay SetContent call ≠ 0 / no chunk upsert).

- [ ] **Step 3: Add 31922/31923 to the source kinds**

Modify `amb-indexer/main.go:165`:

```go
Kinds: []nostr.Kind{30142, 30023, 30818, 31922, 31923},
```

- [ ] **Step 4: Extend the content-direct gate**

Modify the gate at `amb-indexer/worker.go:66-74`. Current:

```go
	if ev.Kind == 30023 || ev.Kind == 30818 {
		return w.processContentDirect(ctx, ev)
	}
```

New:

```go
	if ev.Kind == 30023 || ev.Kind == 30818 || ev.Kind == 31922 || ev.Kind == 31923 {
		return w.processContentDirect(ctx, ev)
	}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd <amb-indexer-worktree> && go test ./... -run TestWorker_ProcessContentDirect_Kind31923 -v`
Expected: PASS

- [ ] **Step 6: Run the full indexer suite**

Run: `cd <amb-indexer-worktree> && go test ./...`
Expected: PASS (no regressions)

- [ ] **Step 7: Commit**

```bash
git add main.go worker.go worker_test.go
git commit -m "$(cat <<'EOF'
feat(calendar): chunk 31922/31923 via the long-form content-direct path

Subscribe to NIP-52 date/time calendar kinds and route them through
processContentDirect (docFromLongformEvent), so their descriptions are
chunked/embedded into the shared chunk collection for semantic search.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: amb-relay — `inCalendarWindow` predicate

**Repo/Worktree:** current relay worktree. All Go commands use `GOWORK=off`.

**Files:**
- Create: `amb-relay/calendar_rerank.go`
- Test: `amb-relay/calendar_rerank_test.go`

**Interfaces:**
- Consumes: `nip52.ParseCalendarEvent(e) → CalendarEvent{Start, End time.Time}` (zero when absent); `calendar.CalendarFilter{StartAfter, StartBefore, EndAfter, EndBefore int64, Geohashes []string}`.
- Produces: `func inCalendarWindow(e nostr.Event, cf calendar.CalendarFilter) bool` — keep iff every bound present in the REQ is satisfied inclusively; a missing gated field fails its bound.

- [ ] **Step 1: Write the failing test**

Create `amb-relay/calendar_rerank_test.go`:

```go
package main

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
)

func calEvent(t *testing.T, start, end string) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"d", "e1"}, {"title", "T"}}
	if start != "" {
		tags = append(tags, nostr.Tag{"start", start})
	}
	if end != "" {
		tags = append(tags, nostr.Tag{"end", end})
	}
	e := nostr.Event{Kind: 31923, Tags: tags}
	e.ID = e.GetID()
	return e
}

func TestInCalendarWindow(t *testing.T) {
	// event: start=1718600000, end=1718603600
	e := calEvent(t, "1718600000", "1718603600")

	cases := []struct {
		name string
		cf   calendar.CalendarFilter
		want bool
	}{
		{"no bounds", calendar.CalendarFilter{}, true},
		{"start_after pass", calendar.CalendarFilter{StartAfter: 1718500000}, true},
		{"start_after fail", calendar.CalendarFilter{StartAfter: 1718700000}, false},
		{"start_before pass", calendar.CalendarFilter{StartBefore: 1718700000}, true},
		{"start_before fail", calendar.CalendarFilter{StartBefore: 1718500000}, false},
		{"end_after pass", calendar.CalendarFilter{EndAfter: 1718600000}, true},
		{"end_after fail", calendar.CalendarFilter{EndAfter: 1718700000}, false},
		{"end_before pass", calendar.CalendarFilter{EndBefore: 1718700000}, true},
		{"end_before fail", calendar.CalendarFilter{EndBefore: 1718600000}, false},
		{"inclusive lower", calendar.CalendarFilter{StartAfter: 1718600000}, true},
		{"inclusive upper", calendar.CalendarFilter{StartBefore: 1718600000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inCalendarWindow(e, c.cf); got != c.want {
				t.Errorf("inCalendarWindow = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInCalendarWindowMissingFieldFailsBound(t *testing.T) {
	// event with no end tag: any end bound must fail.
	e := calEvent(t, "1718600000", "")
	if inCalendarWindow(e, calendar.CalendarFilter{EndBefore: 1718700000}) {
		t.Error("missing end with EndBefore set: want false")
	}
	if inCalendarWindow(e, calendar.CalendarFilter{EndAfter: 1}) {
		t.Error("missing end with EndAfter set: want false")
	}
	// no start tag: any start bound must fail.
	e2 := calEvent(t, "", "1718603600")
	if inCalendarWindow(e2, calendar.CalendarFilter{StartAfter: 1}) {
		t.Error("missing start with StartAfter set: want false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test . -run TestInCalendarWindow -v`
Expected: FAIL with "undefined: inCalendarWindow"

- [ ] **Step 3: Write minimal implementation**

Create `amb-relay/calendar_rerank.go`:

```go
package main

import (
	"context"
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/semantic"
	"fiatjaf.com/nostr/nip52"
)

// inCalendarWindow reports whether a calendar event satisfies every range
// bound present in the REQ, inclusively. A missing gated field fails its
// bound — matching the Typesense numeric path (a doc missing `end` fails an
// `end:<=` clause), and deliberately NOT khatru/calendar's unexported
// matchesCalendarFilter (which treats a missing upper-bounded field as
// passing).
func inCalendarWindow(e nostr.Event, cf calendar.CalendarFilter) bool {
	cal := nip52.ParseCalendarEvent(e)
	hasStart := !cal.Start.IsZero()
	hasEnd := !cal.End.IsZero()
	var start, end int64
	if hasStart {
		start = cal.Start.Unix()
	}
	if hasEnd {
		end = cal.End.Unix()
	}
	if cf.StartAfter > 0 && (!hasStart || start < cf.StartAfter) {
		return false
	}
	if cf.StartBefore > 0 && (!hasStart || start > cf.StartBefore) {
		return false
	}
	if cf.EndAfter > 0 && (!hasEnd || end < cf.EndAfter) {
		return false
	}
	if cf.EndBefore > 0 && (!hasEnd || end > cf.EndBefore) {
		return false
	}
	return true
}
```

> NOTE: the `context`, `iter`, `semantic` imports are unused until Tasks 3–4 add `windowCalendar`/`calendarRerankQuery`. To keep Task 2 compiling on its own, add only the imports this step uses (`nostr`, `calendar`, `nip52`) now, and add `context`/`iter`/`semantic` in Steps of Tasks 3 and 4 when their code lands. (Go fails to compile on unused imports.)

Use this trimmed import block for Task 2:

```go
import (
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/nip52"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `GOWORK=off go test . -run TestInCalendarWindow -v`
Expected: PASS (both `TestInCalendarWindow` and `TestInCalendarWindowMissingFieldFailsBound`)

- [ ] **Step 5: Commit**

```bash
git add calendar_rerank.go calendar_rerank_test.go
git commit -m "$(cat <<'EOF'
feat(calendar): add inCalendarWindow predicate for rerank windowing

Inclusive per-bound check with explicit missing-field-fails semantics, to
stay parity-faithful with the Typesense numeric window path.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: amb-relay — `windowCalendar` iterator wrapper

**Files:**
- Modify: `amb-relay/calendar_rerank.go` (add `windowCalendar` + grow import block)
- Test: `amb-relay/calendar_rerank_test.go` (add window-wrapper tests)

**Interfaces:**
- Consumes: `inCalendarWindow` (Task 2); `calendar.IsCalendarEventKind(kind)`; `semantic.KindSearchSnippet` (= kind 21142).
- Produces: `func windowCalendar(seq iter.Seq[nostr.Event], cf calendar.CalendarFilter, limit int) iter.Seq[nostr.Event]` — drops out-of-window calendar parents, keeps a snippet iff its parent was kept, passes non-calendar kinds through, and re-applies `limit` counting parents only.

- [ ] **Step 1: Write the failing test**

Add to `amb-relay/calendar_rerank_test.go`:

```go
func seqOf(events ...nostr.Event) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for _, e := range events {
			if !yield(e) {
				return
			}
		}
	}
}

func snippetFor(t *testing.T, parentID string) nostr.Event {
	t.Helper()
	e := nostr.Event{
		Kind: semantic.KindSearchSnippet,
		Tags: nostr.Tags{{"e", parentID}},
	}
	e.ID = e.GetID()
	return e
}

func TestWindowCalendarDropsOutOfWindow(t *testing.T) {
	in := calEvent(t, "1718600000", "1718603600")  // in window
	out := calEvent(t, "1710000000", "1710003600")  // before window
	cf := calendar.CalendarFilter{StartAfter: 1718000000, StartBefore: 1719000000}

	got := collectEvents(windowCalendar(seqOf(in, out), cf, 0))
	if len(got) != 1 || got[0].ID != in.ID {
		t.Fatalf("got %d events, want 1 (the in-window event)", len(got))
	}
}

func TestWindowCalendarKeepsSnippetWithKeptParent(t *testing.T) {
	parent := calEvent(t, "1718600000", "1718603600")
	snip := snippetFor(t, parent.ID.Hex())
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(parent, snip), cf, 0))
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (parent + its snippet)", len(got))
	}
	if got[1].Kind != semantic.KindSearchSnippet {
		t.Errorf("second event kind = %d, want snippet", got[1].Kind)
	}
}

func TestWindowCalendarDropsSnippetWithDroppedParent(t *testing.T) {
	parent := calEvent(t, "1710000000", "1710003600")  // out of window
	snip := snippetFor(t, parent.ID.Hex())
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(parent, snip), cf, 0))
	if len(got) != 0 {
		t.Fatalf("got %d, want 0 (parent dropped, so snippet dropped)", len(got))
	}
}

func TestWindowCalendarAppliesLimit(t *testing.T) {
	a := calEvent(t, "1718600000", "1718603600")
	b := calEvent(t, "1718600001", "1718603601")
	c := calEvent(t, "1718600002", "1718603602")
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(a, b, c), cf, 2))
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (limit applied to parents)", len(got))
	}
}

func TestWindowCalendarPassesNonCalendar(t *testing.T) {
	cal := calEvent(t, "1710000000", "1710003600") // out of window calendar
	other := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}}}
	other.ID = other.GetID()
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(cal, other), cf, 0))
	if len(got) != 1 || got[0].Kind != 30142 {
		t.Fatalf("want only the non-calendar 30142 event through, got %d", len(got))
	}
}
```

> `collectEvents` already exists in the relay test package (`query_combined_integration_test.go`). The new `calendar_rerank_test.go` import block must grow with each task: Task 2 imports `testing`, `fiatjaf.com/nostr`, `fiatjaf.com/nostr/khatru/calendar`; Task 3 adds `iter` and `fiatjaf.com/nostr/khatru/semantic` (used by `seqOf`/`snippetFor`); Task 4 adds `context`. Go fails on unused imports, so add each only when its first user lands.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test . -run TestWindowCalendar -v`
Expected: FAIL with "undefined: windowCalendar"

- [ ] **Step 3: Write minimal implementation**

Grow the import block in `calendar_rerank.go` to add `iter` and `semantic`:

```go
import (
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/semantic"
	"fiatjaf.com/nostr/nip52"
)
```

Add the function:

```go
// windowCalendar post-filters a reranked sequence by the calendar window.
// ChunkRerankQuery interleaves a kind-21142 snippet AFTER each parent it
// keeps; we keep a snippet iff its parent passed the window. Non-calendar
// kinds pass through untouched. The limit counts parents only and is applied
// after windowing (ChunkRerankQuery was called with Limit=0 to hand us the
// full reranked pool).
func windowCalendar(seq iter.Seq[nostr.Event], cf calendar.CalendarFilter, limit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		n := 0
		keptParent := false
		done := false
		for e := range seq {
			if e.Kind == semantic.KindSearchSnippet {
				if keptParent {
					if !yield(e) {
						return
					}
				}
				if done {
					return
				}
				continue
			}
			if done {
				return
			}
			keep := !calendar.IsCalendarEventKind(e.Kind) || inCalendarWindow(e, cf)
			keptParent = keep
			if !keep {
				continue
			}
			if !yield(e) {
				return
			}
			n++
			if limit > 0 && n >= limit {
				done = true
			}
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `GOWORK=off go test . -run TestWindowCalendar -v`
Expected: PASS (all five window tests)

- [ ] **Step 5: Commit**

```bash
git add calendar_rerank.go calendar_rerank_test.go
git commit -m "$(cat <<'EOF'
feat(calendar): add windowCalendar rerank-result post-filter

Drops out-of-window calendar parents (and their trailing snippets), passes
non-calendar kinds through, and re-applies the REQ limit counting parents
only.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: amb-relay — `calendarRerankQuery` dispatcher + wire into main.go

**Files:**
- Modify: `amb-relay/calendar_rerank.go` (add `calendarRerankQuery` + grow imports to add `context`)
- Modify: `amb-relay/main.go:426-444` (add `chunked: true,` to calendar contentType)
- Modify: `amb-relay/main.go:819-836` (swap the QueryStored rerank call to `calendarRerankQuery`)
- Test: `amb-relay/calendar_rerank_test.go` (dispatcher tests)

**Interfaces:**
- Consumes: `fetchFunc` (content_registry.go:12, `func(nostr.Filter, int) iter.Seq[nostr.Event]`); `semantic.ChunkRerankQuery(ctx, filter, searcher, fetch, maxLimit, sk)`; `semantic.ChunkSearcher`; `calendar.ExtractCalendarFilter`, `calendar.RemoveCalendarTags`, `calendar.HasCalendarKinds`; `windowCalendar` (Task 3).
- Produces: `func calendarRerankQuery(ctx context.Context, filter nostr.Filter, searcher semantic.ChunkSearcher, fetch fetchFunc, maxLimit int, sk nostr.SecretKey) iter.Seq[nostr.Event]`.

- [ ] **Step 1: Write the failing test**

Add to `amb-relay/calendar_rerank_test.go`. Reuse the package test doubles `fakeChunkSearcher` and `fakeStore` (from `query_combined_integration_test.go`). The dispatcher decisions to verify:
1. geohash present → bypass rerank, call `fetch` directly (searcher untouched);
2. calendar topic-only (no range) → plain `ChunkRerankQuery` (searcher called);
3. topic + time → rerank with stripped filter, results windowed;
4. non-calendar kinds (`HasCalendarKinds` false) → plain `ChunkRerankQuery`.

First add a `calEventD` helper (custom `d` so two events get distinct coords/ids) and refactor `calEvent` to delegate to it. Place beside `calEvent` in `calendar_rerank_test.go`:

```go
func calEventD(t *testing.T, d, start, end string) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"d", d}, {"title", "T"}}
	if start != "" {
		tags = append(tags, nostr.Tag{"start", start})
	}
	if end != "" {
		tags = append(tags, nostr.Tag{"end", end})
	}
	e := nostr.Event{Kind: 31923, Tags: tags}
	e.ID = e.GetID()
	return e
}
```

Update `calEvent` (from Task 2) to delegate: `func calEvent(t *testing.T, start, end string) nostr.Event { return calEventD(t, "e1", start, end) }`.

```go
func TestCalendarRerankGeohashBypassesRerank(t *testing.T) {
	searcher := &fakeChunkSearcher{}
	store := &fakeStore{}
	filter := nostr.Filter{
		Kinds:  []nostr.Kind{31923},
		Search: "lernen",
		Tags:   nostr.TagMap{"g": []string{"u33d"}},
	}
	collectEvents(calendarRerankQuery(context.Background(), filter, searcher, store.fetch, 200, nostr.Generate()))
	if searcher.called {
		t.Error("geohash query must bypass rerank (searcher should be untouched)")
	}
	if len(store.calls) != 1 {
		t.Errorf("fetch calls = %d, want 1 (direct fetch path)", len(store.calls))
	}
}

func TestCalendarRerankTopicOnlyUsesPlainRerank(t *testing.T) {
	searcher := &fakeChunkSearcher{}
	store := &fakeStore{}
	filter := nostr.Filter{Kinds: []nostr.Kind{31923}, Search: "lernen"}
	collectEvents(calendarRerankQuery(context.Background(), filter, searcher, store.fetch, 200, nostr.Generate()))
	if !searcher.called {
		t.Error("topic-only calendar search must go through ChunkRerankQuery")
	}
}

func TestCalendarRerankNonCalendarUsesPlainRerank(t *testing.T) {
	searcher := &fakeChunkSearcher{}
	store := &fakeStore{}
	filter := nostr.Filter{Kinds: []nostr.Kind{30142}, Search: "lernen"}
	collectEvents(calendarRerankQuery(context.Background(), filter, searcher, store.fetch, 200, nostr.Generate()))
	if !searcher.called {
		t.Error("non-calendar search must go through ChunkRerankQuery")
	}
}

func TestCalendarRerankTopicTimeStripsAndWindows(t *testing.T) {
	// in- and out-of-window calendar events both returned by the lexical fetch,
	// both topic-matched by the searcher; only the in-window one should survive.
	inEv := calEventD(t, "e1", "1718600000", "1718603600")
	outEv := calEventD(t, "e2", "1710000000", "1710003600")
	searcher := &fakeChunkSearcher{hits: []semantic.ChunkHit{
		{EventID: inEv.ID.Hex(), EventCoord: coordFor(inEv), Score: 0.9, Snippet: "in"},
		{EventID: outEv.ID.Hex(), EventCoord: coordFor(outEv), Score: 0.8, Snippet: "out"},
	}}
	store := &fakeStore{events: []nostr.Event{inEv, outEv}}
	filter := nostr.Filter{
		Kinds:  []nostr.Kind{31923},
		Search: "lernen",
		Tags: nostr.TagMap{
			"start_after":  []string{"1718000000"},
			"start_before": []string{"1719000000"},
		},
	}
	got := collectEvents(calendarRerankQuery(context.Background(), filter, searcher, store.fetch, 200, nostr.Generate()))
	ids := idsOf(got)
	if len(ids) != 1 || ids[0] != inEv.ID.Hex() {
		t.Fatalf("want only in-window event, got ids %v", ids)
	}
	// every filter handed to fetch must carry no synthetic range tags
	for i, c := range store.calls {
		for _, k := range []string{"start_after", "start_before", "end_after", "end_before"} {
			if _, ok := c.Tags[k]; ok {
				t.Errorf("fetch call %d still carries range tag %q", i, k)
			}
		}
	}
}
```

> Verified against `query_combined_integration_test.go`: `fakeChunkSearcher{hits []semantic.ChunkHit, called bool, ...}`, `fakeStore{events, calls}` whose `fetch` applies `filter.Matches` (which ignores `Search`, so the kind-31923 events pass), `semantic.ChunkHit{EventID, EventCoord, Score, Snippet}`, plus `coordFor`/`idsOf`/`collectEvents`. Relay secret keys are made with `nostr.Generate()` (no `testRelaySK` helper exists). `ChunkRerankQuery` resolves parents from the lexical fetch body cache, so with both events returned lexically there is exactly ONE `store.calls` entry (the line-151 lexical fetch with the stripped filter); no id-only fetch occurs.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test . -run TestCalendarRerank -v`
Expected: FAIL with "undefined: calendarRerankQuery"

- [ ] **Step 3: Write minimal implementation**

Grow the import block in `calendar_rerank.go` to add `context`:

```go
import (
	"context"
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/semantic"
	"fiatjaf.com/nostr/nip52"
)
```

Add the function:

```go
// calendarRerankQuery wraps semantic.ChunkRerankQuery so a calendar topic+time
// REQ is semantically reranked AND time-windowed. The shared rerank path
// post-filters with filter.Matches, which rejects calendar events whenever the
// synthetic range params (start_after/…) are present (they are query params,
// not real event tags). So for a windowed calendar search we strip those four
// keys before rerank, take the full reranked pool (Limit=0), and post-window
// relay-side. Searches without a window — or non-calendar searches, or
// geohash (Bolt-only) searches — are left to the plain paths.
func calendarRerankQuery(ctx context.Context, filter nostr.Filter, searcher semantic.ChunkSearcher, fetch fetchFunc, maxLimit int, sk nostr.SecretKey) iter.Seq[nostr.Event] {
	if !calendar.HasCalendarKinds(filter) {
		return semantic.ChunkRerankQuery(ctx, filter, searcher, fetch, maxLimit, sk)
	}
	cf := calendar.ExtractCalendarFilter(filter)
	if len(cf.Geohashes) > 0 {
		// geohash-prefix is Bolt-only; rerank can't serve it.
		return fetch(filter, maxLimit)
	}
	if cf.StartAfter == 0 && cf.StartBefore == 0 && cf.EndAfter == 0 && cf.EndBefore == 0 {
		return semantic.ChunkRerankQuery(ctx, filter, searcher, fetch, maxLimit, sk)
	}
	limit := filter.Limit
	if limit <= 0 || limit > maxLimit {
		limit = maxLimit
	}
	stripped := calendar.RemoveCalendarTags(filter)
	stripped.Limit = 0 // take the full reranked pool; windowCalendar re-caps.
	ranked := semantic.ChunkRerankQuery(ctx, stripped, searcher, fetch, maxLimit, sk)
	return windowCalendar(ranked, cf, limit)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `GOWORK=off go test . -run TestCalendarRerank -v`
Expected: PASS (all four dispatcher tests)

- [ ] **Step 5: Register calendar `chunked:true`**

In `amb-relay/main.go:426-444`, the calendar `contentType` literal lacks a `chunked` field. Add `chunked: true,`. Read the literal first to place the field consistently with the other fields; example:

```go
	calendarType := contentType{
		name:    "calendar",
		kinds:   []nostr.Kind{31922, 31923, 31924, 31925},
		chunked: true,
		// …existing fields (fetch: calendarFetch(...), store, schema, etc.)…
	}
```

> Match the actual struct literal — only ADD the `chunked: true,` line; do not reorder or rewrite existing fields.

- [ ] **Step 6: Wire the dispatcher into QueryStored**

In `amb-relay/main.go:819-836`, the rerank branch currently reads (around line 832-833):

```go
		if reg.targetsChunked(filter) && searchHasFreeText(filter.Search) {
			return semantic.ChunkRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)
		}
```

Swap the call to the calendar-aware dispatcher (it delegates to `ChunkRerankQuery` for every non-windowed/non-calendar case, so non-calendar searches are unaffected):

```go
		if reg.targetsChunked(filter) && searchHasFreeText(filter.Search) {
			return calendarRerankQuery(ctx, filter, chunkSearcher, reg.fetch, maxLimit, relaySK)
		}
```

> Confirm the exact identifier names at this site (`chunkSearcher`, `reg.fetch`, `maxLimit`, `relaySK`) by reading main.go:819-836 and use them verbatim. `reg.fetch` has signature `fetchFunc`.

- [ ] **Step 7: Build + run the full relay suite**

Run: `GOWORK=off go build . && GOWORK=off go test ./...`
Expected: build OK; PASS (no regressions; calendar rerank + window tests green).

- [ ] **Step 8: Commit**

```bash
git add calendar_rerank.go calendar_rerank_test.go main.go
git commit -m "$(cat <<'EOF'
feat(calendar): route calendar searches through window-aware chunk rerank

Register calendar chunked:true and add calendarRerankQuery: strips synthetic
range params before ChunkRerankQuery (filter.Matches would reject them), then
post-windows the reranked pool relay-side. Non-calendar and windowless
searches fall through to plain ChunkRerankQuery unchanged.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Testing Summary

- **Indexer (Task 1):** `go test ./...` — 31923 routed to content-direct, coord `31923:<pk>:<d>`, embedding+snippet present with `permissive=false`, no SetContent call, no dead-letter.
- **Relay window predicate (Task 2):** each bound pass/fail + inclusive edges + missing-field-fails.
- **Relay window wrapper (Task 3):** drop out-of-window, keep/drop snippet with its parent, apply limit (parents only), pass non-calendar through.
- **Relay dispatcher (Task 4):** geohash→fetch passthrough; calendar-topic-only→plain rerank; non-calendar→plain rerank; topic+time→stripped+windowed.
- All relay commands use `GOWORK=off`.

## Deploy (manual, after merge — dev only)

Both services rebuilt to **dev only** (`dev.amb-relay.edufeed.org`). Calendar semantic engages only when `CHUNK_RERANK_ENABLED=true` and `INDEXER_API_TOKEN` is set on the relay. A backfill reindex on the indexer chunks existing calendar events (fresh writes chunk automatically once subscribed). Production is a later, separately-authorized step.
