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
```

**Note:** `nak` does not support colon-delimited tag names (`#about:id`, `#learningResourceType:id`). For these filters, use a Go client with `nostr.TagMap`. See the [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation.

### Direct Typesense debugging

```bash
curl -H "X-TYPESENSE-API-KEY: $TS_APIKEY" \
  "$TS_HOST/collections/$TS_COLLECTION/documents/search?q=*&per_page=1"
```

## Event Validation

The relay only accepts kind 30142 events and requires:
- A `d` tag (resource identifier)
- A `name` tag (resource title)

Events missing these tags are rejected.

## Architecture

```
Nostr Client → Khatru Relay (:3334) → TSBackend → Typesense (:8108)
```

- **Khatru**: Nostr relay framework (part of nostrlib fork)
- **TSBackend**: Typesense eventstore for kind 30142 (part of nostrlib fork)
- **Typesense**: Full-text search engine with nested field support
