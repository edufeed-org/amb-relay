# Typesense Relay for AMB Metadata

Based on the [khatru](https://github.com/fiatjaf/khatru) relay.

## Quick Start

1. Copy `.env.example` to `.env`
2. Add metadata to your relay and typesense connection info
3. Start typesense with `docker compose up -d typesense`
4. Start relay with `go run .`

## Environment Configuration

When deploying, you need to configure the following environment variables in your `.env` file:

### Relay Metadata
- `NAME`: Your relay's display name (e.g., "AMB Relay")
- `PUBKEY`: Your Nostr public key
- `DESCRIPTION`: A description of your relay (e.g., "AMB Metadata Relay")
- `ICON`: URL to your relay's icon image

### Typesense Configuration

#### `TS_APIKEY`
The API key for authenticating with Typesense.

- **For local development/default docker-compose**: Use `xyz` (as configured in `docker-compose.yml` via `--api-key=xyz`)
- **For production**: Generate a secure random string and use the same value in both your Typesense server config and this variable

#### `TS_HOST`
The URL where Typesense is accessible.

- **For local development** (running relay with `go run .`): `http://localhost:8108`
- **For Docker Compose deployment**: This is automatically set to `http://typesense:8108` in docker-compose.yml, so you can leave it empty in `.env`
- **For external Typesense instance**: Use the full URL (e.g., `https://your-typesense-server.com:8108`)

#### `TS_COLLECTION`
The name of the Typesense collection to store/query events. This is a logical name you choose.

- **Suggested value**: `amb_events` or `nostr_events` or any descriptive name
- This collection will be created automatically if it doesn't exist

### Example `.env` files

**For local development:**
```env
NAME="AMB Relay"
PUBKEY="your-nostr-pubkey"
DESCRIPTION="AMB Metadata Relay"
ICON="https://example.com/icon.png"
TS_APIKEY="xyz"
TS_HOST="http://localhost:8108"
TS_COLLECTION="amb_events"
```

**For Docker Compose:**
```env
NAME="AMB Relay"
PUBKEY="your-nostr-pubkey"
DESCRIPTION="AMB Metadata Relay"
ICON="https://example.com/icon.png"
TS_APIKEY="xyz"
TS_HOST=""
TS_COLLECTION="amb_events"
```

> **Security Note**: For production deployments, change the default `xyz` API key in both `docker-compose.yml` and your `.env` file to a secure random string.

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
