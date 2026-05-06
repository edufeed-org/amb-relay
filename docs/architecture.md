# AMB Search — architecture & operator guide

This document explains how the AMB search stack fits together. It covers
the five cooperating services, the ingest pipeline, the public query
surface, the refetch signaling loop, and how to verify each part end-to-end.

If you just want to know how to run things, start at the top. If you want
to understand a specific subsystem, jump to the section.

## Services at a glance

```
                            ┌───────────────┐
        nostr clients       │   amb-relay   │   NIP-86 admin
        (publish 30142) ────▶  khatru + BoltDB│◀──── (operator)
                            │  ws :3334     │
                            └───────┬───────┘
                                    │ stores raw events in BoltDB
                                    │ writes event docs to Typesense
                                    ▼
                          ┌────────────────────┐
                          │    Typesense       │
                          │  amb_events        │ (from relay)
                          │  amb_chunks_30142  │ (from indexer)
                          │  :8108             │
                          └────────────────────┘
                                    ▲                    ▲
                                    │                    │
                                    │ SubscribeMany      │ chunk upserts
                            ┌───────┴────────┐           │
                            │  amb-indexer   │───────────┘
                            │  workers :8080 │
                            └───┬────────┬───┘
                                │        │
                      HTTP GET  │        │ POST /extract
                                ▼        ▼
                    ┌──────────────┐  ┌──────────────┐
                    │ external     │  │ Apache Tika  │
                    │ resources    │  │  :9998       │
                    │ (HTML, PDF)  │  └──────────────┘
                    └──────────────┘

                             (external)
                    ┌─────────────────────────┐
                    │ embedder HTTP service   │
                    │ (e.g. embed.edufeed.org)│
                    └─────────────────────────┘
                              ▲
                              │ POST {"texts":[...]}
                              │
                        amb-indexer (during ingest AND during /search_chunks)
```

### Who does what

| Service | Job | State |
|---|---|---|
| **amb-relay** | Accepts 30142 AMB events + kind-5 deletes. Stores raw events in BoltDB and projects them into the Typesense `amb_events` collection. Exposes NIP-86 admin (setcontent, refetchcontent, listrefetch, acknowledgerefetch). | `./data/relay.db` (BoltDB) |
| **Typesense** | Two collections: `amb_events` (from relay, for nostr filter + search) and `amb_chunks_30142` (from indexer, for RAG-style chunk search with hybrid BM25+vector). | `typesense-data/` |
| **amb-indexer** | For each 30142 event: fetches the referenced URL, extracts text, chunks it, embeds each chunk, upserts chunks to Typesense, writes the fulltext back to the relay's `content` field via NIP-86 `setcontent`. Also serves `/search_chunks` + `/admin/*`. | `indexer_data` volume (BoltDB: cursor, event-hash, dead-letter) |
| **Tika** | Converts PDFs → XHTML so the chunker can see structural metadata (headings, pages). Used by indexer only for `application/pdf` fetches. | stateless |
| **Embedder** | External HTTP service that turns text into 384-dim vectors. Called by indexer during ingest (for chunks) and during `/search_chunks` (for the query). | external |

Both `amb-relay` and `amb-indexer` live on the same docker network (in the
relay repo's `docker-compose.yml`). Only the relay's ws port is published
by default; the indexer's HTTP surface stays on the internal network
unless you add a compose override.

## Plan 2b: what was added

Before Plan 2b the indexer was a pure ingest pipeline with no public
surface. The relay could `refetchcontent` but had no way to tell the
indexer to re-ingest. Plan 2b added:

- **`POST /search_chunks`** — hybrid BM25+vector search over chunks, with
  AMB metadata enrichment on each hit.
- **`/admin/*`** — dead-letter list/replay (including bulk), force-refetch,
  takedown.
- **`/metrics`, `/health`, `/ready`** — Prometheus + probes.
- **Refetch signaling** — `refetchcontent` on the relay now writes a
  `needs_refetch` BoltDB flag; the indexer's poller calls `listrefetch`
  every 30s, re-ingests each id, then `acknowledgerefetch`s them.
- **Nostrlib `query_by` update** — the relay's event search now weights
  `content` so fulltext written by the indexer is actually findable.

## The ingest pipeline (end-to-end, one event)

```
 client publishes 30142 event to amb-relay
          │
          ▼
 relay validates → writes to BoltDB
                 → upserts to amb_events (Typesense)
          │
          ▼
 indexer nostr source picks up event (SubscribeMany, cursor-based resume)
          │
          ▼
 worker extracts source URL per NIP-AMB:
   url → r (first http/https) → d (if URL-shaped) → mainEntityOfPage:id → e → a
          │  (none found → dead_letter "no source URL found")
          ▼
 idempotency check (event_hash == sha256(source_url) → skip)
          │
          ▼
 license gate (default allowlist: CC0, CC-BY, CC-BY-SA, publicdomain)
   non-permissive → status=license_denied, no chunks, still writes
                    setcontent with empty text
          │
          ▼
 FilterDelete by event_coord (replace older chunks for re-published events)
          │
          ▼
 fetch (exp backoff 1s, 4s, 16s; SSRF guard; per-host politeness semaphore)
   HTML → go-readability
   PDF  → Tika sidecar → XHTML with page markers
   nostr: (kind 30023) → relay REQ for the `a` coord
          │
          ▼
 chunk (structural pass keeps headings + section_path + page)
          │
          ▼
 embed (batched POST to embedder with {"texts": [...]})
          │
          ▼
 upsert chunks to amb_chunks_30142 (one doc per chunk, nested embedding field)
          │
          ▼
 setcontent on relay (NIP-86, NIP-98-signed): writes fulltext back to
   relay's Typesense content field + content_status + content_fetched_at
          │
          ▼
 advance cursor + event_hash (idempotency). Cursor advances on terminal
 outcomes (success AND permanent failure) so bad events don't loop forever.
```

### Cursor and Since semantics

The indexer persists a BoltDB cursor = last processed `CreatedAt`. On
startup it subscribes with `Since=cursor`. The relay's slicestore uses
**strict-greater-than** for `Since`, so this correctly resumes *after*
the cursor; do **not** add `+1`.

### Dead-letter: what ends up there

Terminal failures (no URL, fetch exhausted, embed exhausted, unsupported
content type, missing d-tag, takedown). Dead-letter entries are persisted
in the indexer's BoltDB and surfaced via `GET /admin/dead_letter`.
`POST /admin/dead_letter/{id}/replay` and `replay_all` both re-enqueue
and clear the entry on successful enqueue.

## The query path (`/search_chunks`)

```
 client POST {q, k, filter?}
          │ Bearer INDEXER_API_TOKEN
          ▼
 QueryEngine.Search:
   1. Embedder.Embed([q]) → 384-dim vector
   2. Typesense hybrid search on amb_chunks_30142:
        query_by         = text,heading,section_path
        query_by_weights = 4,2,1
        vector_query     = embedding:([...], k:<k>)
        alpha            = 0.3    # 30% BM25, 70% vector (tunable)
        filter_by        = <built from req.filter>
        per_page         = k
   3. Translate hits → SearchHit; license gate elides `text` on
      non-permissive rows (snippet still returned).
   4. Normalize score to [0, 1]:
        hybrid_search_info.rank_fusion_score  (preferred)
        → 1 - cosine_distance/2                (vector-only fallback)
        → text_match / max(text_match in batch) (text-only fallback)
   5. Collect event_coords, batch one nostr REQ for the missing ones
      (AMBCache: LRU + 10-min TTL, cap 10k), attach AMB metadata.
   6. Return {hits, total}.
```

### Example request

```bash
curl -sS \
  -H "Authorization: Bearer $INDEXER_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"q":"photosynthesis chloroplast","k":2}' \
  http://localhost:8080/search_chunks | jq .
```

### Example response (trimmed)

```json
{
  "total": 22,
  "hits": [
    {
      "chunk_id":   "054d419f...:96",
      "event_id":   "054d419f...",
      "event_coord":"30142:4686...:plan2b-test-html-1776955403",
      "chunk_idx":  96,
      "text":       "…Photosynthesis by isolated chloroplasts. II. Photophosphorylation…",
      "snippet":    "…Photosynthesis by isolated chloroplasts. II. Photophosphorylation…",
      "heading":    "References",
      "section_path":["Photosynthesis","References"],
      "source_url": "https://en.wikipedia.org/wiki/Photosynthesis",
      "score":      1.0,
      "amb": {
        "name": "Photosynthesis (Wikipedia)",
        "description": "Test resource for Plan 2b smoke",
        "license": "CC-BY-SA",
        "learning_resource_type": ["https://w3id.org/kim/hcrt/worksheet"]
      }
    }
  ]
}
```

### Filtering

The request body's `filter` object is translated into a Typesense
`filter_by` expression. Supported keys: `about_id`, `learning_resource_type`,
`license`, `license_permissive`.

```json
{
  "q": "climate",
  "k": 10,
  "filter": {
    "learning_resource_type": ["https://w3id.org/kim/hcrt/worksheet"],
    "license_permissive": true
  }
}
```

### License gate

If a chunk's `license_permissive` is false, the response strips `text`
but keeps `snippet` so callers can still render a preview. This lets the
same index serve discovery for non-permissive resources without making
the full text retrievable.

## The admin surface

| Endpoint | Method | What it does |
|---|---|---|
| `/admin/dead_letter` | GET | List all entries. |
| `/admin/dead_letter/{id}/replay` | POST | Fetch the event, push it into the ingest channel, clear the entry on success. |
| `/admin/dead_letter/replay_all` | POST | Bulk replay. Body optional: `{"reason_substr":"no source URL","limit":100}`. Replay entries whose `Error` contains the substring; cap with `limit`. Response: `{total, replayed, skipped, failed, errors}`. |
| `/admin/refetch/{id}` | POST | Calls relay `refetchcontent` (clears content + arms `needs_refetch`), deletes local `event_hash`, re-enqueues. |
| `/admin/event/{id}` | DELETE | Takedown: drops chunks from Typesense (`event_id:=<id>`), sets relay content to empty with `status=redacted`, records in dead-letter. |

All admin endpoints require `Authorization: Bearer $INDEXER_ADMIN_TOKEN`.

### Bulk replay — the common case

When a pipeline bug strands a known cohort in dead-letter (e.g. 250 events
that were wrongly flagged "no source URL found" before the parser fix),
clear them with one call:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $INDEXER_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason_substr":"no source URL"}' \
  http://localhost:8080/admin/dead_letter/replay_all
```

Response:

```json
{"total":250,"replayed":250,"skipped":1,"failed":0}
```

The filter matters. Without it `replay_all` would also replay *redacted*
entries from takedowns, which you almost certainly don't want.

## The refetch signaling loop

When an operator wants the indexer to re-ingest a specific event (for
example after a source-side correction), they call the relay, not the
indexer directly:

```
operator ──▶ relay NIP-86 refetchcontent [event_id]
                  │
                  ▼
           relay clears content + marks needs_refetch in BoltDB
                  │
                  ▼
           (≤ REFETCH_POLL_INTERVAL_SEC later)
                  │
           indexer poller: listrefetch → [ids]
                  │
                  ▼
           for each id: delete event_hash, FetchEventByID, push to ingest
                  │
                  ▼
           acknowledgerefetch [ids_enqueued]  (only ids it actually took)
                  │
                  ▼
           relay removes acknowledged ids from needs_refetch
```

The protocol is durable across indexer downtime: entries that weren't
acknowledged stay in the bucket and get retried on the next tick. Nothing
is dropped silently.

### Probing the two new NIP-86 methods

```bash
# Show what's currently flagged
nak nip86 -c <admin-nsec> ws://localhost:3334 listrefetch

# Clear a single id manually (normally the indexer does this)
nak nip86 -c <admin-nsec> ws://localhost:3334 acknowledgerefetch '["<event_id>"]'
```

## Score normalization

Typesense's raw `text_match` is a packed int64 (values in the 1e18 range)
that's only comparable *within one response*. For the public API we emit
a normalized score in `[0, 1]`:

1. If the response is a hybrid search and Typesense returned
   `hybrid_search_info.rank_fusion_score`, use it (already 0..1).
2. Else if only vector search ran, use `1 - cosine_distance/2`.
3. Else fall back to `text_match / max(text_match in batch)`.
4. Clamp to `[0, 1]`.

So a top hit on a direct-match query lands at `1.0`, and scores decay
smoothly. See `amb-indexer/search.go` (`normalizeScores`).

## Observability

`GET /metrics` (unauthenticated, Prometheus text). Key series:

- `indexer_events_received_total` — everything the ingest channel sees.
- `indexer_events_processed_total{outcome=…}` — success, skipped, failed,
  license_denied, truncated, dead_letter.
- `indexer_fetch_total{content_type, outcome}`,
  `indexer_fetch_duration_seconds{content_type}` — per-content-type
  fetch stats.
- `indexer_embed_batch_duration_seconds`,
  `indexer_search_duration_seconds` — latency of the two embedder hot paths.
- `indexer_chunks_indexed_total` — cumulative chunk upserts.
- `indexer_dead_letter_depth` — current depth of the dead-letter store.
- `indexer_nostr_cursor_lag_seconds` — `now - cursor`, sampled per
  worker iteration. A rising number means the indexer is falling behind.
- `indexer_ambcache_hits_total`, `_misses_total`, `_size` —
  AMB-metadata cache behaviour.
- `indexer_refetch_processed_total{outcome=…}` — outcome of each poller
  tick's per-id processing: `enqueued`, `list_failed`, `fetch_failed`,
  `ack_failed`.

### Readiness semantics

`GET /ready` returns 200 iff Typesense and the relay have been marked
healthy within the last 60s. The TS prober runs every 15s; the relay
freshness is stamped each time the refetch poller's `listrefetch`
succeeds (default every 30s). Brief blips don't flap readiness.

`GET /health` is 200 always — use it for liveness, not dependency health.

## Configuration reference (indexer, Plan 2b-specific)

| Var | Default | Purpose |
|---|---|---|
| `INDEXER_LISTEN` | `:8080` | HTTP surface bind address. Empty = pure-ingest, no HTTP. |
| `INDEXER_API_TOKEN` | (required if listening) | Bearer for `/search_chunks`. Generate with `openssl rand -hex 32`. |
| `INDEXER_ADMIN_TOKEN` | (required if listening) | Bearer for `/admin/*`. |
| `REFETCH_POLL_INTERVAL_SEC` | `30` | How often the poller calls `listrefetch`. |
| `AMB_CACHE_TTL_SEC` | `600` | AMB metadata LRU TTL. |
| `AMB_CACHE_MAX` | `10000` | LRU capacity. |

Full list in `amb-indexer/.env.example`.

## How to test

### Unit + race

```bash
cd amb-indexer
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
gofmt -l .   # must be empty
```

### End-to-end script

`amb-indexer/e2e_test.sh` spins up the full stack, publishes four
kind-30142 fixtures (HTML, PDF, kind-30023 naddr, and a non-permissive
license case), and asserts:

- Relay content field populated with correct `content_status`.
- Relay search by a detectable term returns the right event.
- Indexer chunks have correct `event_coord` and `license_permissive`.
- `/search_chunks` without token → 401; with token → hit with AMB
  enrichment.
- `/admin/refetch/{id}` round-trips; content comes back.
- `/admin/event/{id}` takedown drops chunks, sets `status=redacted`.
- `/admin/dead_letter/replay_all` with `reason_substr` preserves the
  redacted entry.

Run it with docker available:

```bash
cd amb-indexer
./e2e_test.sh
```

It expects `EMBED_ENDPOINT` (and `EMBED_TOKEN` if the embedder requires
auth) to be set — either in the env or in `amb-relay/.env`.

### Manual smoke (one command per step)

```bash
# 1. Pipeline health
curl -sS -H "Authorization: Bearer $INDEXER_ADMIN_TOKEN" \
  http://localhost:8080/admin/dead_letter \
  | jq '.entries | to_entries | map(.value.error) | group_by(.) | map({reason: .[0], n: length})'

# 2. Search works and returns normalized scores
curl -sS -H "Authorization: Bearer $INDEXER_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"q":"photosynthesis","k":3}' \
  http://localhost:8080/search_chunks \
  | jq '.hits[] | {chunk_id, score, source_url, name: .amb.name}'

# 3. Metrics reachable
curl -sS http://localhost:8080/metrics | grep -E '^indexer_(events|chunks|dead_letter)' | head

# 4. Readiness
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' http://localhost:8080/ready
```

### Observing a refetch round-trip

```bash
# Capture a recent event id from the relay
EID=$(nak req -l 1 -k 30142 ws://localhost:3334 | jq -r .id)

# Trigger refetch (admin-signed)
nak nip86 -c <admin-nsec> ws://localhost:3334 refetchcontent "$EID"

# Within REFETCH_POLL_INTERVAL_SEC (default 30s):
#   - content_status flips back from "" to fetched/truncated
#   - indexer_refetch_processed_total{outcome="enqueued"} increments
watch -n2 '
  curl -sS http://localhost:8080/metrics \
    | grep indexer_refetch_processed_total'
```

## Troubleshooting

**Many "no source URL found" in dead_letter.** Expected for events that
legitimately have no URL. If the count is large and the events are real
AMB data, the URL parser may be missing a tag shape — check
`extractSourceURL` in `amb-indexer/worker.go` against the event's tags.
Then bulk-replay once fixed.

**`/ready` returns 503.** One of Typesense or the relay hasn't been
marked healthy in the last 60s. Check the indexer logs for the specific
probe error; the TS prober logs at every poll tick, the refetch poller
logs on failure.

**Scores look wrong.** They should be in `[0, 1]`. If you see `1.15e18`
you're on a build before commit `98aad26`; rebuild.

**Indexer can't reach the embedder.** The embedder contract is
`POST {"texts":[...]}` (not `inputs`). Pre-commit `ba87da1` builds send
the wrong key and get 422.

**Chunks missing for re-published events.** Indexer calls
`FilterDelete` by `event_coord` before each upsert to supersede old
chunks for a given `30142:pub:d`. If old chunks persist, check that the
indexer's `INDEXER_NSEC` pubkey is in the relay's `ADMIN_PUBKEYS` — a
failed `setcontent` doesn't roll back chunk writes but will log.

**Where are the logs.** `docker compose logs -f amb-indexer` and
`docker compose logs -f amb-relay`.

## Comparison notes (local hybrid vs prod name/description search)

On 2026-04-24, a snapshot of `wss://amb-relay.edufeed.org` (250 kind-30142
events, the relay's cap per REQ) was mirrored into local dev via
`amb-indexer/cmd/mirror-prod` and processed end-to-end. The comparison is
against the *mirrored* corpus on the local relay, not against live prod —
but since both use the same typesense30142 name/description search, the
right-hand side represents what prod would return for the same queries.

**Corpus breakdown after drain:**

| content_status   | count | share  |
|------------------|-------|--------|
| fetched          | 16    | 6.4 %  |
| license_denied   | 229   | 91.6 % |
| (pending/other)  | 5     | 2.0 %  |

The 91.6 % license-denied rate is *not* a regression in the license gate —
it is what the real prod corpus looks like. Most prod events carry no
`license` or `license:id` tag at all; only the handful tagged with a
Creative Commons URL (which the Task-1 normalizer now recognizes) pass the
permissive allowlist. Pre-fix, even those 16 events would have been
denied — the fix is working, there's just less body text to index than
the plan assumed there would be.

**Side-by-side query results (`k=10`):**

| Query          | Local `/search_chunks` | Prod name/desc REQ |
|----------------|------------------------|--------------------|
| Demokratie     | 14                     | 3                  |
| Pythagoras     | 10                     | 1                  |
| Bruchrechnung  | 10                     | 0                  |
| Photosynthese  | 10                     | 0                  |
| Hoffnung       | 18                     | 6                  |

Chunk-level hybrid search surfaces body-text matches that a name/description
search cannot reach — most visibly `Bruchrechnung` and `Photosynthese`,
which prod returns nothing for but the indexer returns the per-chunk cap
from bodies where the term appears. Spot-checked snippets (e.g. for
`Demokratie`: "…Mehrheitsentscheidungen sowie Ambiguitäts- und
Pluralitätstoleranz…") confirm the hits are substantive, not metadata
echoes.

**Relay-side fix landed alongside this comparison:** `patchDoc` in
`content_ts.go` was embedding the Typesense doc id (`{pubkey}:{d-tag}`)
unescaped into the PATCH URL. For AMB events whose d-tag is a resource
URL, the `/` characters split into path segments and Typesense returned
404 for every `setcontent` call. `url.PathEscape` around the docID
resolved it. The bug was latent until real prod events were exercised —
all prior testing used plain-string d-tags.

**How to reproduce:**

```bash
# On a fresh local stack (docker compose down -v + wipe typesense-data + up):
GOWORK=off go build -o /tmp/mirror-prod ./amb-indexer/cmd/mirror-prod
/tmp/mirror-prod --src wss://amb-relay.edufeed.org --dst ws://localhost:3334

# Wait for drain (indexer logs fall silent when idle); then:
open http://localhost:18080/ui
```

### Typesense as a projection of BoltDB+ContentStore

The relay treats BoltDB (raw events) and ContentStore (indexer-supplied
fulltext) as the source of truth. Typesense is a derived **projection**
maintained by a single writer goroutine inside `TSWriteBuffer` — it
batches incoming events and flushes them to the search index, and it
also handles fulltext patches from `setcontent`. Nothing else writes to
Typesense at runtime.

This pattern is borrowed from the `bleve` eventstore in nostrlib (see
`RawEventStore` on the `TSBackend`): queries that can be answered by the
raw store (e.g. address-replaceable lookups by `kind+author+#d`) bypass
Typesense entirely and read from BoltDB. Typesense only owns the
full-text/search-relevant projection; replaceability and dedupe live in
BoltDB.

**Why this matters during bulk replay:** before the projection refactor
the relay had two writers — the live event-storage path *and* the
indexer's NIP-86 `setcontent` PATCH — racing to upsert the same
Typesense doc. During a `mirror-prod` run that race manifested as
widespread `setcontent` 404s (the doc didn't exist yet when the indexer
patched it), and `content_status=fetched` stayed at 0 across the entire
8554-event corpus. With the projector serializing all writes,
`setcontent` is applied atop a doc that is guaranteed to already exist,
and `content_status` rolls forward as the indexer drains.

**Recovery still goes through reindex.** If Typesense is wiped or its
schema is reset, the NIP-86 `reindex` method drops the collection and
rebuilds it from BoltDB events plus the ContentStore — same projection,
applied in bulk. There is no "rebuild from Typesense" path because
Typesense is never the authority.

See `docs/superpowers/plans/2026-05-05-typesense-projection-pattern.md`
for the full plan and the verification run that proved out the pattern.

## See also

- `amb-indexer/README.md` — service-level quick reference.
- `amb-indexer/CLAUDE.md` — more detail on the ingest pipeline internals.
- `amb-relay/CLAUDE.md` — relay internals + fulltext content methods.
- [nostrlib eventstore/typesense30142 README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) — supported filter syntax.
