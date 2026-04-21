# AMB Resource Fulltext Indexing — Design

**Date:** 2026-04-21
**Status:** Draft for review
**Goals:** (1) improve search recall on the amb-relay corpus, (2) enable RAG/LLM use cases on AMB-indexed resources.

## Problem

AMB events (kind 30142) carry metadata *about* a learning resource (title, description, keywords, `about`, license, creator, etc.), but not the resource itself. The relay's existing search — hybrid BM25 + metadata embeddings via `paraphrase-multilingual-MiniLM-L12-v2` — only sees that metadata. For OER material the metadata is often sparse (a title and a one-line description), which limits both (1) keyword recall on specific terminology and (2) usefulness for LLM context retrieval.

The 30142 corpus distribution today is roughly 70% HTML URLs, 28% PDFs, ~2% nostr-native. Indexing only metadata leaves the *content* — where the terminology actually lives — unused.

## Summary of approach

Add a new service `amb-indexer` that:

1. Subscribes to amb-relay for kind 30142 events
2. Fetches the referenced resource (HTML / PDF via Tika / nostr kind 30023)
3. Extracts normalized text with structural metadata (headings, page numbers)
4. Chunks text using a structural+recursive strategy
5. Embeds chunks against the existing edufeed embedding service
6. Writes fulltext back to amb-relay via a new NIP-86 `setcontent` method (persisted in a new BoltDB bucket, projected to a new `content` field on the Typesense event doc)
7. Owns its own Typesense collection `amb_chunks_30142` containing per-chunk vectors + metadata
8. Exposes an HTTP `/search_chunks` endpoint and an MCP server for RAG consumers

**License gate:** vectors are always computed. Raw fulltext (on the relay) and raw chunk text (on the indexer) are persisted only when the AMB `license` field matches a configurable permissive allow-list.

**Freshness:** event-driven only. A replacement 30142 (same `d`-tag, newer `created_at`) triggers re-fetch. An operator-triggered NIP-86 `refetchcontent` method is the escape hatch. No scheduled revisit in Phase 1.

## Architecture

```
                      ┌───────────────────────┐
  Nostr clients ────► │     amb-relay         │◄──── nostr clients
  (publish 30142)     │  (khatru + BoltDB +   │      (search)
                      │   Typesense[events])  │
                      └───────┬───────┬───────┘
                              │       ▲ setcontent / refetchcontent
                    nostr REQ │       │         (NIP-86, over WS or HTTP)
                              ▼       │
                      ┌───────────────┴───────┐     ┌─────────┐
                      │     amb-indexer       │───► │  Tika   │
                      │  (fetch, extract,     │     │(PDF/DOC)│
                      │   chunk, embed)       │     └─────────┘
                      │  Typesense[chunks]    │
                      └──────────┬────────────┘
                                 │
  RAG clients (MCP/HTTP) ────────┘
```

**Components:**

- **amb-relay** — effectively unchanged. Two new NIP-86 methods (`setcontent`, `refetchcontent`), one new BoltDB bucket (`fetched_content`), three new fields on the Typesense schema (`content`, `content_fetched_at`, `content_status`).
- **amb-indexer** — new Go service. Nostr client for ingestion. Typesense client for chunk collection. HTTP server for RAG. MCP server wrapping the HTTP server.
- **Tika** — `apache/tika:latest-full` sidecar. Pure extractor, HTTP-reachable from the indexer.
- **Typesense** — same instance as the relay's in Phase 1, separate collection (`amb_chunks_30142`). Can be split to a dedicated cluster later if noisy-neighbor becomes observable.
- **Embedding service** — the existing edufeed `/embed` endpoint, reused as-is. Same 384-dim model for both the relay's metadata vectors and the indexer's chunk vectors.

**Auth:**

- The indexer has its own dedicated nostr keypair. Its pubkey is added to amb-relay's `ADMIN_PUBKEYS` env var to authorize NIP-86 calls.
- The indexer's `/search_chunks` surface uses bearer-token auth (`INDEXER_API_TOKEN`). No fine-grained ACLs in Phase 1; it's a read-only retrieval API.

## Ingest pipeline

Per AMB event received from the relay subscription:

```
receive 30142
    │
    ▼
compute content hash of (source_url || nostr ref)
    │
    ▼
already indexed with same hash? ──yes──► skip
    │ no
    ▼
license gate decision
    (persist_text = license matches allow-list)
    │
    ▼
fetch resource
    │
    ▼
┌───────────┬────────────┬──────────────┐
HTML        PDF          kind 30023     (unsupported)
readability Tika HTTP    nostr REQ      → dead-letter
    │           │             │
    └───────────┴─────────────┘
    │
    ▼
normalized text + structure
(text, headings[], pages[] for PDF, section_path)
    │
    ▼
hybrid structural+recursive chunker
(prefer structural boundaries; recursive split within over-long sections)
    │
    ▼
embed chunks (batched N=32 per embedder call)
    │
    ▼
┌──────────────────────┴──────────────────────┐
▼                                             ▼
setcontent(relay)                   upsert chunks collection
  text (if persist_text)              vectors always
  fetched_at                          text only if persist_text
  status                              snippet always (first ~200 chars
                                        of chunk text, fair-use fragment)
                                      metadata always
    │                                         │
    ▼                                         ▼
advance nostr cursor (persisted in BoltDB)
```

**Concurrency and sizing:**

- N worker goroutines (default `WORKERS=4`) pull from an in-memory queue fed by the nostr subscription.
- Cursor persistence on each successful upsert cycle. At-least-once delivery; reprocessing is idempotent (deterministic chunk IDs + content-hash skip).
- Fetch timeout 30s; Tika timeout 60s; embed timeout 30s.
- `FETCH_MAX_BYTES=10485760` (10 MB download cap); `EXTRACT_MAX_BYTES=2097152` (2 MB extracted text cap). Beyond extract cap: truncate and set `content_status=truncated`.
- Per-host politeness: max 2 concurrent requests, 500 ms minimum interval between requests per host. In-memory token bucket, reset on restart.
- SSRF guard: deny list of RFC1918, loopback, link-local, cloud metadata (`169.254.169.254`, `metadata.google.internal`, `metadata.azure.com`, etc.), and `.onion`. Configurable via `EGRESS_DENY_CIDRS` and `EGRESS_DENY_HOSTS`.

**Failure handling:**

- **Transient** (network error, DNS failure, 5xx, timeout): retry 3x with exponential backoff (1s, 4s, 16s). After exhaustion, dead-letter.
- **Permanent** (4xx, unsupported content-type, oversized after partial download, extraction failure, license-denied): straight to dead-letter.
- **Dead-letter** stored in a BoltDB bucket on the indexer (`dead_letter`) keyed by event id with `{error, first_seen, last_retry, retry_count, source_url}`. The indexer exposes `/admin/dead_letter` (list, inspect, replay) under the same bearer-token auth.
- Metrics: counters per failure mode to `/metrics`.

**Replacement semantics:**

- On receiving an event whose `(pubkey, d-tag)` address coordinate matches an already-indexed event with a *newer* `created_at`:
  1. Delete chunks from `amb_chunks_30142` where `event_coord` matches (Typesense filter delete).
  2. Re-run the pipeline for the new event.
  3. `setcontent` overwrites the BoltDB entry and TS `content` field.

## Chunking strategy

**Hybrid structural + recursive.**

1. **Structural pass** — prefer boundaries native to the source:
   - **HTML:** split on `<h2>`/`<h3>` sections after readability normalization. Section-path derived from nested heading hierarchy.
   - **PDF (via Tika):** use Tika's XHTML output, which emits page breaks and heading tags. Split on pages, then on headings within pages.
   - **kind 30023:** markdown; split on `#`/`##`/`###` headings.
2. **Recursive pass** — for any structural block exceeding the chunk target (~256 tokens ≈ ~1000 chars for Latin script), recursively split on paragraph breaks, then sentence boundaries, then finally fall back to a fixed-size cut.
3. **Target and overlap:**
   - Target size: ~256 tokens (chosen for MiniLM-L12-v2's effective context window).
   - Overlap: ~50 tokens between adjacent chunks produced by recursive splitting. Structural boundaries produce no overlap (the heading itself is context).
4. **Metadata carried per chunk:**
   - `heading` — the immediate enclosing heading (string).
   - `section_path` — array of ancestor headings (e.g., `["Chapter 3", "Photosynthesis", "Light reactions"]`).
   - `page` — PDF page number when available.
   - `source_url` — original resource URL.
   - `chunk_idx` — ordinal within the resource.

Chunk IDs are deterministic: `<event_id>:<chunk_idx>`, so re-processing is idempotent and prior versions are wholly replaced on update.

## Query paths

### Search (goal 1)

No client-facing change. The relay's existing hybrid search picks up the new `content` field automatically via Typesense's `query_by` weights. Proposed weights (applied via the existing `updatecollectionschema` NIP-86 method):

```
query_by=name,description,keywords,content
query_by_weights=5,3,2,1
```

Metadata hits still outrank body hits on equal term overlap; body hits lift documents that have no metadata match at all. Metadata embeddings continue to contribute via the existing `alpha=0.3` hybrid ranking on the event doc.

### RAG (goal 2)

Two entry points on the indexer, both backed by the same query engine:

**HTTP:** `POST /search_chunks`

Request:
```json
{
  "q": "photosynthesis light reactions",
  "k": 10,
  "filter": {
    "about_id": ["https://w3id.org/kim/schulfaecher/s1017"],
    "learning_resource_type": ["https://w3id.org/kim/hcrt/course"],
    "license_permissive": true
  }
}
```

Response:
```json
{
  "hits": [
    {
      "chunk_id": "<event_id>:7",
      "event_id": "...",
      "event_coord": "30142:<pubkey>:<d>",
      "chunk_idx": 7,
      "text": "...",                // omitted if license non-permissive
      "snippet": "...200 chars...", // always present (fair-use fragment)
      "heading": "Light reactions",
      "section_path": ["Photosynthesis", "Light reactions"],
      "page": 47,
      "source_url": "https://...",
      "score": 0.874,
      "amb": {
        "name": "...",
        "description": "...",
        "creator": [...],
        "license": "...",
        "about": [...]
      }
    }
  ],
  "total": 37
}
```

**MCP:** an MCP server wrapping the same engine, exposing a tool `search_educational_chunks(query, k, filters)` with equivalent schema. Claude / Cursor / other LLM tooling registers the indexer as a context source in one config line.

**Pipeline inside the indexer:**

1. Embed query via `/embed`.
2. Typesense hybrid multi_search on `amb_chunks_30142` (BM25 on `text` + cosine on `embedding`, `alpha=0.3` matching the relay's existing default).
3. For each hit, look up the parent event's AMB metadata (via nostr REQ or a small cache populated from the ingest path) and enrich the response.
4. Apply `license_permissive` filter post-scoring when `text` is requested (non-permissive hits have `text` omitted and only `snippet` returned).

## Schema

### amb-relay — Typesense (additions to existing collection)

```json
{"name": "content",            "type": "string", "optional": true},
{"name": "content_fetched_at", "type": "int64",  "optional": true},
{"name": "content_status",     "type": "string", "optional": true}
```

`content_status` values: `fetched` | `truncated` | `license_denied` | `unsupported` | `failed`.

### amb-relay — BoltDB (new bucket)

```
fetched_content
  key:   event_id (bytes)
  value: JSON { text, fetched_at, status, source_url, content_hash }
```

Read by `reindex` alongside the event bucket so rebuilds preserve fulltext. Written by the new `setcontent` NIP-86 handler. Cleared for a given event when a replacement 30142 arrives (handled in the existing replacement code path with a small hook).

### amb-relay — NIP-86 methods (new)

- `setcontent([event_id, text, fetched_at, status, source_url])` — admin-only. Upserts the BoltDB row and the TS doc fields. Returns `{ok: true}` or `{error}`.
- `refetchcontent([event_id])` — admin-only. Clears `fetched_content` row and marks the event for re-indexing (publishes a nostr event hint the indexer listens for, OR the indexer polls a `needs_refetch` flag — see open questions). Returns `{ok: true}`.

### amb-indexer — Typesense (new collection `amb_chunks_30142`)

```json
{
  "name": "amb_chunks_30142",
  "fields": [
    {"name": "id",                       "type": "string"},
    {"name": "event_id",                 "type": "string", "facet": true},
    {"name": "event_coord",              "type": "string", "facet": true},
    {"name": "pubkey",                   "type": "string", "facet": true},
    {"name": "chunk_idx",                "type": "int32"},
    {"name": "text",                     "type": "string",   "optional": true},
    {"name": "snippet",                  "type": "string"},
    {"name": "embedding",                "type": "float[]", "num_dim": 384, "vec_dist_metric": "cosine"},
    {"name": "heading",                  "type": "string",   "optional": true},
    {"name": "section_path",             "type": "string[]", "optional": true},
    {"name": "page",                     "type": "int32",    "optional": true},
    {"name": "source_url",               "type": "string"},
    {"name": "content_type",             "type": "string",   "facet": true},
    {"name": "license",                  "type": "string",   "facet": true, "optional": true},
    {"name": "license_permissive",       "type": "bool",     "facet": true},
    {"name": "about_id",                 "type": "string[]", "facet": true, "optional": true},
    {"name": "learning_resource_type",   "type": "string[]", "facet": true, "optional": true},
    {"name": "created_at",               "type": "int64"}
  ],
  "default_sorting_field": "created_at"
}
```

`query_by=text,heading,section_path` for BM25; `query_by_weights=4,2,1`. Embedding field used for vector similarity.

### amb-indexer — BoltDB (new)

```
cursor        (key: "nostr_cursor", value: int64 since-timestamp)
dead_letter   (key: event_id, value: JSON { error, first_seen, last_retry, retry_count, source_url })
event_hash    (key: event_id, value: content_hash)  -- for no-op skip on re-delivery
```

## Configuration

### amb-relay (additions)

```
ADMIN_PUBKEYS=<existing>,<indexer_pubkey>      # authorize indexer for NIP-86
```

No other relay-side env vars are added. Behavior gated on whether an indexer is actually connected; relay is fully functional standalone.

### amb-indexer

```
# Upstream
RELAY_URL=wss://amb-relay.example.org
RELAY_NIP86_URL=https://amb-relay.example.org/
INDEXER_NSEC=nsec1...

# Typesense (chunk collection)
TS_HOST=http://typesense:8108
TS_APIKEY=xxx
TS_CHUNKS_COLLECTION=amb_chunks_30142

# Embedding
EMBED_ENDPOINT=https://embed.edufeed.org/embed
EMBED_TOKEN=xxx

# Tika
TIKA_URL=http://tika:9998

# Public API
INDEXER_API_TOKEN=xxx
INDEXER_LISTEN=:8080

# Workers and limits
WORKERS=4
FETCH_MAX_BYTES=10485760
EXTRACT_MAX_BYTES=2097152
FETCH_TIMEOUT_SEC=30
TIKA_TIMEOUT_SEC=60
EMBED_TIMEOUT_SEC=30

# License policy
LICENSE_ALLOWLIST=CC0,CC-BY,CC-BY-SA,publicdomain
# (case-insensitive substring match on AMB `license` URL or label;
#  operators can override with e.g. "ALL" to persist everything)

# SSRF / egress
EGRESS_DENY_CIDRS=127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,::1/128,fc00::/7,fe80::/10
EGRESS_DENY_HOSTS=metadata.google.internal,metadata.azure.com
EGRESS_DENY_TLDS=onion

# Politeness
HOST_MAX_CONCURRENCY=2
HOST_MIN_INTERVAL_MS=500
```

## Observability

- **Indexer `/metrics` (Prometheus):**
  - `indexer_events_received_total`
  - `indexer_events_processed_total{outcome=success|skipped|failed|license_denied|truncated}`
  - `indexer_fetch_total{content_type,outcome}`
  - `indexer_fetch_duration_seconds{content_type}` (histogram)
  - `indexer_extract_duration_seconds{content_type}` (histogram)
  - `indexer_embed_batch_duration_seconds` (histogram)
  - `indexer_chunks_indexed_total`
  - `indexer_dead_letter_depth` (gauge)
  - `indexer_nostr_cursor_lag_seconds` (gauge: now - cursor)
- **Indexer `/health`** — liveness (process up).
- **Indexer `/ready`** — readiness (nostr connected, Typesense reachable, embedder reachable).
- **Admin surface** (bearer-token auth on `INDEXER_API_TOKEN`):
  - `GET /admin/dead_letter` — list failed events.
  - `POST /admin/dead_letter/{event_id}/replay` — re-enqueue.
  - `POST /admin/refetch/{event_id}` — force re-fetch (parallel to relay's `refetchcontent` for operator convenience).

## Deployment

Both services plus Tika added to `docker-compose.yml`:

```yaml
services:
  typesense:    # existing
  amb-relay:    # existing, + ADMIN_PUBKEYS extended
  amb-indexer:  # new
    depends_on: [typesense, amb-relay, tika]
    env_file: .env.indexer
  tika:
    image: apache/tika:latest-full
    restart: unless-stopped
```

The indexer is stateless except for its BoltDB (`cursor`, `dead_letter`, `event_hash`) which lives on a named volume. Loss of that volume is recoverable: clear the cursor, re-subscribe from genesis, content-hash skip prevents redundant re-work.

## Testing

- **Unit:** chunker (structural + recursive), license matcher, SSRF guard, Tika client, readability wrapper.
- **Integration:** end-to-end ingest against a test relay + test Typesense instance + a mock upstream HTTP server (standard Go `httptest`), one real small PDF through a real Tika container.
- **Contract:** snapshot tests for `/search_chunks` response shape.
- **Regression:** extend existing relay `test_e2e.sh` with a new section that verifies fulltext reaches the relay's TS `content` field via a mocked setcontent caller, and that `reindex` preserves it.

## Phase delineation

**Phase 1 (this spec, fully deployable):**
- Service skeleton, nostr subscription with cursor
- Fetcher for HTML (readability), PDF (Tika), kind 30023
- License gate
- Structural + recursive chunker with metadata
- Embedding batching against existing embedder
- NIP-86 `setcontent` / `refetchcontent` on relay + BoltDB bucket + TS field
- Chunk collection schema + indexer writes
- `/search_chunks` HTTP + MCP server
- Admin surface (dead-letter, refetch)
- Prometheus metrics
- Docker compose wiring

**Explicitly out of scope (Phase 2+ or deferred):**
- Scheduled revisit / crawl-based freshness
- OCR for scanned PDFs (Tika without Tesseract covers born-digital)
- Office formats beyond Tika defaults
- Reranker model on chunk results
- Multi-tenant per-operator chunk collections
- Fine-grained per-consumer ACLs on `/search_chunks`
- Nostr-native chunk retrieval (chunks as addressable events)
- Cross-relay subscription for federated indexing

## Open questions

These are intentionally deferred to implementation planning and do not block spec approval:

- **`refetchcontent` delivery mechanism** — two candidates: (a) the relay publishes a transient ephemeral event the indexer subscribes to, or (b) the indexer polls a `needs_refetch` flag in BoltDB via a NIP-86 read method. (a) is more elegant; (b) is simpler. Decide during planning.
- **AMB metadata join in `/search_chunks` responses** — whether to cache a minimal projection of AMB fields in the chunk collection (denormalized, stale-on-update) or do a live nostr REQ / BoltDB read per query (fresh, higher latency). Likely the denormalized path with a TTL, but confirm during planning.
- **Content hash granularity** — hash of `(source_url, last_modified)` vs hash of fetched body. Body hashing catches silent content drift but means every refetch embeds; URL hashing is cheap but misses content-only changes. Start with URL hash + `fetched_at` comparison.

## Risks

- **Copyright/takedown.** Even under the license gate, operators may receive takedown requests. The admin surface should include `DELETE /admin/event/{id}` to purge both relay content and indexer chunks in one call; this is Phase 1 scope.
- **Embedding service cost.** A one-time backfill of 100k events × avg ~20 chunks = ~2M embed calls. Batched at 32/call that's ~63k HTTP requests to the embedder. Needs coordination with the embedding service operator for rate limits.
- **Tika stability.** Tika is a large JVM process; OOMs under pathological PDFs are a known class of failure. Container memory limit + per-request timeout contain the blast radius.
- **Noisy-neighbor in shared Typesense.** Chunk collection will be 1-2 orders of magnitude larger than event collection. If query latency on the event collection degrades, split to a dedicated cluster (schema is unchanged, data is re-generable).
