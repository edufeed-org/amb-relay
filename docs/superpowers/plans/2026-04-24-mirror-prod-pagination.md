# Paginate `cmd/mirror-prod` so it walks the full source corpus

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make `amb-indexer/cmd/mirror-prod` mirror the *entire* source-relay corpus rather than only the newest 250 events, by looping on `until=<oldest-seen created_at>` until a page returns no new events.

**Architecture:**
- Wrap the existing single-call `pool.FetchMany` in a pagination loop. Each iteration REQs `{kinds, since, until, limit=page_size}`, publishes events to `dst`, then sets `until = oldest_seen_created_at` for the next iteration. Events are deduped across pages by `id` so identical `created_at` at page boundaries doesn't cost us data.
- Termination: loop ends when a page yields **zero new** events (after dedupe) or an overall safety cap of N pages is hit. The common case is "page has fewer events than requested" — also terminates, since there is no need for another REQ.
- Backward-compatible CLI: `--limit` becomes the per-REQ page size (default 500, which prod silently caps at 250). Adds `--max-pages` safety flag (default 200) and optional `--max-events` overall cap for dry-run/testing. `--since` still works as a lower time bound and is passed through on every page.

**Tech stack:** Go 1.23+, `fiatjaf.com/nostr` (`pool.FetchMany`, `nostr.Filter`), in-process khatru relays + slicestore for testing.

---

## Context

The previous comparison plan ("Mirror prod + side-by-side comparison UI") shipped a one-shot `cmd/mirror-prod`. During the UI-walk-through check we discovered it is **not fully syncing** the corpus:

```
prod   kind-30142:  8672
local  kind-30142:   250
```

Root cause: `mirror(ctx, src, dst, cfg)` in `amb-indexer/cmd/mirror-prod/main.go:38-75` opens a single `pool.FetchMany` with at most `filter.Limit` events. Prod's khatru caps each REQ at 250, so the tool walks away thinking "EOSE, done" after the newest 250. Older events (8.4k of them, going back to 2017) are silently dropped.

This also biases the side-by-side `/ui` comparison: the local-chunk search only has ~67 chunks from 16 events, while the prod-REQ column sees all 8672 live. The qualitative conclusion ("chunk beats name/desc search per event") still holds, but hit-count comparisons across the two columns aren't apples-to-apples until local catches up.

Khatru relays do not implement NIP-45 `COUNT` on our deployed version, but `nak count` against prod does return a number (8672) — so we can verify success post-mirror by comparing `nak count -k 30142 ws://localhost:3334` to the prod figure.

## File inventory

### Modified files

| File | Change |
|---|---|
| `amb-indexer/cmd/mirror-prod/main.go` | Wrap `FetchMany` in a pagination loop; add `MirrorConfig.PageSize`, `MaxPages`, `MaxEvents`; adjust progress logging |
| `amb-indexer/cmd/mirror-prod/main_test.go` | Add `TestMirror_PaginatesAcrossPageBoundary` + `TestMirror_DedupesAcrossPages`; existing tests unchanged |
| `amb-indexer/README.md` | Document pagination behaviour, new flags |

### No new files.

---

## Task 1 — Pagination loop

**Files:**
- Modify: `amb-indexer/cmd/mirror-prod/main.go`
- Modify: `amb-indexer/cmd/mirror-prod/main_test.go`

- [ ] **Step 1.1: Write the failing pagination test**

Append to `main_test.go`:

```go
func TestMirror_PaginatesAcrossPageBoundary(t *testing.T) {
	srcURL, srcPublish, srcCleanup := newTestRelay(t)
	defer srcCleanup()
	dstURL, _, dstCleanup := newTestRelay(t)
	defer dstCleanup()

	sk := nostr.Generate()
	// 7 distinct events, strictly decreasing created_at so page-boundary logic
	// (until = oldest_seen) exercises cleanly.
	const total = 7
	for i := 0; i < total; i++ {
		srcPublish(signedEvent(t, sk, fmt.Sprintf("d%d", i), int64(1_700_000_100-i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pub, errs, err := mirror(ctx, srcURL, dstURL, MirrorConfig{PageSize: 3, MaxPages: 10})
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if errs != 0 {
		t.Errorf("errs = %d, want 0", errs)
	}
	if pub != total {
		t.Errorf("published = %d, want %d", pub, total)
	}
}
```

(Reuse the existing `newTestRelay` / `signedEvent` helpers at `main_test.go:18-73`. Add `"fmt"` to imports.)

Run: `cd amb-indexer && GOWORK=off go test -run TestMirror_Paginates -v ./cmd/mirror-prod`
Expected: FAIL — either `unknown field PageSize in struct literal` or `published = 3, want 7`.

- [ ] **Step 1.2: Write the failing dedupe-at-boundary test**

```go
func TestMirror_DedupesAcrossPages(t *testing.T) {
	srcURL, srcPublish, srcCleanup := newTestRelay(t)
	defer srcCleanup()
	dstURL, _, dstCleanup := newTestRelay(t)
	defer dstCleanup()

	sk := nostr.Generate()
	// Two events share created_at exactly at the boundary:
	//   page 1 (limit=2, until=now) returns e1, e2 where created_at(e1) ==
	//   created_at(e2). The loop sets until = oldest = that shared second.
	//   Page 2 MUST also include e1 again (relay re-emits it, same second) —
	//   dedupe drops it, and e0 is the only new event. After page 3 the cursor
	//   has passed below the corpus and we exit.
	srcPublish(signedEvent(t, sk, "e0", 1_700_000_050))
	srcPublish(signedEvent(t, sk, "e1", 1_700_000_100))
	srcPublish(signedEvent(t, sk, "e2", 1_700_000_100))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pub, errs, err := mirror(ctx, srcURL, dstURL, MirrorConfig{PageSize: 2, MaxPages: 10})
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if errs != 0 {
		t.Errorf("errs = %d", errs)
	}
	// "published" counts unique events delivered to dst — not re-emitted dupes.
	if pub != 3 {
		t.Errorf("published = %d, want 3", pub)
	}
}
```

Run: same command. Expected: FAIL (same reasons as 1.1, plus will likely hang or loop infinitely without dedupe termination).

- [ ] **Step 1.3: Add fields to `MirrorConfig`**

In `main.go`, extend `MirrorConfig` (lines 24-33):

```go
type MirrorConfig struct {
	Kinds  []nostr.Kind
	Since  nostr.Timestamp
	// PageSize is the per-REQ event cap. 0 → default 500. Source relay
	// may cap it lower (prod khatru caps at 250); pagination loop handles
	// that transparently.
	PageSize uint
	// MaxPages bounds the pagination loop so a misbehaving source can't
	// cause unbounded work. 0 → default 200.
	MaxPages uint
	// MaxEvents caps total events mirrored this run. 0 → unlimited.
	MaxEvents uint
	DryRun    bool
}
```

Remove the old `Limit` field entirely — there are only two callers (the CLI and the tests) and both move to `PageSize`.

- [ ] **Step 1.4: Implement the pagination loop**

Replace the body of `mirror` (main.go:38-75) with:

```go
func mirror(ctx context.Context, srcURL, dstURL string, cfg MirrorConfig) (int, int, error) {
	pool := nostr.NewPool(nostr.PoolOptions{})

	var dst *nostr.Relay
	if !cfg.DryRun {
		var err error
		dst, err = nostr.RelayConnect(ctx, dstURL, nostr.RelayOptions{})
		if err != nil {
			return 0, 0, fmt.Errorf("connect dst %s: %w", dstURL, err)
		}
		defer dst.Close()
	}

	kinds := cfg.Kinds
	if len(kinds) == 0 {
		kinds = []nostr.Kind{30142}
	}
	pageSize := cfg.PageSize
	if pageSize == 0 {
		pageSize = 500
	}
	maxPages := cfg.MaxPages
	if maxPages == 0 {
		maxPages = 200
	}

	seen := make(map[nostr.ID]struct{}) // dedupe across pages
	var (
		published, errs int
		until           nostr.Timestamp // 0 → "now"
	)

	for page := uint(0); page < maxPages; page++ {
		filter := nostr.Filter{
			Kinds: kinds,
			Since: cfg.Since,
			Limit: int(pageSize),
		}
		if until > 0 {
			filter.Until = until
		}

		ch := pool.FetchMany(ctx, []string{srcURL}, filter, nostr.SubscriptionOptions{})

		var (
			pageCount    int // total events this page (incl. dupes)
			pageNew      int // first-time-seen events this page
			pageOldest   nostr.Timestamp
			pageHasOldest bool
		)
		for re := range ch {
			pageCount++
			if _, dup := seen[re.Event.ID]; dup {
				continue
			}
			seen[re.Event.ID] = struct{}{}
			pageNew++

			if !pageHasOldest || re.Event.CreatedAt < pageOldest {
				pageOldest = re.Event.CreatedAt
				pageHasOldest = true
			}

			if cfg.DryRun {
				published++
			} else if err := dst.Publish(ctx, re.Event); err != nil {
				log.Printf("publish %s: %v", re.Event.ID.Hex(), err)
				errs++
			} else {
				published++
			}

			if cfg.MaxEvents > 0 && uint(published) >= cfg.MaxEvents {
				log.Printf("mirror: hit MaxEvents=%d, stopping", cfg.MaxEvents)
				return published, errs, nil
			}
		}

		log.Printf("mirror page %d: got=%d new=%d published=%d oldest=%d",
			page, pageCount, pageNew, published, pageOldest)

		// Terminate: relay said nothing matches.
		if pageCount == 0 {
			return published, errs, nil
		}
		// Short page with new events = end of stream.
		if pageNew > 0 && uint(pageCount) < pageSize {
			return published, errs, nil
		}

		// Advance cursor. Default: reuse the oldest seen second so events
		// sharing that second aren't missed (dedupe handles the relay
		// re-emitting them). But if THIS page was 100% dupes, the relay is
		// stuck re-emitting the same boundary second — step past it,
		// otherwise we'd loop forever.
		//
		// Limitation: if more than pageSize events share one created_at AND
		// we've already seen pageSize of them, the remaining ones at that
		// second are lost. With pageSize=500 against the prod corpus this is
		// effectively impossible.
		if pageNew == 0 {
			until = pageOldest - 1
		} else {
			until = pageOldest
		}
	}

	log.Printf("mirror: hit MaxPages=%d, stopping (may be incomplete)", maxPages)
	return published, errs, nil
}
```

- [ ] **Step 1.5: Update the CLI**

Replace the `main()` flag block (main.go:78-86) with:

```go
var (
	srcFlag  = flag.String("src", "wss://amb-relay.edufeed.org", "source relay ws URL")
	dstFlag  = flag.String("dst", "ws://localhost:3334", "destination relay ws URL")
	since    = flag.Int64("since", 0, "only mirror events with created_at > this unix timestamp")
	pageSize = flag.Uint("page-size", 500, "per-REQ event cap (source relay may cap lower)")
	maxPages = flag.Uint("max-pages", 200, "safety cap on pagination loop")
	maxEvts  = flag.Uint("max-events", 0, "overall cap on events mirrored (0 = unlimited)")
	dry      = flag.Bool("dry-run", false, "subscribe and count but do not publish")
)
flag.Parse()

ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()

start := time.Now()
pub, errs, err := mirror(ctx, *srcFlag, *dstFlag, MirrorConfig{
	Since:     nostr.Timestamp(*since),
	PageSize:  *pageSize,
	MaxPages:  *maxPages,
	MaxEvents: *maxEvts,
	DryRun:    *dry,
})
```

Update the existing `log.Printf("mirror done: ...")` to include `pages` if cheap; otherwise leave it alone.

Keep the `--limit` flag removed. A single-shot user who liked the old behaviour can pass `--max-events 250`.

- [ ] **Step 1.6: Update existing tests to use the new field name**

The two existing tests (`TestMirror_CopiesAllEvents`, `TestMirror_DryRunPublishesNothing`) pass `MirrorConfig{Limit: 10}` / `{Limit: 10, DryRun: true}`. Change both to `MirrorConfig{PageSize: 10}` / `{PageSize: 10, DryRun: true}`. No other logic changes.

- [ ] **Step 1.7: Run the full test package**

```
cd amb-indexer
GOWORK=off go test -race -count=1 ./cmd/mirror-prod -v
```

Expected: all four tests PASS (two existing + two new).

- [ ] **Step 1.8: Vet + fmt**

```
GOWORK=off go vet ./cmd/mirror-prod
gofmt -l cmd/mirror-prod
```

Both clean.

- [ ] **Step 1.9: Commit**

```
git add cmd/mirror-prod/main.go cmd/mirror-prod/main_test.go
git commit -m "Paginate mirror-prod so it walks the full source corpus

Prod khatru caps each REQ at 250 events, so a single FetchMany walked
away with only the newest slice (~2.9% coverage of the 8672-event
corpus). Wrap FetchMany in a cursor loop: set until=oldest_seen after
each page, dedupe by id across pages so events sharing a second at
the boundary aren't lost, terminate when a page returns no new events
or fewer events than the per-REQ cap.

Renames MirrorConfig.Limit -> PageSize and adds MaxPages + MaxEvents
safety caps. CLI grows --page-size / --max-pages / --max-events;
--limit is removed (set --max-events to reproduce the old cap)."
```

---

## Task 2 — Smoke-verify against prod

- [ ] **Step 2.1: Dry-run against prod**

```
cd amb-indexer
GOWORK=off go run ./cmd/mirror-prod --src wss://amb-relay.edufeed.org --dry-run
```

Expected: multiple `mirror page N: ...` lines, final `mirror done: published=~8672 ...`. Allow ±1% for concurrent writes.

- [ ] **Step 2.2: Fresh local stack, real mirror**

```
cd /home/laoc/coding/edufeed/amb-relay
docker compose down -v
docker run --rm -v "$(pwd)/typesense-data:/data" alpine rm -rf /data/*
docker compose up -d --build
curl -sf http://localhost:18080/ready   # wait until true

cd /home/laoc/coding/edufeed/amb-indexer
GOWORK=off go run ./cmd/mirror-prod --src wss://amb-relay.edufeed.org --dst ws://localhost:3334
```

Expected: `mirror done: published=~8672 errors=0 elapsed=<a few minutes>`.

- [ ] **Step 2.3: Verify count parity**

```
nix-shell -p nak --run 'nak count -k 30142 ws://localhost:3334'
nix-shell -p nak --run 'nak count -k 30142 wss://amb-relay.edufeed.org'
```

Expected: both within 1% of each other. (Exact parity is unlikely because prod may ingest new events while we mirror.)

- [ ] **Step 2.4: Wait for indexer drain, then re-walk the `/ui`**

Per the prior plan's verification: watch `indexer_chunks_indexed_total`, replay any `"no source URL"` dead-letters with `/admin/dead_letter/replay_all`, then open <http://localhost:18080/ui> and retry the suggested queries. Now both sides are searching the same corpus, so hit-count comparisons are meaningful.

- [ ] **Step 2.5: Amend comparison notes in architecture.md**

Update `/home/laoc/coding/edufeed/amb-relay/docs/architecture.md` "Comparison notes" section with the full-corpus numbers. Keep the prior (biased 250-event) table too if useful, but label it "initial single-page snapshot" and add a "full-corpus" section underneath.

- [ ] **Step 2.6: Commit docs + README update**

```
# In amb-indexer:
git add README.md
git commit -m "Document mirror-prod pagination, --page-size / --max-pages flags"

# In amb-relay:
git add docs/architecture.md
git commit -m "Refresh comparison notes with full-corpus numbers"
```

---

## Out of scope

- Two-way or incremental sync. This remains a one-shot pull.
- Source-relay-specific quirks beyond the REQ cap (e.g. rate limits, per-author partitioning).
- Persistent cursor state across runs. If a run aborts, re-run from scratch — the dst relay dedupes by id so it's cheap.
- Replacing the ad-hoc `since`/`until` cursor with negentropy. Prod's khatru supports negentropy but the complexity isn't justified for a dev-stage tool; revisit if 8.6k events become 100k.
- UI changes in `amb-indexer/ui.html`. Once the local corpus catches up, the existing UI is already apples-to-apples.

## Verification

- **Unit + race:** `cd amb-indexer && GOWORK=off go test -race ./cmd/mirror-prod` green.
- **Dry-run prod count:** `mirror done: published=N` where `N >= 8000` (full corpus ± concurrent writes).
- **Real mirror parity:** `nak count -k 30142 ws://localhost:3334` within 1% of the prod count.
- **Indexer ingestion:** `indexer_events_processed_total{outcome="indexed"}` grows by the count of permissive-licensed events (subset of 8672; majority will still be `license_denied` per the known prod-corpus gap).
- **UI apples-to-apples:** running the same query on both columns of `/ui` surfaces hit counts that differ by *ranking quality*, not *corpus size*.

## Critical files to modify

- `amb-indexer/cmd/mirror-prod/main.go` — pagination loop, flag surface
- `amb-indexer/cmd/mirror-prod/main_test.go` — two new tests; existing tests rename field
- `amb-indexer/README.md` — flag docs
- `amb-relay/docs/architecture.md` — comparison-notes refresh

## Reuse map

| Existing | Reused in |
|---|---|
| `pool.FetchMany` (current main.go:60) | pagination loop (inside for-loop) |
| `nostr.RelayConnect + Publish` (current main.go:44-67) | unchanged per-event publish path |
| `newTestRelay` / `signedEvent` (main_test.go:18-73) | two new tests |
| `nak count` (already installed via nix-shell) | prod/local parity check in Task 2.3 |
