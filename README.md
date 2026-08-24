# nope-relay

A Nostr relay and search platform for structured metadata. Kind-30142 AMB
(Learning Resource Metadata) is always served; long-form, wiki, calendar,
transferkiosk, publications, community shares and kind-0 profile indexing are
opt-in per deployment — see **[Content types](#content-types)**. Built on the
[khatru](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/khatru)
relay framework with [Typesense](https://typesense.org/) as the search backend.

The relay is paired with [`amb-indexer`](https://git.edufeed.org/edufeed/amb-indexer), which fetches
the resources referenced by each event, chunks + embeds them,
writes the fulltext back via NIP-86 `setcontent`, and exposes a
`/search_chunks` HTTP surface. For the full system architecture and
end-to-end testing guidance see **[`docs/architecture.md`](docs/architecture.md)**.

> **Repository:** the canonical home is git-over-nostr at
> `nostr://laoc.xyz/nope-relay` ([gitworkshop.dev](https://gitworkshop.dev/laoc.xyz/nope-relay)).
> [git.edufeed.org](https://git.edufeed.org/edufeed/amb-relay) and
> [GitHub](https://github.com/edufeed-org/amb-relay) are mirrors. Docker images,
> the compose service and the module path still use the `amb-relay` name.

## Quick Start

```bash
# 1. Clone amb-indexer as a sibling (or skip — see Deployment for image mode)
git clone https://git.edufeed.org/edufeed/amb-indexer.git ../amb-indexer

# 2. Configure the relay
cp .env.example .env
$EDITOR .env                                          # set PUBKEY, ADMIN_PUBKEYS,
                                                      # and any content-type flags

# 3. Configure the indexer (the relay's compose stack pulls this in)
cp ../amb-indexer/.env.indexer.example ../amb-indexer/.env.indexer
$EDITOR ../amb-indexer/.env.indexer                   # set INDEXER_NSEC

# 4. Bring up the full stack
docker compose up -d --build
```

The relay listens on `:3334` (override with `PORT`), Typesense on `:8108`. See
**[Deployment](#deployment)** for production guidance (TLS, persistent data, updates).

## Content types

Each content type is a registration in the content-type registry: a set of kinds,
a validator, and its own Typesense collection. AMB is always present; everything
else is appended only when its flag is set.

| Kinds | What | Enable with | Collection (env / default) | Required tags | Chunk-indexed |
|---|---|---|---|---|---|
| `30142` | AMB learning resource (NIP-AMB) | *always on* | `TS_COLLECTION` *(no default)* | `d`, `name` | yes |
| `30023` | [Long-form article (NIP-23)](https://github.com/nostr-protocol/nips/blob/master/23.md) | `LONGFORM_ENABLED=true` | `TS_COLLECTION_LONGFORM` / `longform_30023` | `d`, `title` | yes |
| `30818` | [Wiki article (NIP-54)](https://github.com/nostr-protocol/nips/blob/master/54.md) | `WIKI_ENABLED=true` | `TS_COLLECTION_WIKI` / `wiki_30818` | `d` | yes |
| `30143`, `30144` | Transferkiosk Projekt / Maßnahme (NIP-DIDACTIC) | `TRANSFERKIOSK_ENABLED=true` | `TS_COLLECTION_TRANSFERKIOSK` / `transferkiosk` | `d`, `name` | yes |
| `30040`, `30041` | Publication index / section (NKBIP-01) | `PUBLICATIONS_ENABLED=true` | `TS_COLLECTION_PUBLICATIONS` / `publications` | `d`, `title`; a `30040` **must** have empty `content` | yes |
| `31922`, `31923` | [Calendar date / time event (NIP-52)](https://github.com/nostr-protocol/nips/blob/master/52.md) | `CALENDAR_ENABLED=true` | `TS_COLLECTION_CALENDAR` / `calendar_31922`, plus `CALENDAR_INDEX_PATH` | `d`, `title`, `start` | yes |
| `31924` | Calendar (NIP-52) | `CALENDAR_ENABLED=true` | ↑ same | `d`, `title` | yes |
| `31925` | Calendar RSVP (NIP-52) | `CALENDAR_ENABLED=true` | ↑ same | `d`, `a`, `status` ∈ `accepted`/`declined`/`tentative` | yes |
| `16`, `30222` | Community share — [NIP-18](https://github.com/nostr-protocol/nips/blob/master/18.md) repost / legacy targeted publication | `COMMUNITY_SHARES_ENABLED=true` | `TS_COLLECTION_SHARES` / `community_shares` | ≥1 community target + ≥1 `e`/`a`; `30222` also `d` | no |
| `0` | Author profile index | `PROFILES_ENABLED=true` | `TS_COLLECTION_PROFILES` / `profiles_0` | *read-only — client writes are rejected* | no |
| `5` | [Deletion request (NIP-09)](https://github.com/nostr-protocol/nips/blob/master/09.md) | *always on* | *BoltDB only* | `e`, or `a` naming a served kind | no |

Notes:

- Every flag defaults to `false` and must be **exactly** the string `true`.
- Enabling a type creates its Typesense collection at startup and adds its kinds
  to the NIP-11 `retention` document. Disabling it stops acceptance and removes
  the kinds from queries and NIP-11 — it does **not** drop the collection or the
  BoltDB events.
- **Chunk-indexed** means `amb-indexer` chunks and embeds the type into the shared
  chunk collection, so it participates in chunk re-ranking and kind-21142 snippets
  (see [Search & ranking](#search--ranking)). Types marked *no* are served straight
  from Typesense.
- The four calendar kinds share one flag and one collection; they are listed
  separately only because their required tags differ.
- Calendar events are **dual-indexed**: structured fields in Typesense, plus a
  BoltDB start/end/geohash index for range and location queries.
- Kind-0 profiles are populated only by the relay's internal fetcher, which pulls
  each content author's kind-0 from `PROFILE_RELAYS`. Client writes of kind 0 are
  rejected.

**Field projection, facet names and per-type query semantics are documented in
[`CLAUDE.md`](CLAUDE.md).** That file is developer-oriented but is the
authoritative reference for what is searchable inside each collection; this
README deliberately does not duplicate it.

### Event acceptance order

`OnEvent` applies these checks in order, before the owning content type's
validator runs:

1. Author pubkey is banned → `pubkey is banned`
2. Event id is banned → `event is banned`
3. Event was previously deleted by its author → `blocked: event was deleted by its author`
4. Writes are restricted and the author is neither an admin nor write-allowlisted
   → `restricted: pubkey not on write allowlist` (see [Access control](#access-control--authentication))
5. The registered validator for the event's kind

A kind with no registration is rejected with `kind not accepted`.

### Deletions

Kind-5 deletion requests let authors delete their own events by `a` (addressable)
or `e` (by id) reference. Only the original author may delete an event, and a
deletion request may not target another deletion request (per NIP-09,
"publishing a deletion request event against a deletion request has no effect").

Deleted ids are recorded so the exact event cannot be re-published. Deletion
requests are themselves stored and served — NIP-09 says relays SHOULD continue
to publish them — filterable by `kinds`, `authors`, `ids`, `#e`, `#a` and `#k`.

### Canonical queries

```bash
# Topic search across content types (interleaved, chunk-ranked)
nak req --search "klimawandel" -k 30142 -k 30023 -k 30818 --limit 5 ws://localhost:3334

# Calendar: events in a time window
nak req -k 31923 -t "start_after=$(date +%s)" -t "start_before=$(date -d '+7 days' +%s)" ws://localhost:3334

# Calendar: events near a location (geohash prefix; prefix length = radius)
nak req -k 31922 -k 31923 -t "g=u33d" ws://localhost:3334

# Transferkiosk: measures belonging to a project
nak req -k 30144 --search "partOf:30143:<pubkey>:<d-tag>" ws://localhost:3334

# Publications: by DOI
nak req -k 30040 --search 'doi:10.1234/abcd.5678' ws://localhost:3334

# Content shared with a community
nak req -k 30142 -k 31923 --search "community:<community-pubkey>" ws://localhost:3334
```

Two traps worth knowing before you file a bug:

- **Most structured facets are reachable only through `search`, not tag filters.**
  The shared query builder maps only a handful of tag names to Typesense fields
  (`t`→`keywords`, plus `r`/`p`/`a`/`h` and any `ns:facet` shape). A plain
  `#doi` / `#author` / `#partOf` / `#published_on` filter hits the builder's
  default case and is **silently skipped** — use NIP-50
  `search:"field:value"` instead.
- **Query community membership with `search:"community:X"`, never `#h:X`.** When a
  community member shares content, the relay denormalizes the community onto the
  content's own document; it cannot add an `h` tag to the event, since that would
  invalidate the author's signature. The relay resolves `#h:X` against the same
  field and does return stamped content, but clients that re-validate REQ results
  against the raw event's tags drop it. `search:"community:X"` carries no tag to
  re-validate and works on every client.

## Environment Variables

Every variable the relay reads is templated in `.env.example`. Anything left
unset falls back to the code default shown here.

### Relay metadata & networking

| Variable | Description | Default |
|----------|-------------|---------|
| `NAME` | Relay display name (NIP-11) | empty |
| `PUBKEY` | Relay operator's Nostr public key (hex). Always a NIP-86 admin | empty |
| `DESCRIPTION` | Relay description (NIP-11) | empty |
| `ICON` | URL to the relay's icon image (NIP-11) | empty |
| `SERVICE_URL` | Public-facing WebSocket URL (e.g. `wss://your-relay.example.com`). Set when running behind a reverse proxy so NIP-98 auth `u`-tag validation uses the correct URL instead of the auto-detected `ws://` | auto-detected |
| `PORT` | Listen port | `3334` |

### Storage & management

| Variable | Description | Default |
|----------|-------------|---------|
| `TS_APIKEY` | Typesense API key | `xyz` locally — change for production |
| `TS_HOST` | Typesense URL. `.env` points at `http://localhost:8108` for local `go run .`; compose overrides it to `http://typesense:8108` | none |
| `TS_COLLECTION` | Collection for AMB (30142) events. **Required — no built-in default**; an empty value yields a broken backend | none (`amb-local` in `.env.example`) |
| `DB_PATH` | BoltDB file for raw event persistence, ban lists and the refetch queue | `./data/relay.db` |
| `ADMIN_PUBKEYS` | Comma-separated hex pubkeys with NIP-86 management access, in addition to `PUBKEY` | empty |
| `HYDRATE_ON_START` | When `true`, run an in-process Typesense→BoltDB reconciliation pass before the listener opens. Scans the **AMB collection only**. See [Recovering from BoltDB ↔ Typesense skew](#recovering-from-boltdb--typesense-skew) | `false` |
| `QUERY_FETCH_TIMEOUT_MS` | Budget for a single REQ's search fetch, and identically a NIP-45 COUNT. See below | `20000` |

`QUERY_FETCH_TIMEOUT_MS` hardens against a degraded Typesense. A filter with no
`kinds` fans out across every registered content type **serially**, so one slow
(rather than fast-erroring) backend can otherwise stack up to
`N × per-call timeout` before the request ships. On expiry a REQ gets EOSE with
whatever arrived in time; a COUNT gets a NOTICE and a zero count, since a scalar
has nothing partial to return.

> `"0"` is an explicit, deliberate opt-out (no cap) and is **not** the same as
> leaving the variable unset. Unset, negative and non-numeric all fall back to
> the 20s default.

### Content types

The seven feature flags and their collection names are documented in
**[Content types](#content-types)**: `LONGFORM_ENABLED`, `WIKI_ENABLED`,
`CALENDAR_ENABLED`, `TRANSFERKIOSK_ENABLED`, `PUBLICATIONS_ENABLED`,
`PROFILES_ENABLED`, `COMMUNITY_SHARES_ENABLED`, each with a matching
`TS_COLLECTION_*`, plus `CALENDAR_INDEX_PATH` (default
`./data/calendar_index.db`) for the calendar range index.

### Profile & community fetchers

Both fetchers are internal: they pull events from remote relays to populate
relay-side indexes. Neither accepts client writes.

| Variable | Description | Default |
|----------|-------------|---------|
| `PROFILE_RELAYS` | Comma-separated relays the kind-0 fetcher pulls from | `wss://relay.edufeed.org` |
| `PROFILE_FALLBACK_RELAYS` | Queried only for authors whose kind-0 was not found on `PROFILE_RELAYS`, so a profile living on a non-standard relay is still indexed. Set empty to disable | `wss://purplepag.es,wss://relay.damus.io,wss://relay.nostr.band` |
| `PROFILE_REFRESH_INTERVAL` | How often to re-fetch known authors' kind-0 (Go duration) | `6h` |
| `COMMUNITY_RELAYS` | Relays the community registry fetches kind-10222 definitions and kind-30000 member lists from. Active only with `COMMUNITY_SHARES_ENABLED=true` | `wss://relay.edufeed.org` |
| `COMMUNITY_REFRESH_INTERVAL` | How often to re-resolve known communities (Go duration) | `6h` |

Each fetched profile's claimed `nip05` is verified at index time and stored as
`nip05_verified`. This is **ranking and filtering only, never inclusion**:
unverified profiles stay searchable, verified ones rank first at equal text
relevance, and clients can filter with `search:"<term> nip05_verified:true"`.
The flag is relay-side metadata, not part of the signed kind-0 — a client
wanting to display a checkmark should still verify client-side.

## Search & ranking

### Semantic search

| Variable | Description | Default |
|----------|-------------|---------|
| `EMBED_ENDPOINT` | URL of the embedding service. `.env.example` points at the in-stack `http://embed:8100/embed`; override for an external embedder | empty (disabled) |
| `EMBED_TOKEN` | Bearer token for the embedding service | empty |
| `SEMANTIC_SEARCH_ENABLED` | Auto-enable semantic search on startup | `false` |

With `SEMANTIC_SEARCH_ENABLED=true` the relay performs hybrid search (keyword +
vector similarity) automatically. It can also be toggled at runtime via NIP-86.

**Model:** `intfloat/multilingual-e5-base` (768 dimensions). See the
[eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#semantic-search-hybrid-search)
for technical details.

### Chunk re-ranking

| Variable | Description | Default |
|----------|-------------|---------|
| `CHUNK_RERANK_ENABLED` | Re-rank NIP-50 results by the best matching fulltext passage from amb-indexer's chunk index | `false` |
| `INDEXER_BASE_URL` | Base URL of the amb-indexer HTTP API | `http://amb-indexer:8080` |
| `INDEXER_API_TOKEN` | Bearer token for `POST /search_chunks` — must match `INDEXER_API_TOKEN` in `../amb-indexer/.env.indexer` | empty (disables the feature) |
| `CHUNK_RERANK_TIMEOUT_MS` | Per-request budget for the `/search_chunks` call. Only a positive integer is honoured | `5000` |

When enabled, a search query is answered by first asking amb-indexer's chunk
index for the best matching *passages* inside the indexed documents (PDFs, web
pages, event content), then returning the parent events ordered by best passage
score. Results reflect document content, not just metadata relevance.

Protocol-transparent: clients use the same NIP-50 REQ syntax and receive the same
event shapes — only the ordering changes. All other filter fields (kinds, authors,
tags, since/until, limit) still apply. On any indexer error or empty chunk result
the relay falls back to plain Typesense search, so recall never degrades.

Two paths deliberately bypass re-ranking: Negentropy syncs carry no search field,
and **field-filtered searches** (a `search` term of the form `field:value`) are
answered by Typesense directly, since chunk scores cannot honour a field
constraint. Neither path emits snippets.

> Keep `CHUNK_RERANK_TIMEOUT_MS` above the indexer's query-embed step (1-2s on a
> CPU embed service). Too tight a budget cancels legitimate warm searches
> mid-embed, surfacing as indexer 500s and silent fallback.

### Search snippets (kind 21142)

With chunk re-ranking active, clients can opt in to ephemeral snippet
events by adding kind `21142` to their search REQ:

```json
{"kinds": [30142, 21142], "search": "photosynthese", "limit": 5}
```

Each result is then followed by a relay-signed kind-21142 event (ephemeral
range — never stored) carrying the best matching passage:

```json
{
  "kind": 21142,
  "content": "…bei stärkerem Licht verdoppelt sich die Photosyntheserate…",
  "tags": [
    ["e", "<parent event id>"],
    ["a", "30142:<author pubkey>:<d-tag>"],
    ["k", "30142"],
    ["score", "0.9312"],
    ["page", "12"],
    ["heading", "Lichtabhängigkeit"],
    ["source_url", "https://…/skript.pdf"]
  ]
}
```

The `k` tag carries the parent's real kind, so a mixed-kind search returns
snippets correctly attributed across content types.

`score` is the indexer's normalized relevance for the best matching chunk,
always in `[0,1]`. In the common hybrid-search case it derives from
Typesense's rank fusion and is therefore **relative to the result set**:
useful for ordering and rough confidence within one response, but not
comparable across different queries or different relays. Clients merging
results from multiple relays should merge by per-relay rank instead and
deduplicate by the `a` tag.

`page`, `heading` and `source_url` appear only when the indexer extracted
them. `limit` counts parent events, so an opted-in client receives at most
`2×limit` events before EOSE. Snippets are best-effort decoration: on any
re-rank fallback (indexer down, no chunk hits, feature disabled) clients
get plain results without snippets, and clients that only request the parent
kind are completely unaffected.

Snippet events are signed with `RELAY_SECKEY` (a dedicated relay key — set
it in `.env` for a stable signer identity; when unset a fresh key is
generated per boot and its pubkey printed in the startup log).

Test with nak:

```bash
nak req --search "photosynthese" -k 30142 -k 21142 --limit 5 ws://localhost:3334
```

## Access control & authentication

By default the relay is **open for reads and restricted for writes** to the kinds
it serves: NIP-11 advertises `restricted_writes: true` and `auth_required: false`.
A NIP-42 `AUTH` challenge is sent on every connection, but nothing requires a
response until you restrict access.

Both gates are runtime state held in BoltDB and toggled over NIP-86 — there is no
env var for either.

**Write allowlist.** With `write_restricted` on, `OnEvent` rejects any author who
is neither a NIP-86 admin nor on the write allowlist, with
`restricted: pubkey not on write allowlist`.

**Read allowlist.** With `read_restricted` on, every REQ and COUNT is gated:

- internal calls bypass the check entirely;
- an unauthenticated connection gets `auth-required: authentication required to read`;
- admins bypass;
- an authenticated pubkey not on the read allowlist gets
  `restricted: pubkey not on read allowlist`.

Live pushes to open subscriptions are gated the same way, so restricting reads
does not leak events through an already-open subscription. NIP-11
`limitation.auth_required` **flips to `true` dynamically** whenever reads are
restricted, so clients discover the requirement by fetching the relay document
rather than being surprised by a rejection.

**List references.** Instead of enumerating pubkeys one by one, point the relay at
a remote kind-3 (follow list) or kind-30000 (categorized people list) event: its
`p` tags are resolved from the named relays and merged into the allowlists.
`direction` is `write`, `read` or `both` (default `both`). A reference is fetched
immediately when added and re-resolved every 5 minutes, plus on demand via
`refreshlistreferences`.

> A list that fails to resolve — relay unreachable, event not found — contributes
> **no** pubkeys and is simply skipped. It does not fail closed. If your allowlist
> depends entirely on a remote list, an outage at that relay silently shrinks the
> allowlist to its directly-added entries at the next refresh.

Recipe, using the `call` helper from
[Schema migrations after a release](#schema-migrations-after-a-release):

```bash
# Restrict reads and allow one pubkey
call setaccesscontrol '[{"read_restricted":true}]'
call addtoreadallowlist '["<hex pubkey>","trusted client"]'

# Verify both the relay state and what clients will discover
call getaccesscontrol
curl -s -H 'Accept: application/nostr+json' "$HOST" | jq .limitation.auth_required   # true

# Back to open
call setaccesscontrol '[{"read_restricted":false}]'
```

## Deployment

The full stack is **six services**: amb-relay, Typesense, embed (in-stack
sentence-transformers), Tika (PDF extraction), amb-indexer, and the one-shot
`amb-indexer-initdata`. Compose orchestrates all of them from this repo.

### Prerequisites

- Docker Engine 20+ with Compose v2.24+ (for the `env_file` form used below)
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

Two env files, both loaded by `docker-compose.yml`:

| File | Purpose | Template |
|------|---------|----------|
| `./.env` | Everything the relay reads — metadata, Typesense, content-type flags, ACL-independent toggles | `.env.example` |
| `../amb-indexer/.env.indexer` | Indexer keypair, embed endpoint, fetch limits, license allowlist | `../amb-indexer/.env.indexer.example` |

`./.env` is passed to the relay **in full**; compose overrides exactly one value,
`TS_HOST`, because the relay reaches Typesense over the compose network rather
than the host port `.env` points at for local development. Any variable absent
from `.env` falls back to its code default.

If you don't create `../amb-indexer/.env.indexer`, the `amb-indexer` service fails to start.

### Two compose modes

- **Source-tree (default)**: `docker-compose.yml` builds amb-indexer from `../amb-indexer`. Clone both repos as siblings under one directory.
- **Published image**: replace `build: ../amb-indexer` with `image: git.edufeed.org/edufeed/amb-indexer:main` (or a pinned `vX.Y.Z` / short-sha tag) and you only need amb-relay cloned. The relay itself also publishes to `git.edufeed.org/edufeed/amb-relay`.

Both repos publish images on push to `main` and on `v*` tags via Forgejo Actions.

### Persistent data

Data lives in **four** locations — back up all of them:

| Location | Service | What |
|----------|---------|------|
| `relay_data` (named volume) | amb-relay | BoltDB at `/root/data/relay.db` — raw events, ban lists, ACL state, refetch queue. Also holds the calendar index when enabled. **Source of truth.** Typesense can be rebuilt from it. |
| `./typesense-data/` (bind mount) | Typesense | Search index, every collection. Rebuildable from BoltDB via NIP-86 `reindex`. |
| `indexer_data` (named volume) | amb-indexer | `/data/indexer.db` (cursor, event-hash, dead-letter). Loseable — indexer will replay from the relay on next start. |
| `embed_model_cache` (named volume) | embed | HuggingFace model cache (~120 MB). Loseable — re-downloads on first boot. |

The compose file ships a one-shot `amb-indexer-initdata` service that chowns `indexer_data` to nonroot uid 65532 (the indexer's distroless user). It runs once before `amb-indexer` starts.

### Graceful shutdown

The relay handles SIGTERM/SIGINT and drains its in-memory write buffers before
exiting. That drain needs **time**: set `stop_grace_period` to at least **30s**,
and ~60s where the orchestrator allows it, on the `amb-relay` service. Without
it, SIGKILL cuts the drain short and queued-but-unflushed events are lost from
Typesense on every deploy. Whatever is abandoned is logged with its queue depth
and remains recoverable via `reindex`.

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

To confirm which content types a running relay actually serves:

```bash
curl -s -H 'Accept: application/nostr+json' http://localhost:3334/ | jq '.retention[0].kinds'
```

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

# Resolve an organisation or person name to a pubkey (PROFILES_ENABLED)
nak req -k 0 --search "e-teaching.org" ws://localhost:3334
nak req -k 0 --search "e-teaching.org nip05_verified:true" ws://localhost:3334
```

See [Canonical queries](#canonical-queries) for the per-content-type shapes
(calendar ranges, geohash, `partOf`, DOI, community).

**Note:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`, `#ext:<ns>:<facet>:id`). For these filters, use a Go client with `nostr.TagMap`. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation, including the [`ext:` extension namespace](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md#extension-namespace-ext) for non-AMB-core metadata fields.

### Direct Typesense debugging

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&per_page=1"
```

Swap `$TS_COLLECTION` for any other collection name from the
[Content types](#content-types) table to inspect that type's documents.

## NIP-86 Management API

The relay supports [NIP-86](https://github.com/nostr-protocol/nips/blob/master/86.md) for remote management via HTTP. Send a POST request to the relay URL with `Content-Type: application/nostr+json+rpc` and a [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) `Authorization` header.

Only the relay operator (`PUBKEY`) and additional admins (`ADMIN_PUBKEYS`, plus
any granted via `grantadmin`) are authorized.

### Supported methods

| Method | Description |
|--------|-------------|
| `supportedmethods` | Lists the *typed* NIP-86 handlers this relay implements. The custom methods documented below are dispatched through khatru's generic handler and appear only as `generic` — they are **not** enumerated here |
| `banpubkey` | Ban a pubkey from publishing |
| `listbannedpubkeys` | List all banned pubkeys |
| `allowpubkey` | Remove a pubkey ban |
| `banevent` | Delete an event by ID and block resubmission. Removes from BoltDB, ContentStore, and Typesense, then records the id on the ban list so the same event cannot be re-published. |
| `listbannedevents` | List all banned event IDs |
| `allowevent` | Remove an event ban (does **not** restore the event — the data is gone; only clears the resubmission block) |
| `grantadmin` | Grant NIP-86 access to a pubkey, persisted in BoltDB. Empty `methods` grants full access |
| `revokeadmin` | Revoke NIP-86 access. Fails with `cannot revoke env-configured admin` for pubkeys that came from `PUBKEY` / `ADMIN_PUBKEYS` |
| `changerelayname` | Update relay name (in memory) |
| `changerelaydescription` | Update relay description |
| `changerelayicon` | Update relay icon URL |
| `stats` | Returns `{event_count, uptime}`. **`event_count` covers kind 30142 only** — it is not a total across enabled content types |

Ban lists are persisted in BoltDB and survive restarts.

### Access-control methods

Backing the [Access control](#access-control--authentication) section. All
persisted in BoltDB.

| Method | Params | Description |
|--------|--------|-------------|
| `getaccesscontrol` | none | Returns the current restriction flags, both allowlists, and all list references |
| `setaccesscontrol` | `[{write_restricted: bool, read_restricted: bool}]` | Flip either gate |
| `addtowriteallowlist` | `[pubkey, reason?]` | Add a pubkey to the write allowlist |
| `removefromwriteallowlist` | `[pubkey]` | Remove a pubkey from the write allowlist |
| `addtoreadallowlist` | `[pubkey, reason?]` | Add a pubkey to the read allowlist |
| `removefromreadallowlist` | `[pubkey]` | Remove a pubkey from the read allowlist |
| `addlistreference` | `[{pubkey, kind, d?, relays: [...], direction?}]` | Back an allowlist with a remote list. `kind` must be `3` or `30000`; `relays` must be non-empty; `direction` ∈ `write`/`read`/`both`, default `both`. Fetched immediately |
| `removelistreference` | `[pubkey, kind?, d?]` | Remove a list reference. `kind` defaults to `3` |
| `refreshlistreferences` | none | Re-resolve every list reference now. Returns `{refreshed: N}` |

### Typesense management methods

These custom methods control the Typesense search index schema, reindexing, and collection settings.

| Method | Params | Description |
|--------|--------|-------------|
| `getcollectionschema` | none | Returns the current AMB collection schema (custom or default) |
| `updatecollectionschema` | `[{fields: [...], default_sorting_field: "...", enable_nested_fields: bool}]` | Stores a new schema in BoltDB. Does **not** apply until `reindex` is called |
| `resetcollectionschema` | none | Removes the custom schema, reverting to the hardcoded default |
| `reindex` | none | Rebuilds **every currently enabled** content type's collection from BoltDB. Runs asynchronously — see below |
| `getreindexstatus` | none | Returns `{running, total, indexed, errors, error}` |

`reindex` is not AMB-only. The AMB collection goes through its own path (schema
recreate → batched upsert → fulltext replay → orphan GC); each enabled structured
type is then rebuilt by drop + reproject. Calendar additionally rebuilds its
BoltDB start/end/geohash index, and publications replay indexer-written kind-30040
fulltext — without that replay, reprojection alone would write empty content,
since NKBIP-01 requires the signed 30040 event's content to be empty. Kind-0
profiles are **not** reindexed: they are not persisted in BoltDB.

Schema changes are deferred — `updatecollectionschema` only stores the schema, and
`reindex` applies it. Search is unavailable for the duration, across **all**
content types, and with several enabled the window is the sum of them rebuilt
serially.

#### Schema migrations after a release

When a new relay release changes the hardcoded default schema (e.g. adding a new indexed field), running operators need to roll the live Typesense collection forward. BoltDB is the source of truth, so the recipe is "drop and rebuild from BoltDB".

NIP-86 management methods are served over an **HTTP** endpoint (`POST https://<relay-host>/`) with `Content-Type: application/nostr+json+rpc` and a NIP-98 `Authorization` header — **not** over the websocket. (`nak event -k 24242 ... wss://...` does not work; the relay rejects kind-24242 events on the WS path.) The body is `{"method":"<method>","params":[...]}`. The relay's NIP-98 check requires the auth event (kind 27235) to carry a `u` tag matching the relay base URL and a `payload` tag holding the hex SHA-256 of the exact request body, signed no more than 30s before the request.

```bash
HOST="https://<relay-host>"

call() {  # call <method> [params-json]
  local body="{\"method\":\"$1\",\"params\":${2:-[]}}"
  local hash=$(printf '%s' "$body" | sha256sum | cut -d' ' -f1)
  local auth=$(nak event -k 27235 \
    -t "u=$HOST" -t "method=POST" -t "payload=$hash" \
    --sec "$ADMIN_NSEC" | base64 -w0)
  curl -s -X POST "$HOST/" \
    -H "Content-Type: application/nostr+json+rpc" \
    -H "Authorization: Nostr $auth" \
    -d "$body"
}

# 1. Pull the new code and restart the relay container
docker compose pull && docker compose up -d --build amb-relay

# 2. Apply the new default schema (clears any stored custom schema)
call resetcollectionschema

# 3. Drop and rebuild every enabled collection from BoltDB
call reindex

# 4. Poll until done
call getreindexstatus
```

Skip step 2 if the running relay was previously configured with `updatecollectionschema` and you want to keep that custom schema. Step 3 also re-runs the fulltext replay pass, so `content` rows are preserved across the rebuild. Search is unavailable for the duration of step 3.

### Semantic search & content methods

| Method | Params | Description |
|--------|--------|-------------|
| `getsemanticsearchconfig` | none | Returns `{enabled, embed_fields}` |
| `updatesemanticsearchconfig` | `[{enabled: bool, embed_fields: [...]}]` | Update config and toggle embedding |
| `enablesemanticsearch` | none | Shortcut to enable with default fields |
| `disablesemanticsearch` | none | Shortcut to disable |
| `setcontent` | `[event_id, text, fetched_at, status, source_url]` | Persist fetched fulltext for an event. **Kind-routed**: a kind-30040 event PATCHes the publications collection directly and fails with `publications not enabled` when the flag is off; every other kind goes through the buffered AMB content path. `status` values: `fetched` \| `truncated` \| `license_denied` \| `unsupported` \| `failed`. Admin only. |
| `refetchcontent` | `[event_id]` | Clear fulltext for an event so the indexer reprocesses it. Kind-routed the same way as `setcontent`. Admin only. |
| `listrefetch` | none | List event IDs currently flagged for re-ingestion (set by `refetchcontent`). The indexer polls this. Admin only. |
| `acknowledgerefetch` | `[event_ids]` | Indexer calls this to remove ids from the refetch queue after re-enqueueing. Returns `{acknowledged: N}`. Admin only. |

**Default embed fields:** `name`, `description`, `keywords`, `about`

When enabled, new events are embedded on save and queries use hybrid search (30% vector, 70% keyword weight). Existing events need `reindex` to add embeddings.

### Resource fulltext

The relay stores optional fulltext extracted from the resource referenced by an
event. Fulltext is provided by the external `amb-indexer` service via the
`setcontent` NIP-86 method, persisted in the `fetched_content` BoltDB bucket, and
projected onto the `content`, `content_fetched_at`, and `content_status` fields of
the Typesense document.

When present, the `content` field participates in BM25 search alongside metadata fields. Reindex preserves fulltext: after a collection is rebuilt from BoltDB events, a replay pass PATCHes each stored content row back onto its Typesense document. The same pass drops rows whose events have been deleted (orphan GC) — but only when the event is gone from BoltDB entirely, since a content row may belong to a collection other than AMB.

The indexer service connects as a regular Nostr client + a NIP-86 admin. Add its pubkey to `ADMIN_PUBKEYS` to authorize `setcontent` and `refetchcontent`. See `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md` for the full design.

## Operations

### Mirroring events from another relay

To stage events from a remote relay (e.g. prod) into this one, use the
`mirror-prod` tool that ships with `amb-indexer`. It paginates REQs and
republishes events verbatim — signatures carry over, so the destination
relay accepts the source events as-is. See
**[amb-indexer's *Mirror events from another relay* section](https://git.edufeed.org/edufeed/amb-indexer#user-content-mirror-events-from-another-relay)**
for flag reference, recipes, caveats (no re-signing, no content/chunks,
no kind-5), and alternatives (NIP-77 Negentropy, NIP-86 `reindex`).

### Recovering from BoltDB ↔ Typesense skew

The relay's REQ path treats Typesense as a search index and reads each
hit's raw payload from BoltDB by event ID. If a document is present in
Typesense but the corresponding event is missing from BoltDB, the relay
logs `Search succeeded, found N events` but silently delivers nothing
over the websocket. This skew can happen when an operator restores or
seeds the Typesense collection through a path that bypasses the relay's
dual-write (e.g. a raw Typesense snapshot copy, or a one-time bulk
import script).

`mirror-prod` and NIP-86 `reindex` both go through the relay's normal
accept/save path, so they do **not** create skew. The fix only applies
to operators who seeded Typesense out of band.

To reconcile, run the `hydrate-bolt` tool that ships in the relay image.
It scans Typesense for every document, decodes the stored `eventRaw`
JSON, and writes any missing events into BoltDB. It is idempotent —
already-present events are counted and skipped.

> **Scope:** `hydrate-bolt` scans the AMB collection (`TS_COLLECTION`) only.
> Structured collections are not covered — they are rebuilt from BoltDB by
> NIP-86 `reindex` instead.

After the canonical save pass, `hydrate-bolt` runs a second verify pass
that cross-checks every Typesense doc's `eventID` field against BoltDB
and re-saves under the TS-side id on miss. This surfaces the rare case
where a TS doc's `eventID` has drifted from the hash of its own
`eventRaw` payload — a single such doc per page is enough to silently
truncate downstream REQ pagination, so the verify pass is worth running
even when the canonical save reports no missing events.

BoltDB requires an exclusive lock, so the relay container must be
stopped first:

```bash
docker compose stop amb-relay

docker run --rm \
  --network <your-stack>_default \
  -v <your-stack>_relay_data:/data \
  -e TS_HOST=http://typesense:8108 \
  -e TS_APIKEY=$TS_APIKEY \
  -e TS_COLLECTION=$TS_COLLECTION \
  -e DB_PATH=/data/relay.db \
  --entrypoint /root/hydrate-bolt \
  git.edufeed.org/edufeed/amb-relay:<tag>

docker compose start amb-relay
```

Use `--dry-run` first to see what the scan finds without writing.
Expected output on success:

```
DONE scanned=<N> saved=<missing> already=<already-present> parseErr=0 saveErr=0 mismatches=<M> resaved=<M>
```

If `saved` or `mismatches` is non-zero, REQ throughput for the affected
kinds will jump on next start.

**In-process variant:** set `HYDRATE_ON_START=true` in the relay's env
to run the same scan-and-verify pass as the first step of the relay
process — no stop/start dance needed, since the in-process variant
already holds the BoltDB lock. Logs the same `DONE …` summary before
opening the listener, then continues to normal startup. Fail-open on
Typesense errors (a boot blip won't loop-restart the relay) and bounded
by a 30-min context. Trade-off: adds startup time proportional to
corpus size (≈3 min per 100k events for the two passes). Idempotent
and off by default; turn it on per-instance for deployments that have
ever exhibited the silent-drop pagination skew.

## Architecture

The relay is one service in a six-service stack (relay, Typesense, embed, Tika, amb-indexer, and the one-shot indexer init). For the full system diagram, the data flows, the refetch loop, and end-to-end verification, see **[`docs/architecture.md`](docs/architecture.md)**.

At a glance the relay itself does:

- **Khatru**: Nostr relay framework (part of the [nostrlib](https://git.edufeed.org/edufeed/nostrlib) fork)
- **Content-type registry**: one registration per served kind — validator, storage, fetch and delete. A filter's `kinds` selects which backends to query; a filter without `kinds` fans out across all of them serially, bounded by `QUERY_FETCH_TIMEOUT_MS`
- **Typesense**: full-text search backend. Each content type projects into its **own** collection — `TS_COLLECTION` for AMB plus one per enabled type
- **BoltDB**: embedded key-value store — raw event persistence, ban lists, access-control state, refetch queue, and the calendar range index
- **NIP-86**: HTTP management API for ban lists, access control, schema, reindexing, semantic search, and the indexer-feedback methods (`setcontent` / `refetchcontent` / `listrefetch` / `acknowledgerefetch`)
