# Plan 2b — amb-indexer Query + Admin Surface & Relay Refetch Signaling

## Context

Plan 2a delivered the full ingest pipeline: the indexer subscribes to kind-30142 events on amb-relay, fetches referenced resources (HTML/PDF/kind-30023), chunks, embeds, writes chunks to Typesense `amb_chunks_30142`, and writes fulltext back to the relay via NIP-86 `setcontent`. No public-facing query surface exists. The relay's `refetchcontent` method clears content but doesn't tell the indexer to re-fetch. The relay's search doesn't weight `content` yet, so indexed fulltext is invisible to queries.

Plan 2b closes these gaps. After 2b the system gives operators a RAG-capable HTTP query surface on the indexer, admin surfaces for dead-letter/refetch/takedown, Prometheus metrics, and correct refetch signaling end-to-end. **MCP server is deferred to Plan 2c.** Plan 2b's HTTP surface is the backbone that Plan 2c will wrap.

## Scope decisions (from brainstorming)

- **D10. Split.** Plan 2b = HTTP + admin + metrics + relay refetch signaling + relay `query_by` update. MCP → Plan 2c.
- **D11. Refetch delivery.** Relay's existing `refetchcontent` handler is extended to also write a `needs_refetch` BoltDB entry. A new `listrefetch` NIP-86 read method returns pending entries; the indexer calls a new `acknowledgerefetch` method (with list of event_ids) to clear entries after it's re-enqueued them. Poll interval default 30s. Durable across indexer downtime; no new event kind.
- **D12. AMB metadata join.** In-memory LRU cache in the indexer, keyed by `event_coord` (`30142:<pubkey>:<d>`), TTL 10 min, cap 10 000 entries. Query path: collect missing coords from hits, batch one nostr REQ to `RELAY_URL` for them, populate cache, enrich response. No chunk schema change.
- **D13. Auth.** Two bearer tokens: `INDEXER_API_TOKEN` (for `/search_chunks`), `INDEXER_ADMIN_TOKEN` (for `/admin/*`). `/metrics`, `/health`, `/ready` unauthenticated.
- **D14. Relay `query_by` rollout.** Edit the nostrlib default at `eventstore/typesense30142/query.go` (three sites) to include `content` with weights `5,3,2,1`. Existing deployments run one documented `updatecollectionschema` call via nak/curl.

## Branching

- Plan 2a indexer work is on `amb-indexer` `main`.
- Plan 1 relay work is on `amb-relay` `dev` (unmerged; diverges from `main` which has later compose wiring).
- **Plan 2b branching:** cut a new feature branch `feat/query-admin` from `amb-relay/dev` for relay-side work, and a feature branch from `amb-indexer/main` for indexer-side work. Merging `dev` → `main` in amb-relay and finishing the Plan 2b branches is a separate, operator-driven reconciliation step; it is not a task in this plan. Document the dependency in `amb-relay/dev`'s CLAUDE.md once the branch is built.

## Architecture

```
┌─────────────── amb-indexer (feat/query-admin) ─────────────────┐
│                                                                │
│  main.go:                                                      │
│    ingest pool (Plan 2a)                                       │
│    + httpServer goroutine (INDEXER_LISTEN, default :8080)      │
│    + refetchPoller goroutine (interval REFETCH_POLL_INTERVAL)  │
│    all three join sync.WaitGroup; graceful shutdown            │
│                                                                │
│  http_server.go    HTTP mux, middleware (auth, metrics, log)   │
│  search.go         QueryEngine (embed → TS multi_search → AMB) │
│  ambcache.go       LRU+TTL, batched REQ on miss                │
│  admin.go          /admin/dead_letter/*, /admin/refetch,       │
│                    /admin/event (takedown)                     │
│  metrics.go        prometheus registry, counters+histograms    │
│  refetch.go        poller: CallNIP86("listrefetch") → feed     │
│                    events channel → ack via                    │
│                    CallNIP86("acknowledgerefetch", ids)        │
│  relay_client.go   + CallNIP86 (public generic)                │
│                    + FetchEventsByCoord (batched REQ helper)   │
│                    + FetchEventByID                            │
│  tsclient.go       + Search (hybrid multi_search)              │
└────────────────────────────────────────────────────────────────┘
                        │           ▲
                        │           │ /search_chunks   Prometheus /metrics
                        ▼           │ /admin/*         /health /ready
               ┌────────────────────┴─────────────┐
               │       amb-relay (feat/query-admin on dev) │
               │                                  │
               │  main.go:  NIP-86 switch          │
               │    existing: setcontent, refetch  │
               │    + refetchcontent EXTENDED: also│
               │      mgmt.MarkNeedsRefetch(id)    │
               │    + listrefetch   NEW            │
               │    + acknowledgerefetch NEW       │
               │                                  │
               │  management.go:                   │
               │    + bucketNeedsRefetch           │
               │    + Mark/Remove/List/Clear       │
               │                                  │
               │  nostrlib/typesense30142/query.go:│
               │    query_by: add "content"        │
               │    query_by_weights: 5,3,2,1      │
               └──────────────────────────────────┘
```

## File inventory

### amb-indexer (new files)
| File | Purpose |
|---|---|
| `http_server.go` | `newHTTPServer(...)` returns `*http.Server` with mux + middleware stack |
| `http_server_test.go` | middleware tests (auth, metrics wrapping) |
| `search.go` | `QueryEngine.Search(ctx, SearchRequest) (SearchResponse, error)` |
| `search_test.go` | with `httptest` Typesense + AMB cache stubs |
| `ambcache.go` | `AMBCache.Lookup(ctx, coords []string) (map[string]AMBMetadata, error)` |
| `ambcache_test.go` | LRU + TTL + batched REQ on miss |
| `admin.go` | HTTP handlers: dead_letter list/replay, refetch, takedown |
| `admin_test.go` | auth + happy path per endpoint |
| `metrics.go` | Prometheus Registry + metric definitions + `MetricsHandler()` |
| `metrics_test.go` | counter/histogram increment sanity |
| `refetch.go` | `refetchPoller(ctx, ...)`: poll + enqueue + ack |
| `refetch_test.go` | against in-process khatru or httptest relay stub |

### amb-indexer (modified files)
| File | Change |
|---|---|
| `config.go` / `config_test.go` | add `IndexerAPIToken`, `IndexerAdminToken`, `IndexerListen`, `AMBCacheTTL`, `AMBCacheMax`, `RefetchPollInterval` |
| `relay_client.go` / `relay_client_test.go` | add `CallNIP86(ctx, method, params) ([]byte, error)` (generic), `FetchEventByID`, `FetchEventsByCoord`; existing `SetContent` refactored onto `CallNIP86` |
| `tsclient.go` / `tsclient_test.go` | add `Search(ctx, collection, query map[string]any) (SearchResult, error)` |
| `worker.go` | add `EventSource` hint field on `Process` path *only if needed* for metrics — preferred: caller stamps metrics |
| `main.go` | wire HTTP server + refetch poller; two new goroutines in `wg` |
| `.env.example`, `.env.indexer.example` | new env vars with sensible defaults, `_TOKEN` values blanked |
| `CLAUDE.md` | add sections on query surface + admin + metrics |
| `README.md` | brief API reference + curl examples |
| `e2e_test.sh` | new assertions: search_chunks returns hit after ingest; refetch cycles an event; admin replays dead-letter |

### amb-relay (modified files, feat/query-admin on dev)
| File | Change |
|---|---|
| `main.go` | extend `refetchcontent`: call `mgmt.MarkNeedsRefetch`; add `listrefetch` case; add `acknowledgerefetch` case |
| `management.go` | add `bucketNeedsRefetch`; register in `Init`; add `MarkNeedsRefetch`, `RemoveNeedsRefetch`, `ListNeedsRefetch` |
| `management_test.go` (if exists; else new) | CRUD round-trip |
| `test_e2e.sh` | extend for listrefetch/ack flow |
| `CLAUDE.md` | document the two new methods |

### nostrlib (modified files)
| File | Change |
|---|---|
| `eventstore/typesense30142/query.go` | three sites (lines 182, 228, 266): add `content` to `queryBy`; where weights exist alongside, update weights to match `name,description,keywords,content` → `5,3,2,1` (see note below) |

Note on nostrlib weights: the current default constructs `queryBy` as a single comma list with implicit equal weights; if `query_by_weights` is not currently emitted, add it at the same three sites. Implementation reads existing code first to determine the exact shape.

## Key types

```go
// search.go
type SearchRequest struct {
    Q      string                 `json:"q"`
    K      int                    `json:"k"`
    Filter map[string]any         `json:"filter,omitempty"` // about_id, learning_resource_type, license_permissive
}

type SearchHit struct {
    ChunkID     string         `json:"chunk_id"`   // "{event_id}:{chunk_idx}"
    EventID     string         `json:"event_id"`
    EventCoord  string         `json:"event_coord"`
    ChunkIdx    int            `json:"chunk_idx"`
    Text        string         `json:"text,omitempty"`   // omitted if non-permissive
    Snippet     string         `json:"snippet"`
    Heading     string         `json:"heading,omitempty"`
    SectionPath []string       `json:"section_path,omitempty"`
    Page        int            `json:"page,omitempty"`
    SourceURL   string         `json:"source_url"`
    Score       float64        `json:"score"`
    AMB         *AMBMetadata   `json:"amb,omitempty"`
}

type SearchResponse struct {
    Hits  []SearchHit `json:"hits"`
    Total int         `json:"total"`
}

// ambcache.go
type AMBMetadata struct {
    Name        string   `json:"name,omitempty"`
    Description string   `json:"description,omitempty"`
    Creator     []string `json:"creator,omitempty"`
    License     string   `json:"license,omitempty"`
    About       []string `json:"about,omitempty"`
    LRT         []string `json:"learning_resource_type,omitempty"`
}

type AMBCache struct {
    // LRU with TTL, protected by mutex; batched relay REQ on miss
}
func (c *AMBCache) Lookup(ctx context.Context, coords []string) (map[string]AMBMetadata, error)

// tsclient.go
type TSSearchResult struct {
    Found int               `json:"found"`
    Hits  []TSSearchHitRaw  `json:"hits"`
}
type TSSearchHitRaw struct {
    Document       map[string]any `json:"document"`
    TextMatch      int64          `json:"text_match"`
    VectorDistance float64        `json:"vector_distance,omitempty"`
    HybridScore    float64        `json:"hybrid_search_info,omitempty"` // shape per Typesense version
}
func (t *TSClient) Search(ctx context.Context, collection string, body map[string]any) (TSSearchResult, error)

// relay_client.go
func (r *RelayClient) CallNIP86(ctx context.Context, method string, params []any) (json.RawMessage, error)
func (r *RelayClient) FetchEventByID(ctx context.Context, eventID string) (*nostr.Event, error)
func (r *RelayClient) FetchEventsByCoord(ctx context.Context, coords []string) (map[string]*nostr.Event, error)
```

## Endpoint contracts

- `POST /search_chunks` — Bearer `INDEXER_API_TOKEN`.
  - Req: `{q, k (default 10, max 100), filter?}`
  - Resp: `{hits:[SearchHit], total}`
  - Behavior: embed q (Embedder.Embed with `[]string{q}`), build TS `multi_search` body combining `query_by=text,heading,section_path`, `query_by_weights=4,2,1`, `vector_query=embedding:(<embedding>, k:k)`, `alpha=0.3`, `filter_by` from request filter, apply license gate (strip `text` for non-permissive). Collect `event_coord`s from hits, call `AMBCache.Lookup`, populate `hit.AMB`.
- `GET /admin/dead_letter` — Bearer `INDEXER_ADMIN_TOKEN`. Resp: `{entries: {event_id: DeadLetterEntry}}`.
- `POST /admin/dead_letter/{event_id}/replay` — delete entry, fetch event by ID, push into events channel. Resp: `{ok:true}` / 404.
- `POST /admin/refetch/{event_id}` — delete `event_hash` store entry for id (forces re-ingest on next sighting), fetch event, push into events channel. Calls relay `refetchcontent` via `CallNIP86` first so relay state is consistent. Resp: `{ok:true}`.
- `DELETE /admin/event/{event_id}` — takedown. Delete chunks by `filter_by=event_id:=<id>` from Typesense, call relay `setcontent` with empty text + `status=redacted`, record in dead-letter with reason `redacted`. Resp: `{ok:true, chunks_deleted:N}`.
- `GET /metrics` — Prometheus text format; no auth.
- `GET /health` — liveness (200). `GET /ready` — 200 if nostr connected, Typesense reachable, embedder reachable; 503 otherwise.

## Metrics

- `indexer_events_received_total` (counter)
- `indexer_events_processed_total{outcome}` — success|skipped|failed|license_denied|truncated|dead_letter
- `indexer_fetch_total{content_type,outcome}`
- `indexer_fetch_duration_seconds{content_type}` (histogram)
- `indexer_extract_duration_seconds{content_type}`
- `indexer_embed_batch_duration_seconds`
- `indexer_chunks_indexed_total`
- `indexer_dead_letter_depth` (gauge; updated on each dead-letter write, and on admin list)
- `indexer_nostr_cursor_lag_seconds` (gauge; `now - cursor` sampled each worker iteration)
- `indexer_search_duration_seconds` (histogram)
- `indexer_ambcache_hits_total`, `indexer_ambcache_misses_total`, `indexer_ambcache_size` (gauge)
- `indexer_refetch_processed_total{outcome}` — enqueued|ack_failed

Instrument call sites by passing a thin `*Metrics` into Worker, Fetcher, QueryEngine, AMBCache, refetch poller. Exposition via `promhttp.HandlerFor` on the custom registry — avoids globals and keeps tests hermetic.

## Implementation steps

### Relay-side (amb-relay feat/query-admin branched from dev)

1. Add `bucketNeedsRefetch` constant to `management.go:14-23` var block; add to `Init` registration loop. Add `MarkNeedsRefetch(eventID string) error`, `RemoveNeedsRefetch(eventID string) error`, `ListNeedsRefetch() ([]string, error)`. CRUD tests in `management_test.go` (create if missing).
2. Extend existing `refetchcontent` case in `main.go:696`: after `contentStore.Delete` and `ClearContent` succeed, call `mgmt.MarkNeedsRefetch(eventIDHex)`. If mark fails, log but still return success — operator's intent (clear content) succeeded; the signal step being best-effort is acceptable.
3. Add `case "listrefetch":` to `main.go` NIP-86 switch. No params. Returns `{result: {event_ids: [...]}, null on empty}`. Implementation: `ids, _ := mgmt.ListNeedsRefetch()`; build response.
4. Add `case "acknowledgerefetch":` to switch. Params `[event_ids []string]`. Calls `mgmt.RemoveNeedsRefetch` for each. Returns `{result: {acknowledged: N}}`.
5. Update nostrlib `eventstore/typesense30142/query.go:182, 228, 266`: replace the `queryBy` string literal to include `content`; if `query_by_weights` is emitted anywhere, update to `5,3,2,1` with the new field order. Add a fresh commit to nostrlib and bump `amb-relay/go.mod` `replace` pseudo-version (`GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest`).
6. Extend `test_e2e.sh`: call `refetchcontent` on a test event, then `listrefetch` and assert it appears, then `acknowledgerefetch` and assert it's gone.
7. Update amb-relay `CLAUDE.md`: document the two new NIP-86 methods, note the refetch signaling contract.

### amb-indexer core

8. Config: extend `Config` struct and `LoadConfig` with `IndexerAPIToken` (`INDEXER_API_TOKEN`), `IndexerAdminToken` (`INDEXER_ADMIN_TOKEN`), `IndexerListen` (`INDEXER_LISTEN`, default `:8080`), `AMBCacheTTL` (seconds, default 600), `AMBCacheMax` (default 10000), `RefetchPollInterval` (seconds, default 30). Tokens not required in `Validate` when `IndexerListen=""` (allows a pure-ingest configuration). Tests.
9. `tsclient.go`: add `Search`. POST to `{Host}/collections/{coll}/documents/search` with JSON body (Typesense accepts GET with query params for simple searches and POST with body for multi_search-shaped bodies; we use POST for body). Parse into `TSSearchResult`. Test via `httptest` with canned JSON.
10. `relay_client.go`: refactor — extract `CallNIP86(ctx, method, params)` from existing `SetContent` so `SetContent` becomes a thin call on it. Add `FetchEventByID(ctx, id string) (*nostr.Event, error)` using `nostr.Pool.SubscribeMany` with `{IDs: [id]}` filter, single-shot, read first event or timeout. Add `FetchEventsByCoord(ctx, coords []string) (map[string]*nostr.Event, error)` that parses each coord as `kind:pubkey:d`, builds a `Filter` with `Kinds, Authors, #d` tag filters, subscribes, collects until EOSE or ctx timeout. Tests using in-process khatru (same fixture as `nostr_source_test.go`).
11. `ambcache.go`: implement `AMBCache` using a simple map + doubly-linked-list LRU (or pull in `hashicorp/golang-lru/v2` — prefer stdlib and bounded map for zero new deps). Entries stored as `{value AMBMetadata, expiresAt time.Time}`. `Lookup(coords)` splits into hits+misses, fires one `FetchEventsByCoord` for misses, extracts `name`, `description`, `creator` (`["creator", ...]` tags), `license`, `about` (`about:id` values), `learning_resource_type` (`learningResourceType:id` values) from each event. Tests: cache hit, cache miss, TTL expiry, bounded cap eviction.
12. `metrics.go`: custom `*prometheus.Registry` with counters/histograms declared. `MetricsHandler()` returns `promhttp.HandlerFor(reg, ...)`. Expose typed increment methods on a `*Metrics` struct so call sites don't touch raw Prometheus types. Plumb `*Metrics` into `Worker`, `Fetcher`, `QueryEngine`, `AMBCache`, refetch poller via constructor params. Tests: register, increment, scrape via `httptest`, assert output contains expected series.
13. `search.go`: `QueryEngine` struct with `Embedder *Embedder`, `TS *TSClient`, `Coll string`, `Cache *AMBCache`, `Metrics *Metrics`. `Search(ctx, req)`:
    - Validate `k` (default 10, cap 100).
    - Embed q.
    - Build TS body: `{q: req.Q, query_by: "text,heading,section_path", query_by_weights: "4,2,1", vector_query: "embedding:([e1,e2,...], k:"+k+")", alpha: 0.3, filter_by: buildFilter(req.Filter), per_page: k}`.
    - Call `TS.Search`.
    - Translate hits to `SearchHit`. Apply license gate: if `LicensePermissive=false` on hit doc, set `Text = ""`.
    - Collect `event_coord`s; call `Cache.Lookup`; attach `AMB` field.
    - Return response.
    - Tests: stub TS, stub cache; one test covers permissive + non-permissive mix; one covers empty filter; one asserts `filter_by` composition; one asserts AMB enrichment only for hits.
14. `http_server.go`: mux with routes; middleware `authBearer(token string)`, `instrument(*Metrics, routeLabel string)`, `recoverer`, `accessLog`. `newHTTPServer(cfg Config, qe *QueryEngine, admin *AdminHandler, metrics *Metrics, readyProbe func() bool) *http.Server`. Tests: 401 without token, 401 with wrong token, 200 with right token, 503 ready when probe returns false.
15. `admin.go`: `AdminHandler` with fields for `DeadLetter`, `EventHash`, `TS`, `Relay`, `Events chan<- nostr.Event`, `FetchEventByID`. Handlers:
    - `handleDeadLetterList` → 200 JSON.
    - `handleDeadLetterReplay` → delete entry, fetch event, push to `Events` channel with select+ctx or 500 if full, 200.
    - `handleRefetch` → call `Relay.CallNIP86("refetchcontent", [id])`, delete `EventHash` entry for id, fetch event, push to `Events`, 200.
    - `handleTakedown` → `TS.FilterDelete(coll, "event_id:=<id>")`, then `Relay.SetContent(id, "", now, "redacted", "")`, then `DeadLetter.Put(id, {error:"redacted",...})`, 200 with `chunks_deleted`.
    - Tests with stub TS + stub RelayClient + in-memory stores.
16. `refetch.go`: `refetchPoller(ctx, cfg, relay, fetcher, events chan<- nostr.Event, eventHash *EventHashStore, metrics *Metrics)`:
    - `ticker := time.NewTicker(cfg.RefetchPollInterval); defer ticker.Stop()`
    - On each tick: `raw, err := relay.CallNIP86(ctx, "listrefetch", nil)`. Parse `{event_ids: []string}`. For each id: `eventHash.Delete(id)` (function to add), `ev := relay.FetchEventByID(ctx, id)`, push to events channel (with ctx cancellation). Collect acknowledged ids. If any enqueued: `relay.CallNIP86(ctx, "acknowledgerefetch", [ids])`. Metrics increments around each step. Exit on ctx.Done.
    - Also add `EventHashStore.Delete(eventID string) error` in `store.go`.
    - Tests with fake relay round-tripper that returns canned `listrefetch` results.
17. `main.go`: after existing worker-pool setup and before `source.Run`:
    - Build `metrics := NewMetrics()`. Pass into Worker, Fetcher (if metrics plumbed), query engine, etc.
    - Build `ambCache := NewAMBCache(cfg.AMBCacheMax, time.Duration(cfg.AMBCacheTTL)*time.Second, relayClient, metrics)`.
    - Build `queryEngine := &QueryEngine{...}`, `adminHandler := &AdminHandler{...}`, `readyProbe := func() bool { return /* ts ping + relay pong + embedder reachable */ }`.
    - `srv := newHTTPServer(cfg, queryEngine, adminHandler, metrics, readyProbe)`.
    - Add `wg.Add(1); go func(){defer wg.Done(); if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed { log.Printf("http server: %v", err) }}()`.
    - Add `wg.Add(1); go func(){defer wg.Done(); refetchPoller(rootCtx, cfg, relayClient, fetcher, events, eventHashStore, metrics)}()`.
    - Extend signal handler: on signal, first call `srv.Shutdown(shutdownCtx)` with 10s timeout, then `cancel()`.
    - Make sure readiness probe is correct: indexer can be "up but not ready" if relay momentarily disconnected; ready probe must tolerate transient misses — a single failed check in the last 60s is still ready. Simplest correct path: cache last OK timestamp per dependency; ready=(all last OK within 60s).
    - Confirm existing pool drain sequence still correct: after `source.Run` returns (or sig handler), cancel → `srv.Shutdown` → `close(events)` → `wg.Wait()`.
18. Env file templates and docs:
    - `.env.example` / `.env.indexer.example`: add new vars with defaults, tokens blank (`INDEXER_API_TOKEN=`).
    - `CLAUDE.md`: add sections for `/search_chunks` contract, admin surface, metrics list, readiness probe semantics, refetch signaling protocol (relay-side methods + indexer poll cadence).
    - `README.md`: small curl example for search + admin.
19. E2E extensions in `e2e_test.sh`:
    - After ingesting the HTML fixture, `curl -H "Authorization: Bearer $INDEXER_API_TOKEN"` to POST `/search_chunks` with `q="xyzzyfrobnicate"` and assert the hit appears.
    - Run `nak` (or `curl`) to `refetchcontent` on one of the test events; wait one poll interval; assert the event is re-ingested (chunks upserted again, observable via TS search timestamp or a `refresh` counter).
    - `curl -H "Authorization: Bearer $INDEXER_ADMIN_TOKEN" DELETE /admin/event/{id}` on a test event; assert chunks gone from TS, content cleared on relay.
20. `docker-compose.yml` (amb-relay): expose `8080` from `amb-indexer` for the new HTTP surface (no port publish by default — keep it on the internal network for now and document that operators add `ports: ["127.0.0.1:8080:8080"]` via override file for local debugging). Add token env passthrough.

## Task decomposition (for executing-plans / subagent-driven-development)

Tasks are relay-first where possible, indexer-first otherwise. Each task is self-contained; tests + commit per task.

1. [relay] `management.go` bucket + CRUD + tests
2. [relay] extend `refetchcontent` to mark needs_refetch; test via e2e
3. [relay] `listrefetch` NIP-86 method + `acknowledgerefetch` + tests
4. [nostrlib] update `query_by` and weights at three sites + bump amb-relay go.mod
5. [relay] e2e + CLAUDE.md update
6. [indexer] config additions + tests
7. [indexer] `TSClient.Search` + tests
8. [indexer] `RelayClient.CallNIP86` refactor + `FetchEventByID` + `FetchEventsByCoord` + tests
9. [indexer] `EventHashStore.Delete` + test
10. [indexer] `AMBCache` + tests
11. [indexer] `Metrics` registry + plumbing into Worker/Fetcher + tests
12. [indexer] `QueryEngine` + tests
13. [indexer] `http_server.go` with middleware + tests
14. [indexer] `admin.go` handlers + tests
15. [indexer] `refetch.go` poller + tests
16. [indexer] `main.go` wiring: metrics, HTTP server, poller, graceful shutdown; ready probe with 60s grace
17. [indexer] env templates + CLAUDE.md + README + docker-compose.yml override notes
18. [indexer] `e2e_test.sh` extensions
19. [both] final review: run `GOWORK=off go test -race ./...` in amb-indexer; run relay tests; `gofmt -l` clean; `go vet` clean; run `./e2e_test.sh` with docker

## Critical files to modify

- `amb-relay/main.go` (NIP-86 dispatch; lines ~696 and surrounding)
- `amb-relay/management.go` (bucket list + new CRUD)
- `amb-relay/test_e2e.sh`
- `amb-relay/CLAUDE.md`
- `nostrlib/eventstore/typesense30142/query.go` (lines 182, 228, 266)
- `amb-relay/go.mod` (pseudo-version bump for nostrlib replace)
- `amb-indexer/config.go`, `config_test.go`
- `amb-indexer/tsclient.go`, `tsclient_test.go`
- `amb-indexer/relay_client.go`, `relay_client_test.go`
- `amb-indexer/store.go` (add `EventHashStore.Delete`)
- `amb-indexer/main.go`
- `amb-indexer/.env.example`, `.env.indexer.example`
- `amb-indexer/CLAUDE.md`, `README.md`, `e2e_test.sh`
- `amb-relay/docker-compose.yml` (env passthrough only; no new port publish)

New files per the table above.

## Reuse map (existing → where used)

| Existing | Where reused in 2b |
|---|---|
| `ManagementStore` pattern (`amb-relay/management.go`) | new `needs_refetch` bucket follows the same shape |
| `ContentStore` (`amb-relay/content_store.go`) | untouched; but extended `refetchcontent` handler remains its main writer |
| `TSClient.FilterDelete` (`amb-indexer/tsclient.go`) | takedown endpoint uses `filter_by=event_id:=<id>` |
| `Embedder.Embed` (`amb-indexer/embed.go`) | `QueryEngine` embeds with `[]string{q}`; returns `[0]` |
| `Worker.Process` (`amb-indexer/worker.go`) | `admin.handleRefetch` + `refetchPoller` both inject events into the existing channel so Process runs unchanged |
| `nostr_source_test.go newTestRelay` fixture | reused for `relay_client_test.go`, `refetch_test.go` |
| `RelayClient` NIP-98 signing | promoted to generic `CallNIP86` for `listrefetch`/`acknowledgerefetch`/`refetchcontent` (admin-driven) |
| `nostr.Pool.SubscribeMany` | used for `FetchEventByID` / `FetchEventsByCoord` |

## Verification

- **Unit tests:** `GOWORK=off go test -race ./...` in `amb-indexer` (clean). Run `go test ./...` in `amb-relay` (clean) and in `nostrlib/eventstore/typesense30142` (clean).
- **Static checks:** `GOWORK=off go vet ./...`, `gofmt -l .` empty, in both repos.
- **E2E:** `./e2e_test.sh` in `amb-indexer` passes with new assertions (search hit after ingest, refetch round-trip, admin takedown).
- **Manual curl probes:**
  ```bash
  curl -sS -H "Authorization: Bearer $INDEXER_API_TOKEN" \
       -H "Content-Type: application/json" \
       -d '{"q":"photosynthesis","k":5}' \
       http://localhost:8080/search_chunks | jq .
  curl -sS http://localhost:8080/metrics | head -40
  curl -sS http://localhost:8080/ready  # 200 when all deps healthy
  ```
- **Relay manual probe** (nak):
  ```bash
  # set_content already tested; check new methods
  nak nip86 -c <admin> ws://localhost:3334 refetchcontent <event_id>
  nak nip86 -c <admin> ws://localhost:3334 listrefetch
  nak nip86 -c <admin> ws://localhost:3334 acknowledgerefetch '["<id>"]'
  ```
- **Observe end-to-end refetch:** call `refetchcontent` on an event; within `REFETCH_POLL_INTERVAL` the indexer's `indexer_refetch_processed_total{outcome="enqueued"}` increments and a fresh setcontent arrives back at the relay.

## Out of scope

- MCP server — Plan 2c.
- Reranker on chunk results — deferred to Phase 2.
- Per-consumer ACLs on `/search_chunks` — deferred.
- Merging `amb-relay` `dev` → `main` — operator step, not part of this plan.
- Full-corpus backfill run on production — operator step; plan only builds the mechanism.

## Risks and mitigations

- **Query-time AMB REQ latency.** Cold cache adds ~1 relay round-trip per unique coord in a query. Mitigation: warm cache on startup by subscribing to recent events, batched REQ on miss, 10-min TTL absorbs repeated queries. If still too slow in practice, fall back to denormalized fields — isolated change.
- **Refetch thundering herd.** Operator calling `refetchcontent` on many events at once dumps them all into the indexer's channel at the next poll. Mitigation: channel bounded at `workers*4` from Plan 2a; poller backpressures (blocks on send with ctx) and acknowledges only ids actually enqueued.
- **Auth token leakage via metrics.** Prometheus `/metrics` is unauthenticated by design; tokens must never appear in metric labels. Mitigation: only low-cardinality labels (content_type, outcome, route pattern); no raw paths or ids.
- **Tika/embedder unavailable on readiness probe.** Tight probe will flap. Mitigation: ready = "succeeded at least once within last 60s" per dependency, not "currently succeeds now".
- **Nostrlib pseudo-version bump races.** If two plans bump nostrlib concurrently they'll conflict. Mitigation: land the `query_by` change in nostrlib as its own commit, update amb-relay in its own commit, no other nostrlib changes riding along.
