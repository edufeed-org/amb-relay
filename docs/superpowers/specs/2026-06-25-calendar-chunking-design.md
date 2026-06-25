# Calendar Semantic Search (Chunk Embeddings) — Design

**Date:** 2026-06-25
**Status:** Approved (Approach B)
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

1. **Subscribe to calendar kinds.** Add `31922, 31923` to the source `Kinds`
   (`amb-indexer/main.go:165`). Omit 31924 (calendar collection — just a list of
   refs) and 31925 (RSVP — status only, no meaningful free text).
2. **Route calendar through the content-direct path.** Extend the gate at
   `worker.go:72` so 31922/31923 take `processContentDirect` (no external
   fetch/Tika/setcontent — the body is in the event), like 30023/30818.
3. **Add `docFromCalendarEvent`.** A calendar-specific `ExtractedDoc` builder
   (sibling to `docFromLongformEvent` in `fetch_nostr.go`) assembling the chunk
   text from the calendar event's `title` tag + `summary` tag + `event.Content`
   (the description) + `location` tag(s), joined with newlines. `Kind` label
   `"nostr-calendar"`. **No ATX heading parsing** — calendar descriptions are
   plain text, not markdown, so `Headings` is empty (chunks carry no heading
   locator, same as wiki/Djot).
4. **License gate.** Calendar event listings normally carry no `license` tag.
   **Decision:** an absent license on a calendar kind (31922/31923) is treated
   as indexable — calendar listings are public event announcements, not
   licensed resources. Implement by short-circuiting the license gate in
   `processContentDirect` for calendar kinds (a present, explicitly-restrictive
   license is still honored if one ever appears).
5. Chunk coord is produced automatically as `31922:<pk>:<d>` / `31923:<pk>:<d>`
   (`schema.go:143`) — no change.

### amb-relay changes

1. **Register calendar `chunked:true`** (`main.go:426-444`), so
   `targetsChunked` routes calendar (and mixed `[30142,31923]`) searches into
   `ChunkRerankQuery`.
2. **Calendar window-aware rerank branch.** In the `QueryStored` rerank path
   (`main.go:819-835`), for a calendar search that carries range params:
   - Pass `ChunkRerankQuery` a filter with the **synthetic range tags removed**
     (kinds + search only), so `filter.Matches` (`rerank.go:229`) no longer
     rejects calendar events.
   - **Wrap the returned `iter.Seq`** to drop events outside the original
     window, then apply the limit after windowing. Each bound applies to its
     own field, mirroring `buildNostrFilterExpression`: `start_after`/
     `start_before` gate the event's parsed `start`; `end_after`/`end_before`
     gate its parsed `end`. Only the bounds present in the REQ are applied; an
     event missing the gated field (e.g. no `end`) fails that bound. Reuse
     `calendar.ExtractCalendarFilter` for the bounds and the same nip52 parse
     the projector uses for the event's start/end. Bounds are inclusive,
     matching the Bolt index and the Typesense numeric mapping.
   - Searches *without* a range window need no wrapping — they flow through
     `ChunkRerankQuery` unchanged.
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
| `docFromCalendarEvent` | amb-indexer | Build chunk text from a calendar event's tags + content |
| content-direct gate | amb-indexer | Route 31922/31923 to the content-direct chunk path |
| subscribed kinds | amb-indexer | Ingest calendar events |
| calendar `chunked:true` | amb-relay | Opt calendar into rerank |
| calendar window-wrapper | amb-relay | Strip synthetic range tags in; post-window results out |

## Testing

- **amb-indexer unit:** `docFromCalendarEvent` pulls title+summary+content+
  location into `Text`; coord is `31923:<pk>:<d>`; no headings.
- **amb-indexer unit:** `worker.go:72` gate routes 31922/31923 to
  `processContentDirect` (mirrors the existing 30023/30818 tests).
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
- A `nostrlib` change to make `ChunkRerankQuery` range-aware (Approach C) — the
  relay-only wrapper achieves the same result at this corpus size without
  touching the shared library.
- Storing start/end as filterable fields on chunk docs (indexer-side window) —
  unnecessary given relay-side post-windowing.
