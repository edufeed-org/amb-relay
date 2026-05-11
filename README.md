# AMB Relay

A Nostr relay for AMB (Learning Resource Metadata) events (kind 30142). Built on the [khatru](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/khatru) relay framework with [Typesense](https://typesense.org/) as the full-text search backend.

The relay is paired with [`amb-indexer`](https://git.edufeed.org/edufeed/amb-indexer), which fetches
the resources referenced by each 30142 event, chunks + embeds them,
writes the fulltext back via NIP-86 `setcontent`, and exposes a
`/search_chunks` HTTP surface. For the full system architecture and
end-to-end testing guidance see **[`docs/architecture.md`](docs/architecture.md)**.

## Quick Start

```bash
# 1. Clone amb-indexer as a sibling (or skip — see Deployment for image mode)
git clone https://git.edufeed.org/edufeed/amb-indexer.git ../amb-indexer

# 2. Configure the relay
cp .env.example .env
$EDITOR .env                                          # set PUBKEY, ADMIN_PUBKEYS

# 3. Configure the indexer (the relay's compose stack pulls this in)
cp ../amb-indexer/.env.indexer.example ../amb-indexer/.env.indexer
$EDITOR ../amb-indexer/.env.indexer                   # set INDEXER_NSEC

# 4. Bring up the full stack
docker compose up -d --build
```

The relay listens on `:3334`, Typesense on `:8108`. See **[Deployment](#deployment)** for production guidance (TLS, persistent data, updates).

## Environment Variables

### Relay Metadata
- `NAME`: Your relay's display name
- `PUBKEY`: Your Nostr public key (hex)
- `DESCRIPTION`: A description of your relay
- `ICON`: URL to your relay's icon image

### Typesense Configuration

| Variable | Description | Local dev | Docker Compose |
|----------|-------------|-----------|----------------|
| `TS_APIKEY` | Typesense API key | `xyz` | `xyz` (change for production) |
| `TS_HOST` | Typesense URL | `http://localhost:8108` | leave empty (auto-set to `http://typesense:8108`) |
| `TS_COLLECTION` | Collection name | `amb_events` | `amb_events` |

### Networking

| Variable | Description | Default |
|----------|-------------|---------|
| `SERVICE_URL` | Public-facing WebSocket URL (e.g. `wss://your-relay.example.com`). Set when running behind a reverse proxy so NIP-98 auth `u` tag validation uses the correct URL instead of the auto-detected `ws://` | auto-detected |

### Storage & Management

| Variable | Description | Default |
|----------|-------------|---------|
| `DB_PATH` | Path to BoltDB file for raw event persistence | `./data/relay.db` |
| `ADMIN_PUBKEYS` | Comma-separated hex pubkeys for NIP-86 management API access (in addition to `PUBKEY`) | empty |

### Semantic Search (Optional)

| Variable | Description | Default |
|----------|-------------|---------|
| `EMBED_ENDPOINT` | URL of embedding service. Defaults to in-stack `http://embed:8100/embed` (see `.env.example`); override to point at an external embedder if you don't run the bundled `embed` service | empty (disabled) |
| `EMBED_TOKEN` | Bearer token for embedding service | empty |
| `SEMANTIC_SEARCH_ENABLED` | Auto-enable semantic search on startup | `false` |

When configured with `SEMANTIC_SEARCH_ENABLED=true`, the relay performs hybrid search (keyword + vector similarity) automatically. Can also be enabled/disabled at runtime via NIP-86.

**Model:** Uses MiniLM-L12-v2 (384 dimensions) for embeddings. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#semantic-search-hybrid-search) for technical details.

## Deployment

The full stack is **five services**: amb-relay, Typesense, embed (in-stack sentence-transformers), Tika (PDF extraction), and amb-indexer. Compose orchestrates all of them from this repo.

### Prerequisites

- Docker Engine 20+ with Compose v2
- On KVM VMs (Proxmox, libvirt, etc.) the `embed` service requires a CPU
  profile that exposes the **x86-64-v2** baseline (SSE3/SSSE3/SSE4.1/
  SSE4.2/POPCNT). Default profiles like `kvm64` and `qemu64` only expose
  `sse/sse2/cx16` and break NumPy's import. Use `cpu: host` or
  `cpu: x86-64-v2` (or newer) for the VM running the stack. LXC
  containers and bare-metal hosts are unaffected — they see the host's
  real CPU flags.
- A Nostr keypair for the relay operator (`PUBKEY` in `.env`)
- A Nostr keypair for the indexer (`INDEXER_NSEC` in `.env.indexer`) — its pubkey must be added to `ADMIN_PUBKEYS` so the relay accepts the indexer's `setcontent` calls

### Configuration

Two env files. Both are loaded by `docker-compose.yml`:

| File | Purpose | Template |
|------|---------|----------|
| `./.env` | Relay metadata, Typesense API key, `ADMIN_PUBKEYS`, semantic search toggles | `.env.example` |
| `../amb-indexer/.env.indexer` | Indexer keypair, embed endpoint, fetch limits, license allowlist | `../amb-indexer/.env.indexer.example` |

If you don't create `../amb-indexer/.env.indexer`, the `amb-indexer` service fails to start.

### Two compose modes

- **Source-tree (default)**: `docker-compose.yml` builds amb-indexer from `../amb-indexer`. Clone both repos as siblings under one directory.
- **Published image**: replace `build: ../amb-indexer` with `image: git.edufeed.org/edufeed/amb-indexer:main` (or a pinned `vX.Y.Z` / short-sha tag) and you only need amb-relay cloned. The relay itself also publishes to `git.edufeed.org/edufeed/amb-relay`.

Both repos publish images on push to `main` and on `v*` tags via Forgejo Actions.

### Persistent data

Data lives in **four** locations — back up all of them:

| Location | Service | What |
|----------|---------|------|
| `relay_data` (named volume) | amb-relay | BoltDB at `/root/data/relay.db` — raw events, ban lists, refetch queue. **Source of truth.** Typesense can be rebuilt from it. |
| `./typesense-data/` (bind mount) | Typesense | Search index. Rebuildable from BoltDB via NIP-86 `reindex`. |
| `indexer_data` (named volume) | amb-indexer | `/data/indexer.db` (cursor, event-hash, dead-letter). Loseable — indexer will replay from the relay on next start. |
| `embed_model_cache` (named volume) | embed | HuggingFace model cache (~120 MB). Loseable — re-downloads on first boot. |

The compose file ships a one-shot `amb-indexer-initdata` service that chowns `indexer_data` to nonroot uid 65532 (the indexer's distroless user). It runs once before `amb-indexer` starts.

### Reverse proxy & TLS

Containers talk over the internal docker network without TLS — you only need a reverse proxy to expose the relay's WebSocket to the public internet (and to fix NIP-98 auth, which validates the request URL against `SERVICE_URL`).

The recommended fronting setup is **Traefik** with docker labels. Add these to the `amb-relay` service in `docker-compose.yml` (or in a `docker-compose.override.yml` you keep alongside):

```yaml
amb-relay:
  labels:
    - "traefik.enable=true"
    - "traefik.http.routers.amb-relay.rule=Host(`relay.example.org`)"
    - "traefik.http.routers.amb-relay.entrypoints=websecure"
    - "traefik.http.routers.amb-relay.tls.certresolver=letsencrypt"
    - "traefik.http.services.amb-relay.loadbalancer.server.port=3334"
  networks: [default, traefik_proxy]   # whatever network Traefik watches
```

And set `SERVICE_URL=wss://relay.example.org` in `.env` so NIP-98's `u`-tag validation accepts requests at the public URL.

If you want to expose the indexer's `/search_chunks` HTTP API too, add the same label set on the `amb-indexer` service pointing at port `8080` (and a different `Host()`).

Any TLS-terminating reverse proxy works — Caddy, nginx, Cloudflare Tunnel — Traefik is just the path of least resistance with this compose file.

### Updating

```bash
# 1. Pull new images / rebuild
docker compose pull
docker compose up -d --build

# 2. If a relay release changed the Typesense schema, run the
#    NIP-86 schema migration recipe (see "Schema migrations after a
#    release" further down in this README).
```

### Verifying

For end-to-end smoke tests of the full stack (publish a 30142 event, watch the indexer pick it up, confirm fulltext lands and chunks are searchable), see **[`docs/architecture.md`](docs/architecture.md)**.

## Development

### Setup

After cloning, enable the pre-push hook that runs E2E tests before every push:

```bash
git config core.hooksPath .githooks
```

To skip the hook when needed: `git push --no-verify`

### Simple: everything in Docker

```bash
docker compose up
```

### With local eventstore changes

The eventstore (`typesense30142`) lives in the [nostrlib](https://git.edufeed.org/edufeed/nostrlib) fork. To develop both simultaneously:

1. Clone both repos side-by-side:
   ```
   edufeed/
   ├── go.work          # workspace file
   ├── amb-relay/
   └── nostrlib/
   ```

2. Create `edufeed/go.work`:
   ```go
   go 1.25

   use (
       ./amb-relay
       ./nostrlib
   )
   ```

3. Run Typesense in Docker, relay locally:
   ```bash
   docker compose up -d typesense
   go run .
   ```

The `go.work` file tells Go to use local nostrlib instead of the version from git.edufeed.org. Changes to the eventstore are immediately available on restart.

### Updating the nostrlib dependency

After pushing changes to nostrlib on git.edufeed.org:

```bash
# Get the new pseudo-version
GOWORK=off go list -m git.edufeed.org/edufeed/nostrlib@latest

# Update go.mod replace directive with the new version
# Then run:
GOWORK=off go mod tidy
```

## Testing

### nak CLI

```bash
# Full-text search
nak req --search "mathematik" -k 30142 ws://localhost:3334

# Field-specific search
nak req --search "publisher.name:e-teaching.org" -k 30142 ws://localhost:3334

# Time range filter
nak req --since 1700000000 --until 1800000000 -k 30142 ws://localhost:3334

# Filter by tagged pubkey
nak req -p <pubkey> -k 30142 ws://localhost:3334

# Delete an event (NIP-09) — by addressable event reference
nak event -k 5 -t "a=30142:<author-pubkey>:<d-tag>" --sec <your-secret> ws://localhost:3334

# Count events (NIP-45)
nak count -k 30142 ws://localhost:3334

# Count with filters
nak count -k 30142 --search "physics" ws://localhost:3334

# Semantic search (if enabled)
# Finds "quantum mechanics" even when searching for related terms
nak req --search "Heisenberg uncertainty principle" -k 30142 ws://localhost:3334

# Extension namespace (NIP-AMB ext:) — search by label or filter by URI
nak req --search "ext.ekw.bistum.prefLabel.de:Hannover" -k 30142 ws://localhost:3334
```

**Note:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`, `#ext:<ns>:<facet>:id`). For these filters, use a Go client with `nostr.TagMap`. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation, including the [`ext:` extension namespace](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#extension-namespace-ext) for non-AMB-core metadata fields.

### Direct Typesense debugging

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&per_page=1"
```

## Event Validation

The relay accepts kind 30142 events and kind 5 deletion events ([NIP-09](https://github.com/nostr-protocol/nips/blob/master/09.md)).

Kind 30142 events require:
- A `d` tag (resource identifier)
- A `name` tag (resource title)

Deletion events (kind 5) allow authors to delete their own events by referencing them with `a` tags (addressable) or `e` tags (by ID). Only the original author can delete an event.

Events from banned pubkeys are rejected.

## NIP-86 Management API

The relay supports [NIP-86](https://github.com/nostr-protocol/nips/blob/master/86.md) for remote management via HTTP. Send a POST request to the relay URL with `Content-Type: application/nostr+json+rpc` and a [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) `Authorization` header.

Only the relay operator (`PUBKEY`) and additional admins (`ADMIN_PUBKEYS`) are authorized.

### Supported methods

| Method | Description |
|--------|-------------|
| `supportedmethods` | List available methods |
| `banpubkey` | Ban a pubkey from publishing |
| `listbannedpubkeys` | List all banned pubkeys |
| `allowpubkey` | Remove a pubkey ban |
| `banevent` | Delete an event by ID and block resubmission. Removes from BoltDB, ContentStore, and Typesense, then records the id on the ban list so the same event cannot be re-published. |
| `listbannedevents` | List all banned event IDs |
| `allowevent` | Remove an event ban (does **not** restore the event — the data is gone; only clears the resubmission block) |
| `changerelayname` | Update relay name (in memory) |
| `changerelaydescription` | Update relay description |
| `changerelayicon` | Update relay icon URL |
| `stats` | Get relay statistics |

Ban lists are persisted in BoltDB and survive restarts.

### Typesense management methods

These custom methods control the Typesense search index schema, reindexing, and collection settings.

| Method | Params | Description |
|--------|--------|-------------|
| `getcollectionschema` | none | Returns the current Typesense collection schema (custom or default) |
| `updatecollectionschema` | `[{fields: [...], default_sorting_field: "...", enable_nested_fields: bool}]` | Stores a new schema in BoltDB. Does **not** apply until `reindex` is called |
| `resetcollectionschema` | none | Removes the custom schema, reverting to the hardcoded default |
| `reindex` | none | Drops the Typesense collection, recreates it with the stored schema, and re-indexes all events from BoltDB. Runs asynchronously |
| `getreindexstatus` | none | Returns `{running, total, indexed, errors, error}` |

Schema changes are deferred — `updatecollectionschema` only stores the schema, and `reindex` applies it. During reindex the relay cannot serve search results (drop + rebuild approach).

#### Schema migrations after a release

When a new relay release changes the hardcoded default schema (e.g. adding a new indexed field), running operators need to roll the live Typesense collection forward. BoltDB is the source of truth, so the recipe is "drop and rebuild from BoltDB":

```bash
# 1. Pull the new code and restart the relay container
docker compose pull && docker compose up -d --build amb-relay

# 2. Apply the new default schema (clears any stored custom schema)
nak event -k 24242 --tag method=resetcollectionschema --sec $ADMIN_NSEC --auth wss://<relay-host>

# 3. Drop and rebuild the Typesense collection from BoltDB
nak event -k 24242 --tag method=reindex --sec $ADMIN_NSEC --auth wss://<relay-host>

# 4. Poll until done
nak event -k 24242 --tag method=getreindexstatus --sec $ADMIN_NSEC --auth wss://<relay-host>
```

Skip step 2 if the running relay was previously configured with `updatecollectionschema` and you want to keep that custom schema. Step 3 also re-runs the fulltext replay pass, so `content` rows are preserved across the rebuild. Search is unavailable for the duration of step 3.

### Semantic search methods

| Method | Params | Description |
|--------|--------|-------------|
| `getsemanticsearchconfig` | none | Returns `{enabled, embed_fields}` |
| `updatesemanticsearchconfig` | `[{enabled: bool, embed_fields: [...]}]` | Update config and toggle embedding |
| `enablesemanticsearch` | none | Shortcut to enable with default fields |
| `disablesemanticsearch` | none | Shortcut to disable |
| `setcontent` | `[event_id, text, fetched_at, status, source_url]` | Persist fetched resource fulltext for an event. `status` values: `fetched` \| `truncated` \| `license_denied` \| `unsupported` \| `failed`. Admin only. |
| `refetchcontent` | `[event_id]` | Clear fulltext for an event so the indexer reprocesses it. Admin only. |
| `listrefetch` | none | List event IDs currently flagged for re-ingestion (set by `refetchcontent`). The indexer polls this. Admin only. |
| `acknowledgerefetch` | `[event_ids]` | Indexer calls this to remove ids from the refetch queue after re-enqueueing. Returns `{acknowledged: N}`. Admin only. |

**Default embed fields:** `name`, `description`, `keywords`, `about`

When enabled, new events are embedded on save and queries use hybrid search (30% vector, 70% keyword weight). Existing events need `reindex` to add embeddings.

### Resource Fulltext

The relay stores optional fulltext extracted from the resource referenced by each AMB event. Fulltext is provided by an external `amb-indexer` service (not part of this repository) via the `setcontent` NIP-86 method, persisted in the `fetched_content` BoltDB bucket, and projected onto the `content`, `content_fetched_at`, and `content_status` fields of the Typesense document.

When present, the `content` field participates in BM25 search alongside metadata fields. Reindex preserves fulltext: after the collection is rebuilt from BoltDB events, a replay pass PATCHes each stored content row back onto its Typesense document. The same pass drops rows whose events have been deleted (orphan GC).

The indexer service connects as a regular Nostr client + a NIP-86 admin. Add its pubkey to `ADMIN_PUBKEYS` to authorize `setcontent` and `refetchcontent`. See `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md` for the full design.

## Mirroring events from another relay

To stage events from a remote relay (e.g. prod) into this one, use the
`mirror-prod` tool that ships with `amb-indexer`. It paginates REQs and
republishes events verbatim — signatures carry over, so the destination
relay accepts the source events as-is. See
**[amb-indexer's *Mirror events from another relay* section](https://git.edufeed.org/edufeed/amb-indexer#user-content-mirror-events-from-another-relay)**
for flag reference, recipes, caveats (no re-signing, no content/chunks,
no kind-5), and alternatives (NIP-77 Negentropy, NIP-86 `reindex`).

## Architecture

The relay is one service in a five-service stack (relay, Typesense, embed, Tika, amb-indexer). For the full system diagram, the data flows, the refetch loop, and end-to-end verification, see **[`docs/architecture.md`](docs/architecture.md)**.

At a glance the relay itself does:

- **Khatru**: Nostr relay framework (part of the [nostrlib](https://git.edufeed.org/edufeed/nostrlib) fork)
- **Typesense**: Full-text search backend — queries go here; the relay projects events into the `amb_events` collection
- **BoltDB**: Embedded key-value store — raw event persistence + ban lists + refetch queue
- **NIP-86**: HTTP management API for ban lists, schema, semantic search, and the indexer-feedback methods (`setcontent` / `refetchcontent` / `listrefetch` / `acknowledgerefetch`)
