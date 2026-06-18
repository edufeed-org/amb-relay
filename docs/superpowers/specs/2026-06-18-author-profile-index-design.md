# Author Profile Index — Design

**Date:** 2026-06-18
**Status:** Approved (design); implementation pending plan.

## Goal

Let clients resolve org/person **names** to **pubkeys** so name-driven queries
work — e.g. "which events does e-teaching run in the next two weeks?", "show
past and present events of Mediakontor Hamburg", "events from user X". The
relay indexes the kind-0 profiles of authors who have published content here,
and serves them via a normal NIP-50 `search` over `kinds:[0]`. The MCP (or any
client) maps a name → top pubkey(s), then issues the calendar/resource REQ with
`authors:[...]`.

This subsystem is the foundation; the calendar MCP consumer that uses it is a
separate follow-on.

## Why relay-side, kind-0 + NIP-50

- Calendar/resource events do **not** carry the author's display name, so
  name→pubkey resolution cannot be a NIP-50 field-filter on those events. A
  separate profile index is required.
- Serving profiles as **kind-0 events with NIP-50 `search`** is idiomatic
  Nostr and reuses the relay's existing query path (content-type registry +
  Typesense full-text) and the MCP's existing relay-query plumbing. No new
  wire protocol.
- The relay already maintains an **outbound `nostr.Pool`** (see
  `allowlist.go`), so fetching kind-0 from other relays is an established
  pattern here.

## Background: what already exists (reuse map)

| Need | Reuse | Location |
|------|-------|----------|
| Parse kind-0 JSON → struct | `sdk.ParseMetadata(event)` (pure; errors if kind≠0 or bad JSON) | `nostrlib/sdk/metadata.go` |
| Outbound fetch + refresh loop | AllowlistManager pattern: `nostr.Pool`, `Init()`, `StartRefreshLoop(interval)`, `Stop()` | `allowlist.go` |
| Typesense collection for a non-AMB kind | `typesense30142.TSBackend{}` with custom `Schema` + `SearchFields` | already done for longform/wiki/calendar in `main.go` |
| Synchronous projected-doc upsert | `structuredEnvelope`, `structuredEnvelopeFields`, `upsertStructuredDoc`, `storeStructured`, `reprojectStructured` | `structured.go` |
| Read path (NIP-50 over kind-0) | registry `fetch`/`selected`/`count` fan-out + dedup | `content_registry.go` |
| Startup backfill (enumerate content authors) | `boltDB.QueryEvents(filter, maxLimit) iter.Seq[nostr.Event]` | `nostrlib/eventstore/boltdb/query.go` |
| Batch kind-0 fetch | `pool.FetchMany(ctx, relays, filter, opts)` with `Authors:[…]` | `nostrlib/pool.go` |
| NIP-05 heuristic (deferred) | `nip05.QueryIdentifier`, `ProfileMetadata.NIP05Valid` | `nostrlib/nip05`, `sdk/metadata.go` |

**Not reused:** `sdk.System.FetchProfileMetadata` — it is the heavyweight
outbox-model client (needs Store, KVStore, MetadataCache, replaceableLoaders,
Publisher, hints). KISS dictates reusing only the pure `ParseMetadata` plus our
own pool fetch, mirroring how `allowlist.go` already fetches lists.

## Architecture

Three additions, all gated behind `PROFILES_ENABLED` (zero overhead when off,
mirroring the other content flags):

1. **`profiles.go`** — projection + storage. A `nostrToProfile(*nostr.Event)
   (*profileDoc, error)` projector (uses `sdk.ParseMetadata`), a `profileSchema`
   (Typesense `CollectionSchema`), and a thin `storeProfile` over the existing
   `structured.go` helpers.
2. **`profile_manager.go`** — `ProfileManager`, modeled 1:1 on
   `AllowlistManager`: owns a `nostr.Pool`, a durable candidate queue (BoltDB
   bucket), `Init()` (startup backfill), `StartRefreshLoop(interval)`,
   `Enqueue(pubkey)`, and `Stop()`.
3. **Wiring in `main.go`** — instantiate a `profilesDB *TSBackend`, register
   kind-0 in the content-type registry (read-only; see below), add one line to
   `relay.StoreEvent`/`relay.ReplaceEvent` to enqueue the author, and start/stop
   the manager.

### The registry read/write split (the one subtlety)

The registry couples write-`validate` and read-`fetch` by kind via `byKind`.
We want client kind-0 **writes rejected** but kind-0 **reads served**. The
content type for kind 0 is therefore:

```go
contentType{
    kinds:    []nostr.Kind{0},
    validate: func(nostr.Event) (bool, string) { return true, "kind not accepted" },
    store:    func(nostr.Event) {},          // never reached: validate rejects first
    fetch:    profilesDB.QueryEvents,
    count:    profilesDB.CountEvents,
    deleteID: profilesDB.DeleteEvent,
    chunked:  false,                          // kinds:[0] search → plain full-text
}
```

- `OnEvent` → `reg.validate` rejects any client kind-0 submission ("kind not
  accepted"), so `relay.StoreEvent` never runs for kind 0 and `store` is dead.
- The **ProfileManager writes to `profilesDB` directly** via `storeProfile`,
  bypassing the relay event path entirely. This is the deliberate split: the
  index is populated only by our internal fetcher, never by clients.
- Reads (`reg.fetch`/`selected`/`count`) work normally: a `{kinds:[0],
  search:"…"}` REQ fans out to `profilesDB.QueryEvents`.

## Data flow

```
Client writes content (30142/30023/30818/31922-31925)
   └─ relay.StoreEvent / relay.ReplaceEvent
        └─ profileMgr.Enqueue(event.PubKey)        [dedup'd, durable BoltDB bucket]

ProfileManager (gated behind PROFILES_ENABLED):
   ├─ Init(): startup backfill
   │     boltDB.QueryEvents({kinds: content kinds}) → distinct PubKeys → Enqueue each
   ├─ drain loop: take a batch of queued pubkeys
   │     pool.FetchMany(ctx, PROFILE_RELAYS, {kinds:[0], authors: batch})
   │       → for each kind-0 event: ParseMetadata → storeProfile(profilesDB)
   │       → on success, remove pubkey from queue; on failure, leave queued
   └─ StartRefreshLoop(interval): re-fetch already-known authors so renamed /
        updated profiles stay fresh (re-enqueue known pubkeys, then drain)

Client REQ {kinds:[0], search:"e-teaching", limit:10}
   └─ reg.fetch → profilesDB.QueryEvents → NIP-50 full-text over
        name, display_name, about, nip05 → ranked kind-0 events (full eventRaw)
```

The drain loop is shared by initial backfill, on-write enqueues, and refresh —
one code path, three triggers.

## Profile document & schema

`profileDoc` embeds `structuredEnvelope` (so the full kind-0 event is
reconstructable from `eventRaw`) plus the searchable fields:

| Field | Type | Notes |
|-------|------|-------|
| (envelope) | — | `eventID`, `eventKind`(=0), `eventPubKey`, `eventCreatedAt`, `eventRaw` |
| `name` | string | from kind-0 `name` |
| `display_name` | string | kind-0 `display_name`/`displayName` |
| `about` | string | kind-0 `about` |
| `nip05` | string | kind-0 `nip05` (stored only; not validated in v1) |

`SearchFields = "name,display_name,about,nip05"`. The Typesense document id is
the pubkey (one doc per author; a newer kind-0 upserts over the older — newest
profile wins, matching kind-0 replaceable semantics). `eventPubKey` is a facet
so an exact-pubkey lookup is also possible.

## Candidate admission (v1) & queue

- **Admission heuristic v1:** "authored ≥1 stored content event." Candidates
  come only from `StoreEvent`/`ReplaceEvent` enqueues + startup backfill — no
  extra scan, and we never index the cheap/spam profiles of pubkeys that never
  contributed content. (NIP-05 validity and interaction-ranking heuristics are
  deferred to a later phase; the `nip05` field is already captured for them.)
- **Queue:** a BoltDB bucket (`profile_queue`) keyed by pubkey hex →
  fire-and-forget, deduplicated, and durable across restarts. A pubkey is
  removed once its profile is successfully fetched+stored; left in place on
  fetch failure so the next drain retries it.
- **Refresh set:** the distinct `eventPubKey`s already in `profilesDB` (or,
  equivalently, re-enqueue from the backfill scan) — re-fetched on the refresh
  interval to catch renames.

## Configuration (new env)

| Var | Default | Purpose |
|-----|---------|---------|
| `PROFILES_ENABLED` | `false` | Master gate (manager + kind-0 registration). |
| `TS_COLLECTION_PROFILES` | `profiles_0` | Typesense collection name. |
| `PROFILE_RELAYS` | `wss://relay.edufeed.org` | Comma-separated source relays for kind-0 fetch. `relay.edufeed.org` is a general relay holding kind-0; the AMB relays do not. |
| `PROFILE_REFRESH_INTERVAL` | `6h` | Refresh-loop period (Go duration). |

## Error handling & edge cases

- **PROFILE_RELAYS unreachable / fetch timeout** → log, leave the candidate
  queued for the next drain/refresh. The content write is never blocked
  (`Enqueue` is fire-and-forget, like the existing write buffers).
- **`ParseMetadata` error** (no kind-0 found for a pubkey, malformed content) →
  skip that pubkey, leave it queued; never write a poisoned/empty doc.
- **`PROFILES_ENABLED=false`** → manager not started, kind-0 not registered,
  no queue bucket touched. Exactly the pre-feature behavior.
- **Typesense blip on `storeProfile`** → logged, pubkey stays queued (mirrors
  `storeStructured`'s fire-and-forget-but-log semantics; the candidate is not
  lost because the queue is durable).
- **Empty-`Kinds` queries** (Negentropy, chunk-rerank parent fetch) include the
  kind-0 type in the fan-out. Profiles are valid signed kind-0 events, so
  syncing/returning them is harmless; they are not chunked, so chunk-rerank
  never mis-handles them.
- **Reindex:** the kind-0 collection is **not** part of the BoltDB→Typesense
  reindex (profiles are not stored in BoltDB as relay events; they live only in
  `profilesDB`, sourced from remote relays). A future "rebuild profiles" admin
  op can re-run backfill; out of scope for v1.

## Testing (TDD)

Unit (Go, table-driven where natural):

- `nostrToProfile`: kind-0 JSON → doc fields; `display_name` vs `displayName`;
  missing optional fields; non-kind-0 input returns error.
- `profileSchema`: required searchable + envelope fields present.
- Registry: a client kind-0 write is rejected ("kind not accepted"); a
  `{kinds:[0]}` read routes to `profilesDB`; a kind-0 search is not treated as
  chunked.
- `ProfileManager` with a fake pool/queue: `Enqueue` dedups; backfill
  enumerates distinct authors from a BoltDB fixture; a fetch failure leaves the
  pubkey queued; a successful fetch removes it and stores the doc; refresh
  re-fetches a known author and a renamed profile upserts (newest wins).

Integration (optional, behind the dev stack):

- Write a content event from author A, then a `{kinds:[0], search:"<A's
  name>"}` REQ returns A's profile once the manager has drained.

## Out of scope (future phases)

- NIP-05 validity as an admission/ranking heuristic (field already captured).
- Interaction-based ranking (reactions/zaps/mentions).
- Admin "rebuild profiles" NIP-86 op.
- The MCP consumer that maps names→pubkeys and composes the calendar REQ.
