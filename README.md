# Typesense Relay for AMB Metadata

Based on the [khatru](https://github.com/fiatjaf/khatru) relay.

## Quick Start

1. Copy `.env.example` to `.env`
2. Add metadata to your relay and typesense connection info
3. Start typesense with `docker compose up -d typesense`
4. Start relay with `go run .`

## Development vs Production

### Local Development

For local development, you want changes to the `eventstore` module to be immediately available without publishing to GitHub.

**Prerequisites:**

1. Clone both repos side-by-side:
   ```
   coding/edufeed/
   ├── amb-relay/
   └── eventstore/
   ```

2. Create a `go.work` file in `amb-relay/`:
   ```go
   go 1.24.1

   use (
       .
       ../eventstore
   )
   ```
   
   > **Note:** `go.work` is already in `.gitignore` - never commit it.

**Running locally:**

```bash
# Start only Typesense in Docker
docker compose up -d typesense

# Set the Typesense host (or add to .env)
export TS_HOST=http://localhost:8108

# Run the relay directly
go run .
```

**Why this works:** The `go.work` file tells Go to use the local `../eventstore` directory instead of downloading from GitHub. Any changes you make to `eventstore` are immediately available when you restart the relay.

### Production Deployment

For production, Docker Compose runs both services:

```bash
docker compose up -d --build
```

**How dependencies work:**
- Docker builds use `go.mod` (not `go.work`)
- `go.mod` downloads dependencies from GitHub
- `go.work` is ignored because it's in `.dockerignore` and `.gitignore`

**When you need to update the eventstore dependency:**

1. Push and tag a new version in the eventstore repo:
   ```bash
   cd ../eventstore
   git add .
   git commit -m "Your changes"
   git tag v0.0.X
   git push origin main --tags
   ```

2. Update go.mod in amb-relay:
   ```bash
   cd ../amb-relay
   go get github.com/edufeed-org/eventstore@v0.0.X
   ```

3. Rebuild Docker images:
   ```bash
   docker compose up -d --build
   ```

### Common Pitfall

**Problem:** "I made changes to eventstore but Docker isn't using them!"

**Cause:** Docker builds ignore `go.work` and use `go.mod`, which downloads the published version from GitHub.

**Solution:** 
- For development: Run the relay with `go run .` (not in Docker)
- For production: Tag and push a new eventstore version, update go.mod, then rebuild

## Examples

```bash
# Full-text search
nak req --search "pythagoras" ws://localhost:3334 | jq .

# Query by kind
nak req -k 30142 -l 2 ws://localhost:3334 | jq .

# Field-specific search (publisher name)
nak req -k 30142 --search "publisher.name:e-teaching.org" ws://localhost:3334 | jq .
```

## Architecture

- **Typesense**: Full-text search engine with nested field support
- **Khatru**: Nostr relay framework
- **eventstore**: Custom Typesense wrapper for Nostr kind:30142 events (AMB metadata)
