# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

AMB Relay is a Nostr relay specializing in AMB (Learning Resource Metadata) events (kind 30142). Built on khatru relay framework with Typesense as the full-text search backend.

## Specifications

* nostr naddr of specification: `naddr1qvzqqqrcvypzp0wzr7fmrcktw4sgemxh5zsq5auh08vnvlwf0x9anusn7pkft0zgqy28wumn8ghj7un9d3shjtnyv9kh2uewd9hsqzm9v36kvet9vskkzmtzvjvrtf`
* The relay implements the `ext:` namespace defined in NIP-AMB for non-core metadata fields. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#extension-namespace-ext) for query examples.

## Build & Run Commands

```bash
# Local development (starts both Typesense and relay)
docker compose up

# Local development with eventstore changes (run relay outside Docker)
docker compose up -d typesense
go run .

# Production deployment
docker compose up -d --build

# Run tests in eventstore
cd ../nostrlib/eventstore/typesense30142 && go test ./...
```

## Architecture

```
Nostr Client → Khatru Relay (:3334) ─┬→ Typesense (:8108)  [search index]
              NIP-86 HTTP API ────────┘  BoltDB             [raw events + bans]
                                  ▲
                                  │  NIP-86 setcontent (NIP-98 signed)
                                  │
                              amb-indexer ──► Typesense (amb_chunks_30142)
                                  │           [chunk embeddings]
                                  ▼
                                Tika   (PDF extraction)
                                Embed  (sentence-transformers; in-stack)
```

The relay itself does not fetch or embed resources — that is `amb-indexer`'s job (see [`../amb-indexer`](../amb-indexer)). The indexer subscribes to kind-30142 events, extracts text, chunks and embeds it, and calls the relay's NIP-86 `setcontent` to populate the `content` field on each event. Chunks go to a separate Typesense collection.

Embedding runs in-stack as the `embed` service (`./embed`) — a small FastAPI container loading `paraphrase-multilingual-MiniLM-L12-v2` (384-dim). Both the relay (semantic write-path) and the indexer (chunk pipeline) point `EMBED_ENDPOINT` at `http://embed:8100/embed`, so there's no external dependency on `embed.edufeed.org` for the docker stack. Pytest suite for the service lives in `embed/tests/`.

**Main Entry Point:** `main.go` - Sets up khatru relay with dual-write to Typesense (search) and BoltDB (raw persistence), NIP-42 auth, NIP-86 management API, and Negentropy protocol.

**Management:** `management.go` - BoltDB-backed store for ban lists (pubkeys, events) and Typesense schema configuration, sharing the same bbolt database as the event store.

**Reindexer:** `reindex.go` - Async reindex from BoltDB to Typesense with progress tracking, triggered via NIP-86 `reindex` method.

**Event Flow:**
- Banned pubkeys are rejected on submission (checked before validation)
- Accepts kind 30142 events (AMB educational metadata) and kind 5 deletion events (NIP-09)
- Kind 30142 events are saved to both BoltDB (raw) and Typesense (indexed); kind 5 events are stored in BoltDB only
- Deletion events remove the referenced event from both BoltDB and Typesense (author must match)
- Queries go through Typesense for full-text search capability
- Queries support NIP-01 filter fields, tag filters, and NIP-50 search — see [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation
- Tags using the `ext:<ns>:<facet>:<sub>` shape (NIP-AMB extension namespace) are folded into a separate `ext` object in Typesense and queryable via NIP-50 (`ext.<ns>.<facet>.id:val`) and tag filter (`#ext:<ns>:<facet>:id`).

**Long-form (NIP-23 kind-30023), gated behind `LONGFORM_ENABLED`:**
- When `LONGFORM_ENABLED=true`, the relay also accepts kind-30023 long-form events. Required tags: `d` + `title` (validated in OnEvent; rejected when missing).
- 30023 structured fields are projected (`nostrToLongform`, `longform.go`) into a SEPARATE Typesense collection (`longform_30023` / `TS_COLLECTION_LONGFORM`), keeping the AMB collection pristine. StoreEvent/ReplaceEvent route 30023 to the second backend (`tsDB2`); DeleteEvent targets both collections.
- The query path merges all registered collections by kind via the content-type registry (`content_registry.go`): a filter's `kinds` selects which backends to hit (`registry.selected`/`registry.fetch`), and the ID-only parent fetch from chunk re-ranking queries every registered type.
- amb-indexer chunks 30023 `event.content` DIRECTLY (no external fetch/Tika) and writes to the SAME shared chunk collection (`amb_chunks_30142`); chunk coords are kind-prefixed (`30023:<pubkey>:<d>`).
- NIP-50 search over `kinds:[30142,30023]` returns interleaved results ranked by chunk score; opted-in clients (adding `21142`) receive a snippet kind-21142 event after each result carrying the correct parent `k` tag (`parentKind` derives it from the coord prefix).

**Wiki (NIP-54 kind-30818), gated behind `WIKI_ENABLED`:**
- When `WIKI_ENABLED=true`, the relay also accepts kind-30818 wiki events. Required tag: `d` (validated by `validateWiki`); `title`/`summary` optional. Structurally a near-subset of long-form 30023.
- 30818 structured fields are projected (`nostrToWiki`, `wiki.go`) into a SEPARATE Typesense collection (`wiki_30818` / `TS_COLLECTION_WIKI`), registered as a third `contentType` in the registry with its own backend (`tsDB3`). Long-form and wiki share the synchronous import-upsert + envelope helpers in `structured.go` (`upsertStructuredDoc`, `structuredEnvelope`, `storeStructured`); the AMB collection stays special (write buffer, embedder, NIP-86 schema).
- amb-indexer chunks 30818 `event.content` via the SAME content-direct path as 30023 (no external fetch/Tika/setcontent) into the shared chunk collection; chunk coords are kind-prefixed (`30818:<pubkey>:<d>`). Wiki content is Djot, so its headings are not parsed as ATX — wiki chunks carry no heading locators.

**Typesense Schema Management:**
- Custom NIP-86 methods (`getcollectionschema`, `updatecollectionschema`, `resetcollectionschema`, `reindex`, `getreindexstatus`) via khatru's `Generic` handler
- Schema config persisted in BoltDB; on startup, custom schema (if stored) overrides the hardcoded default
- Reindex drops the Typesense collection and rebuilds from BoltDB events

## Ingest pipeline (amb-indexer)

`amb-indexer` runs as a separate service. It is wired into this repo's `docker-compose.yml` and shares the same Typesense instance. Its Typesense collection (`amb_chunks_30142` by default) is disjoint from the relay's event collection.

- Source: [`../amb-indexer`](../amb-indexer) — see its `CLAUDE.md` and `README.md` for the pipeline detail.
- Compose services added alongside the relay: `tika` (Apache Tika for PDF extraction), `embed` (sentence-transformers FastAPI service; built from `./embed`), and `amb-indexer` itself (writes to a named volume `indexer_data`). The embed model cache is persisted via the `embed_model_cache` named volume.
- Auth: the indexer's pubkey (derived from `INDEXER_NSEC`) must be in the relay's `ADMIN_PUBKEYS` so its NIP-86 `setcontent` calls are accepted. Set `ADMIN_PUBKEYS` in this repo's `.env`.
- Configuration for the indexer lives in `../amb-indexer/.env.indexer` (template: `.env.indexer.example`).

**Fulltext content methods (admin-only, NIP-86):**
- `setcontent [event_id, text, fetched_at, status, (source_url)]` — amb-indexer writes extracted fulltext back; updates ContentStore + Typesense `content`/`content_fetched_at`/`content_status` fields.
- `refetchcontent [event_id]` — clears stored content and flags the event in the `needs_refetch` BoltDB bucket so the indexer will re-ingest it.
- `listrefetch []` — returns `{event_ids: [...]}` of events currently flagged for re-ingestion. Survives indexer downtime.
- `acknowledgerefetch [event_ids]` — indexer calls this after re-enqueueing, removing each id from the bucket. Returns `{acknowledged: N}`.

The indexer polls `listrefetch` every `REFETCH_POLL_INTERVAL` seconds, re-fetches each event, and acks the ids it accepted.

## Key Dependencies

- **nostrlib** (`fiatjaf.com/nostr`): Fork of nostr libraries including khatru relay framework and eventstore — hosted at [git.edufeed.org/edufeed/nostrlib](https://git.edufeed.org/edufeed/nostrlib)
- **eventstore/typesense30142**: Typesense wrapper for kind 30142 events (part of nostrlib)
- **eventstore/boltdb**: BoltDB wrapper for raw event persistence (part of nostrlib)

## Local Development with eventstore

The eventstore (`typesense30142`) lives in the nostrlib fork at `../nostrlib/eventstore/typesense30142`.

**How dependencies resolve:**

- **`go.mod`** has `replace fiatjaf.com/nostr => git.edufeed.org/edufeed/nostrlib v0.0.0-...` — this is what Docker and server deployments use (downloads from Gitea).
- **Parent `go.work`** at `edufeed/go.work` overrides the replace and uses `./nostrlib` directly — this is what local `go run .` uses.

```
edufeed/
├── go.work          # workspace: ./amb-relay, ./communikey-relay, ./nostrlib
├── amb-relay/
│   └── go.mod       # replace → git.edufeed.org/edufeed/nostrlib (for Docker/server)
└── nostrlib/
    └── eventstore/
        └── typesense30142/
```

Local changes to the eventstore are immediately available on relay restart (`go run .`). No need to push or update versions during development.

### Updating the nostrlib dependency

After pushing nostrlib changes to git.edufeed.org, update go.mod so Docker builds pick up the new code:

```bash
# Get the new pseudo-version
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest

# Update the replace directive in go.mod with the new version, then:
GOWORK=off go mod tidy
```

To verify the standalone build works (without local nostrlib):

```bash
GOWORK=off go build .
```

## Environment Variables

Required in `.env` (copy from `.env.example`):
- `NAME`, `PUBKEY`, `DESCRIPTION`, `ICON`: Relay metadata
- `TS_APIKEY`: Typesense API key (default: `xyz` for local dev)
- `TS_HOST`: Typesense URL (`http://localhost:8108` for local dev)
- `TS_COLLECTION`: Collection name for events
- `DB_PATH`: BoltDB file path (default: `./data/relay.db`)
- `ADMIN_PUBKEYS`: Comma-separated hex pubkeys for NIP-86 management API access (in addition to `PUBKEY`)
- `LONGFORM_ENABLED`: Set to `true` to accept kind-30023 (NIP-23 long-form) events and enable the second Typesense collection (default `false`).
- `TS_COLLECTION_LONGFORM`: Collection name for long-form structured fields (default `longform_30023`).
- `WIKI_ENABLED`: Set to `true` to accept kind-30818 (NIP-54 wiki) events and enable a third Typesense collection (default `false`).
- `TS_COLLECTION_WIKI`: Collection name for wiki structured fields (default `wiki_30818`).

Optional chunk re-ranking (`chunk_rerank.go`): when `CHUNK_RERANK_ENABLED=true` and `INDEXER_API_TOKEN` is set, NIP-50 searches are re-ranked by the best matching fulltext passage from amb-indexer's `POST /search_chunks` (`INDEXER_BASE_URL`, default `http://amb-indexer:8080`). Falls back to plain Typesense search on any indexer error or empty chunk result, so recall never degrades. Negentropy syncs carry no search field and bypass it naturally. Searches that additionally opt in with `kinds:[30142,21142]` receive an
ephemeral kind-21142 snippet event after each result, carrying the best
matching passage (`content`), `e`/`a`/`k` references to the parent, a
`score` tag, and `page`/`heading`/`source_url` locators when known
(`chunk_snippet.go`). Snippets are relay-signed with `RELAY_SECKEY` (a
dedicated key; per-boot generated when unset), never stored, and never
emitted on fallback paths. Clients that only ask for kind 30142 see exactly
the pre-snippet behavior.

## Testing

### nak CLI

```bash
# Full-text search
nak req --search "mathematik" -k 30142 ws://localhost:3334

# Field-specific search
nak req --search "publisher.name:e-teaching.org" -k 30142 ws://localhost:3334

# Time range filter
nak req --since 1700000000 --until 1800000000 -k 30142 ws://localhost:3334

# Delete an event (NIP-09) — by addressable event reference
nak event -k 5 -t "a=30142:<author-pubkey>:<d-tag>" --sec <your-secret> ws://localhost:3334
```

**Limitation:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`). For these filters, use a Go client with `nostr.TagMap`:

```go
relay.Subscribe(ctx, nostr.Filters{{
    Kinds: []nostr.Kind{30142},
    Tags: nostr.TagMap{
        "about:id": {"https://w3id.org/kim/schulfaecher/s1017"},
    },
    Limit: 10,
}})
```

### Direct Typesense debugging

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&per_page=1"
```

For the full list of supported filters, see the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md).

## MCPs 

* use nostr mcp for resolving nostr addresses or other related nostr questions.
* use other mcps if appropriate

## Skills

* consult skills if needed

