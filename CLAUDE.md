# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

AMB Relay is a Nostr relay specializing in AMB (Learning Resource Metadata) events (kind 30142). Built on khatru relay framework with Typesense as the full-text search backend.

## Specifications

* nostr naddr of specification: `naddr1qvzqqqrcvypzp0wzr7fmrcktw4sgemxh5zsq5auh08vnvlwf0x9anusn7pkft0zgqy28wumn8ghj7un9d3shjtnyv9kh2uewd9hsqzm9v36kvet9vskkzmtzvjvrtf`

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
Nostr Client → Khatru Relay (:3334) → TSBackend → Typesense (:8108)
```

**Main Entry Point:** `main.go` - Sets up khatru relay, connects TSBackend via `UseEventstore()`, enables NIP-42 auth and Negentropy protocol.

**Event Flow:**
- Only accepts kind 30142 events (AMB educational metadata)
- Events are indexed in Typesense with full-text search capability
- Queries support NIP-01 filter fields, tag filters, and NIP-50 search — see [eventstore README](https://git.edufeed.org/edufeed/nostrlib/src/branch/master/eventstore/typesense30142/README.md) for full query documentation

## Key Dependencies

- **nostrlib** (`fiatjaf.com/nostr`): Fork of nostr libraries including khatru relay framework and eventstore — hosted at [git.edufeed.org/edufeed/nostrlib](https://git.edufeed.org/edufeed/nostrlib)
- **eventstore/typesense30142**: Typesense wrapper for kind 30142 events (part of nostrlib)

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

## Testing

### nak CLI

```bash
# Full-text search
nak req --search "mathematik" -k 30142 ws://localhost:3334

# Field-specific search
nak req --search "publisher.name:e-teaching.org" -k 30142 ws://localhost:3334

# Time range filter
nak req --since 1700000000 --until 1800000000 -k 30142 ws://localhost:3334
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

