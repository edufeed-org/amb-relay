# Calendar Absorption (NIP-52) — Design

**Date:** 2026-06-17
**Status:** Approved (design); implementation plan to follow
**Phase:** Multi-content-search platform, Phase 6 (calendar)

## Goal

Merge `calendar-relay` (NIP-52 kinds 31922/31923/31924/31925) into `amb-relay`
as a `CALENDAR_ENABLED`-gated content type, **dual-indexed**:

- **Typesense** (`calendar_31922` collection) — unified full-text + kind/tag
  search, consistent with AMB/long-form/wiki.
- **BoltDB start/end/geohash index** (the existing
  `nostrlib/khatru/calendar` package) — fast range & location queries.

Once proven in production, `calendar-relay` is retired.

### Feature intent (why)

The end goal is to make calendar data **easy for MCP servers and LLM-based
services to query** — "which events take place next week?", "near my
location?", "about topic X?". Those three intents map exactly onto the three
index dimensions (time range, geohash, full-text/tag), which is what motivates
the dual-index design. **This spec delivers the queryable primitives and
documents the canonical query shapes.** The natural-language → filter
translation layer (an MCP server with `events_in_range` / `events_near` /
`events_by_topic` tools doing date-parsing and lat,lon→geohash) is a **separate
follow-up sub-project** (its own spec), not part of this plan.

## Architecture

The work slots into amb-relay's existing kind-dispatched registry
(`content_registry.go`) exactly the way long-form (30023) and wiki (30818)
did. **No `nostrlib` changes are required** — every calendar primitive needed
is already exported (`calendar.NewCalendarStore`, `CalendarStore.GetIndex`,
`CalendarIndex.IndexEvent`/`RemoveEvent`, `CalendarStore.QueryEvents`/
`CountEvents`, `nip52.ParseCalendarEvent`).

```
StoreEvent ─┬─ boltBuf.Queue            (raw persistence, already present)
            └─ reg.store(calendar) ─┬─ calStore.GetIndex().IndexEvent   (Bolt: start/end/geohash)
                                    └─ storeStructured → tsDB4          (Typesense projection)

QueryStored ── reg.fetch(calendar) ── router:
                 calendar params + event kinds → calStore.QueryEvents   (Bolt range/geo)
                 otherwise                      → tsDB4.QueryEvents      (Typesense text/kind/tag)
```

### New backends (main.go, gated behind `CALENDAR_ENABLED`)

- `tsDB4 *typesense30142.TSBackend` — collection from `TS_COLLECTION_CALENDAR`
  (default `calendar_31922`), `RawEventStore: &boltDB`, `Schema: &calSchema`.
  Inited exactly like `tsDB2`/`tsDB3`; `nil` when the flag is off.
- `calStore, _ := calendar.NewCalendarStore(&boltDB, calendarIndexPath)` where
  `calendarIndexPath` comes from `CALENDAR_INDEX_PATH` (default
  `./data/calendar_index.db`).

**Lifecycle caution:** we use `calStore` *only* for `GetIndex()` (index
maintenance) and `QueryEvents` (range/geo). We **never** call
`calStore.SaveEvent` (boltBuf already persists the raw event) nor
`calStore.Close()` (it closes the shared `boltDB` too). Shutdown does
`defer calStore.GetIndex().Close()` — closing the index DB only.

### The registry contentType

Appended to `contentTypes` only when `CALENDAR_ENABLED && tsDB4 != nil`:

| field | wiring |
|---|---|
| `kinds` | `31922, 31923, 31924, 31925` |
| `validate` | `validateCalendar` (amb-relay-local; mirrors nostrlib `validateNIP52Event`, in the style of `validateWiki`) |
| `store` | `calStore.GetIndex().IndexEvent(e)` (no-ops on 31924/31925) **+** `storeCalendar(true, tsDB4, e)` → `storeStructured(..., nostrToCalendar)` |
| `fetch` | `calendarFetch` router (below) |
| `count` | `tsDB4.CountEvents` |
| `deleteID` | `tsDB4.DeleteEvent(id)` **and** `calStore.GetIndex().RemoveEvent(id)` |

`reg.kinds()` automatically advertises the four kinds in NIP-11 retention.

### The fetch router (`calendar.go`)

```go
func calendarFetch(calStore *calendar.CalendarStore, tsDB4 *typesense30142.TSBackend) fetchFunc {
    return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
        calFilter := calendar.ExtractCalendarFilter(filter)
        if calFilter.HasCalendarParams() && calendar.HasCalendarKinds(filter) {
            return calStore.QueryEvents(filter, maxLimit) // Bolt range/geo
        }
        return tsDB4.QueryEvents(filter, maxLimit)         // Typesense text/kind/tag
    }
}
```

`HasCalendarKinds` is true for 31922/31923 (or no kind constraint); 31924/31925
always fall to the Typesense path (they carry no start/geohash to index).

### Projection (`calendar.go`, mirrors `longform.go`)

```go
type CalendarDocument struct {
    ID       string `json:"id"`
    D        string `json:"d"`
    Title    string `json:"title"`
    Summary  string `json:"summary,omitempty"`
    Content  string `json:"content,omitempty"`
    Location string `json:"location,omitempty"`
    Start    int64  `json:"start,omitempty"`
    End      int64  `json:"end,omitempty"`
    Geohash  string `json:"geohash,omitempty"`
    Status   string `json:"status,omitempty"` // RSVP: accepted/declined/tentative
    structuredEnvelope
}
```

`nostrToCalendar(event)`:
- requires a `d` tag (addressable; error if missing, like `nostrToLongform`).
- `ID = GenerateDocumentID(pubkey, dTag)`.
- For 31922/31923: use `nip52.ParseCalendarEvent` to fill `Title`, `Start`/`End`
  (`.Unix()`; date-based parsed via `nip52.DateFormat`), first `Location`, first
  `Geohash`. `Content = event.Content`.
- For 31924 (calendar): `Title` from tag; no times.
- For 31925 (RSVP): `Status` from `status` tag; `Content` from event.Content.
- Reads `summary` tag directly.

`calendarSchema(name)` mirrors `longformSchema`: `DefaultSortingField:
"eventCreatedAt"`; fields `id, d, title, summary(opt), content(opt),
location(opt), start(int64,opt,facet), end(int64,opt,facet),
geohash(string,opt,facet), status(string,opt,facet)` + `structuredEnvelopeFields()`.

### Validation (`calendar.go`)

`validateCalendar(event)` dispatches by kind (mirrors nostrlib
`validateNIP52Event`):
- 31922/31923: require non-empty `d`, `title`, `start`.
- 31924: require non-empty `d`, `title`.
- 31925: require non-empty `d`, `a`, and `status` ∈ {accepted, declined, tentative}.

### Reindex (no `reindex.go` change)

Append one `structuredReindexTarget` in main.go (next to the longform/wiki
targets):

```go
if calendarEnabled && tsDB4 != nil {
    structuredTargets = append(structuredTargets, structuredReindexTarget{
        label:    "calendar",
        kinds:    []nostr.Kind{31922, 31923, 31924, 31925},
        recreate: func() error { return tsDB4.RecreateCollection(tsDB4.Schema) },
        reproject: func(e nostr.Event) error {
            if calendar.IsCalendarEventKind(e.Kind) {
                if err := calStore.GetIndex().IndexEvent(e); err != nil {
                    return err
                }
            }
            return reprojectStructured(tsDB4, e, nostrToCalendar)
        },
    })
}
```

A single BoltDB pass over the four kinds rebuilds **both** the Typesense
collection and the Bolt index. Known caveat (same as calendar-relay's
`RebuildIndex`): the Bolt index is not cleared first, so index entries for
events no longer in BoltDB persist as harmless orphans until the index file is
recreated.

## Canonical query shapes (the LLM/MCP contract)

These are the filters the future MCP server emits. Documented here so the spec's
success criteria are concrete and Spec B has a fixed target. Custom calendar
filter tags travel as `#`-prefixed keys in `filter.Tags`.

**1. Events in the next week** (time-based, 31923):
```json
{"kinds":[31923], "#start_after":["1718600000"], "#start_before":["1719204800"], "limit":100}
```
→ Bolt time index (`QueryByStartRange`). Date-based (31922) works identically;
the index stores `start` as Unix (date parsed via `YYYY-MM-DD`).

**2. Events near a location** (geohash prefix):
```json
{"kinds":[31922,31923], "#g":["u33d"], "limit":100}
```
→ Bolt geohash index (`QueryByGeohashPrefix`). The MCP server converts
lat/lon → geohash; prefix length sets the radius.

**3. Events about a topic** (full-text / hashtag):
```json
{"kinds":[31922,31923,31924], "search":"mathematik", "limit":50}
```
→ Typesense full-text over `title`/`summary`/`content`/`location`. Hashtag
filtering via `#t` works too.

**Combined intent** ("next week, near Berlin, about math"): a single relay REQ
prioritizes the Bolt range/geo path and ignores `search` (see Precedence). The
MCP server composes combined intent client-side — e.g. issue the Bolt range/geo
REQ, then post-filter by topic against the returned events (the full event is
always available via `eventRaw`). This composition is cheap because calendar
result sets per range/geo are small, and it keeps the relay simple.

## Query precedence (known limitation)

When a filter carries **both** calendar range/geo params **and** a `search`
term, the router takes the Bolt range/geo path and the full-text term is
ignored for that REQ. This matches real calendar usage (range/geo dominate) and
is resolved by MCP-side composition. A future relay enhancement could translate
calendar params into Typesense `filter_by` so one REQ serves combined queries
server-side; explicitly out of scope here (YAGNI).

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CALENDAR_ENABLED` | `false` | Accept NIP-52 kinds; enable tsDB4 + Bolt index |
| `TS_COLLECTION_CALENDAR` | `calendar_31922` | Typesense collection name |
| `CALENDAR_INDEX_PATH` | `./data/calendar_index.db` | BoltDB calendar index file |

Flag off ⇒ no tsDB4, no calStore, empty reindex target, registry unchanged —
byte-for-byte identical to today.

## Out of scope (follow-ups)

- **Data migration** `calendar-relay → amb-relay`: copy the existing
  `calendar-relay/data/events.db` events into amb-relay's BoltDB (negentropy
  sync or a BoltDB event copy) then run a reindex to populate Typesense + the
  Bolt index. Ops task, following the same parallel-staging approach used for
  the prod full-stack cutover.
- **MCP/LLM query server** (Spec B): tools `events_in_range`, `events_near`,
  `events_by_topic`; date parsing, lat/lon→geohash, topic search; combined-
  intent composition per above.

## Testing

- **Projection** unit tests for `nostrToCalendar`: 31922 (date `start`→Unix),
  31923 (time `start`→Unix), 31924 (title, no times), 31925 (status); missing
  `d` → error. Golden envelope bytes (eventRaw round-trips) alongside the
  existing `golden_test.go` cases.
- **Fetch router** test (fakes, like `query_combined_integration_test.go`):
  `#start_after` present → Bolt path; `search` present → Typesense path;
  31924/31925 → Typesense path.
- **Validation** table test for `validateCalendar` across the four kinds incl.
  rejection cases (missing d/title/start/a/status, bad status value).
- **Flag-off byte-identity:** `CALENDAR_ENABLED` unset ⇒ `structuredTargets`
  empty, registry has only AMB(+longform/wiki), no tsDB4 — assert the registry
  and reindex paths are unchanged.
