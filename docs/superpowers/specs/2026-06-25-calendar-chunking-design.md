# Calendar Semantic Search (Chunk Embeddings) — Design

**Date:** 2026-06-25
**Status:** Approved (Approach B); revised after a DRY/KISS/YAGNI + nostrlib-reuse pass
**Repos:** `amb-indexer` (chunk pipeline) + `amb-relay` (query/rerank). No `nostrlib` change.

## Problem

NIP-52 calendar events (kinds 31922 date-based, 31923 time-based) are searchable
only **lexically** (Typesense full-text over the `calendar_31922` collection). A
conceptual query misses thematically-relevant events that don't contain the
literal query token. Concrete failure (dev, 2026-06-25): a search for
"Aktivierung Studierende" in the next 4 weeks missed the e-teaching.org event
*"SCALE-UP – Lehren und Lernen und der Raum hilft mit"*, whose description says
"aktivierendes und kooperatives Lernen" / "studentisches Lernen" but never the
word "Studierende".

Long-form (30023) and wiki (30818) already have semantic search: amb-indexer
chunks/embeds their content into the shared chunk collection
(`amb_chunks_30142`), and the relay re-ranks NIP-50 searches by best-matching
passage (`CHUNK_RERANK_ENABLED`). Calendar is the only content type left out —
the indexer never subscribes to calendar kinds, so they are never chunked.

## Goal

Give calendar kinds 31922/31923 the same semantic search as long-form/wiki, so a
conceptual topic query (with or without a time window) ranks events by meaning.
The motivating query — **topic + time window in one REQ** — must return
semantically-ranked results that are still correctly windowed.

## Why this is not a one-line change

Exploration of the existing rerank machinery (`amb-relay/content_registry.go`,
`amb-relay/main.go`, `nostrlib/khatru/semantic/rerank.go`,
`nostrlib/khatru/semantic/snippet.go`, `amb-indexer/worker.go`) found that most
of the path is already kind-generic, but two pieces block calendar:

1. **The `chunked` flag.** The rerank gate `registry.targetsChunked`
   (`content_registry.go:128-143`) routes a search to `ChunkRerankQuery` only
   when a targeted kind belongs to a content type registered `chunked:true`.
   Calendar is registered `chunked:false` (`main.go:426-444`), so calendar
   searches bypass rerank entirely — chunks would exist but never be consulted.

2. **`filter.Matches` vs. synthetic range params (the subtle blocker).**
   `ChunkRerankQuery` post-filters every candidate with `filter.Matches(e)`
   (`rerank.go:229`). Our calendar window params (`#start_after`,
   `#start_before`, `#end_after`, `#end_before`) are **synthetic query params,
   not real event tags**. `filter.Matches` treats `start_after` as a tag filter,
   finds no such tag on the event, and rejects it. So naively flipping
   `chunked:true` makes the combined topic+time query (the motivating case)
   return **zero results** whenever rerank is enabled.

What is already generic and needs no change:

- Chunk coords are `<kind>:<pubkey>:<d>` (`amb-indexer/schema.go:143`), so
  `31923:…` / `31922:…` are produced automatically.
- The rerank parent-fetch is an ID-only filter resolved across *all* registered
  content types (`content_registry.go:82-99`), so a calendar event id is found
  without new wiring.
- Snippet `parentKind` parses the coord prefix (`snippet.go:56-68`), so a
  `31923:` coord yields the correct `k` tag on kind-21142 snippet events.

## Approach (B): semantic ranking *under* the time window, relay-only

Deliver semantic ranking for combined topic+time without editing the shared
`nostrlib` rerank path. The relay owns calendar semantics, so it adapts the
filter going in and windows the results coming out.

### amb-indexer changes

The whole indexer side reduces to **one subscription plus one gate edit** — no
new doc-builder, no license code. Calendar reuses the long-form content-direct
path verbatim, exactly as wiki (30818) already does.

1. **Subscribe to calendar kinds.** Add `31922, 31923` to the source `Kinds`
   (`amb-indexer/main.go:165`). Omit 31924 (calendar list — just refs) and 31925
   (RSVP — status only, no free text).
2. **Reuse the content-direct path.** Extend the gate at `worker.go:72` so
   31922/31923 take `processContentDirect` alongside 30023/30818. That path
   already calls `docFromLongformEvent` (`worker.go:313`), which chunks
   `event.Content` (the calendar description — where the semantic signal lives)
   plus the `title` tag. **No `docFromCalendarEvent` is written.** Wiki already
   proves this reuse: 30818 runs the identical builder.
   - *Trade-off (accepted):* the `summary` and `location` tags do not enter the
     chunk text, the chunk `Kind` label reads `"nostr-long"`, and plain-text
     descriptions are scanned for ATX headings (a stray `#` line becomes a
     heading locator). None affects ranking of the motivating case — the
     SCALE-UP match ("aktivierendes … Lernen") is in `event.Content`. Folding
     summary/location into the text is a later, separately-motivated change
     (see Out of scope), not a prerequisite.
3. **No license handling.** Calendar listings carry no `license` tag, so
   `permissive=false`. That does **not** hide them: `BuildChunkDocs`
   (`schema.go:147-171`) always stores the embedding and the 200-rune snippet;
   only the full chunk `text` field is elided. `/search_chunks` returns
   non-permissive hits — it elides `text`, never the hit (`search.go:348`) — and
   the relay's chunk searcher sends only `q`/`k`/`kinds`, never a
   `license_permissive` filter (`searcher_http.go:47`). So calendar events rank
   semantically with no license code at all; the original "short-circuit the
   license gate" step is dropped (YAGNI).
4. Chunk coord is produced automatically as `31922:<pk>:<d>` / `31923:<pk>:<d>`
   (`schema.go:143`) — no change.

### amb-relay changes

1. **Register calendar `chunked:true`** (`main.go:426-444`), so
   `targetsChunked` routes calendar (and mixed `[30142,31923]`) searches into
   `ChunkRerankQuery`.
2. **Calendar window-aware rerank branch.** In the `QueryStored` rerank path
   (`main.go:819-835`), for a calendar search that carries range params:
   - Pass `ChunkRerankQuery` a filter with the **synthetic range tags removed**
     (kinds + search only). Confirmed blocker: `filter.Matches` folds every
     `Tags` key into a real event-tag lookup (`nostrlib/filter.go:61-65`), so a
     `start_after` key makes it reject every calendar event (`rerank.go:229`).
     Stripping the four range keys clears it.
   - **Reuse `calendar.ExtractCalendarFilter(filter)`** (exported,
     `khatru/calendar/filter.go`) to pull the four bounds, and the same
     `nip52.ParseCalendarEvent` the projector uses for the event's start/end.
   - **Wrap the returned `iter.Seq`** with a small inline predicate: keep an
     event iff, for every bound present in the REQ, the gated field exists and
     satisfies it inclusively — `start_after`/`start_before` gate `start`,
     `end_after`/`end_before` gate `end`; a missing gated field fails its bound.
     Apply the limit after windowing. Searches *without* a range window need no
     wrapping — they flow through `ChunkRerankQuery` unchanged.
   - *Do not reuse `khatru/calendar`'s unexported `matchesCalendarFilter`.* It
     treats a missing upper-bounded field as **passing** (`0 > EndBefore` is
     false), which diverges from the Typesense numeric path
     (`buildNostrFilterExpression`) that the rerank-disabled branch must match —
     there a doc missing `end` fails an `end:<=` clause. The ~8-line inline
     predicate keeps both branches identical; the borrowed one would be a latent
     parity bug. (`ExtractCalendarFilter` is genuinely shared and reused; only
     the predicate is deliberately not.)
3. **Parity when rerank is disabled.** When `CHUNK_RERANK_ENABLED` is off,
   `QueryStored` uses plain `reg.fetch` → `calendarFetch` → Typesense numeric
   window (today's shipped behavior). The new branch only engages when rerank is
   enabled. Both paths must return the same window-correct set; only ranking
   differs (semantic vs lexical).

### Data flow (topic + time, rerank enabled)

```
REQ {kinds:[31923], search:"X", #start_after, #start_before}
  → QueryStored: targetsChunked(calendar)=true, hasFreeText=true
  → strip range tags → filter' {kinds:[31923], search:"X"}
  → ChunkRerankQuery(filter', searcher, reg.fetch)
       → /search_chunks("X")  ranks topic matches semantically (all-time)
       → lexical fetch (filter') fuses RRF
       → parent bodies resolved by id across collections
       → filter'.Matches(e): passes (no synthetic tags)
  → relay window-wrapper: keep e where start/end ∈ [after,before]
  → apply limit
```

The candidate pool cap (`chunkSearchKMax=200`) is not a constraint: the calendar
corpus is ~3 k docs, so 200 topic candidates before windowing is ample.

## Components and boundaries

| Unit | Repo | Responsibility |
|------|------|----------------|
| subscribed kinds | amb-indexer | Add 31922/31923 to source kinds |
| content-direct gate | amb-indexer | Route 31922/31923 to the existing `docFromLongformEvent` path (no new builder) |
| calendar `chunked:true` | amb-relay | Opt calendar into rerank |
| calendar window-wrapper | amb-relay | Strip synthetic range tags in; reuse `ExtractCalendarFilter` + nip52 parse; post-window results out |

## Testing

- **amb-indexer unit:** the `worker.go:72` gate routes 31922/31923 to
  `processContentDirect`, producing chunks whose coord is `31923:<pk>:<d>` and
  whose embedding/snippet are present even with `permissive=false` (mirrors the
  existing 30023/30818 gate tests).
- **amb-relay unit:** the window predicate keeps in-window events and drops
  out-of-window ones per present bound, and a missing gated field fails its
  bound (the parity edge case vs. the Typesense numeric path).
- **amb-relay:** windowless topic search → routed to rerank (semantic).
- **amb-relay:** topic+time search → routed to rerank, results semantic **and**
  every returned event's start/end inside the window.
- **amb-relay:** rerank-disabled parity — topic+time returns the same windowed
  set via the Typesense numeric path.
- **amb-relay:** mixed kinds `[30142,31923]` still works (no calendar
  over-filtering of non-calendar results).

## Deploy

Both services rebuilt to **dev only** (`dev.amb-relay.edufeed.org`). Calendar
semantic engages only when `CHUNK_RERANK_ENABLED=true` and `INDEXER_API_TOKEN`
is set on the relay. A backfill reindex on the indexer side is needed to chunk
existing calendar events (fresh writes chunk automatically once subscribed).
Production is a later, separately-authorized step.

## Out of scope (YAGNI)

- Kinds 31924/31925 (no meaningful free text).
- A dedicated `docFromCalendarEvent` and folding `summary`/`location` into the
  chunk text — the reused long-form builder captures title + description, which
  carries the semantic signal. Add a calendar-specific builder only if
  summary/location prove to add unique recall.
- Treating calendar kinds as license-permissive (full passage text in
  snippets) — discovery already works without it; the 200-rune snippet suffices.
- A `nostrlib` change to make `ChunkRerankQuery` range-aware (Approach C) — the
  relay-only wrapper achieves the same result at this corpus size without
  touching the shared library.
- Storing start/end as filterable fields on chunk docs (indexer-side window) —
  unnecessary given relay-side post-windowing.
