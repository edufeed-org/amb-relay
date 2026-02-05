# AMB Relay

A Nostr relay for AMB (Learning Resource Metadata) events (kind 30142). Built on the [khatru](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/khatru) relay framework with [Typesense](https://typesense.org/) as the full-text search backend.

## Quick Start

1. Copy `.env.example` to `.env` and fill in your values
2. Run `docker compose up`

The relay listens on `:3334`, Typesense on `:8108`.

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

### Storage & Management

| Variable | Description | Default |
|----------|-------------|---------|
| `DB_PATH` | Path to BoltDB file for raw event persistence | `./data/relay.db` |
| `ADMIN_PUBKEYS` | Comma-separated hex pubkeys for NIP-86 management API access (in addition to `PUBKEY`) | empty |

## Deployment

Docker Compose runs both Typesense and the relay:

```bash
docker compose up -d --build
```

The Docker build downloads all dependencies from git.edufeed.org — no additional repos or local files needed.

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
```

**Note:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`). For these filters, use a Go client with `nostr.TagMap`. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation.

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
| `banevent` | Ban an event by ID |
| `listbannedevents` | List all banned event IDs |
| `allowevent` | Remove an event ban |
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

## Architecture

```
Nostr Client → Khatru Relay (:3334) ─┬→ Typesense (:8108)  [search index]
                                      └→ BoltDB             [raw event storage]
```

- **Khatru**: Nostr relay framework (part of nostrlib fork)
- **Typesense**: Full-text search engine — queries go here
- **BoltDB**: Embedded key-value store — raw event persistence for backup/reindexing
- **NIP-86**: HTTP management API for banning, relay metadata, and stats
