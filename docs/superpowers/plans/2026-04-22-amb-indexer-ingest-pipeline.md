# amb-indexer Ingest Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a standalone Go service (`amb-indexer`) that consumes kind-30142 events from amb-relay, fetches the referenced resource (HTML/PDF/kind-30023), extracts normalized text with structural metadata, chunks it, embeds chunks against the edufeed embedding service, writes chunks to its own Typesense collection `amb_chunks_30142`, and writes fulltext back to the relay via NIP-86 `setcontent`. Implements Phase 1a of the spec at `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md`. Phase 1b (query API, MCP, admin surface, metrics) is a separate plan.

**Architecture:** One nostr subscription with persisted cursor feeds an in-memory queue consumed by N workers. Each worker runs: content-hash skip check → license gate → fetch (dispatched by content-type) → extract → chunk → embed → upsert chunks → callback `setcontent` on relay → advance cursor. Replacement semantics delete prior chunks by `event_coord` before re-ingesting. Failures retry 3× with exponential backoff, then dead-letter in a local BoltDB bucket.

**Tech Stack:** Go 1.22+, `fiatjaf.com/nostr` (client, khatru, nip86, nip98), `go.etcd.io/bbolt`, `github.com/typesense/typesense-go` (or raw HTTP — see Decision D4), `github.com/go-shiori/go-readability` (Decision D1), Apache Tika `latest-full` sidecar for PDF.

---

## Decisions resolved during planning

- **D1. Readability library:** `github.com/go-shiori/go-readability` (maintained, widely used). Pin a tagged release. If it proves unsuitable for OER material during integration testing, `markusmobius/go-trafilatura` is the fallback.
- **D2. Embedder wire format:** Assume `POST {inputs:[...]}` → `{embeddings:[[...]]}` based on common conventions. Task 12 includes a **probe step**: call the endpoint with one test string before writing tests, and adjust the contract if actual shape differs. Document the final contract in the embedder client file's header comment.
- **D3. Relay NIP-86 auth mechanism:** HTTP + NIP-98 (kind 27235) over POST to `RELAY_NIP86_URL`. Exactly matches the relay's `nip86_call` shell helper in `test_e2e.sh` (relay side, dev branch). Content-Type: `application/nostr+json+rpc`. Auth header: `Nostr <base64 NIP-98 event>`. NIP-98 event tags: `u=<url>`, `method=POST`, `payload=<sha256 of body hex>`.
- **D4. Typesense client:** Raw HTTP (like amb-relay's `content_ts.go`). Using a fat client library adds no value for the handful of operations we need. A thin `tsclient.go` with `EnsureCollection`, `Upsert`, `FilterDelete`, `RawSearch` is enough.
- **D5. kind-30023 fetch relays:** Use `RELAY_URL` only for Phase 1. YAGNI on multi-relay federation.
- **D6. Content-hash strategy:** Hash of `source_url`; comparison also considers presence of prior `fetched_at` record. Body hashing deferred to Phase 2.
- **D7. Refetch signaling from relay:** Out of scope for 2a (the indexer does not consume a refetch signal). Operators trigger re-ingestion in 2b via the admin endpoint. For 2a, a manual re-publish of a 30142 with newer `created_at` suffices.
- **D8. Indexer-side Dockerfile & compose:** amb-indexer gets its own repo/directory, but ships via amb-relay's `docker-compose.yml` (operators run one compose stack). Shared `typesense` service, shared internal network. Indexer's own `.env.indexer` file.
- **D9. Replay harness for nostr testing:** Use nostrlib's in-process khatru relay helper if available; otherwise spin up the real relay docker service and point tests at it. Task 4 decides at implementation time.

---

## File Structure

New repo at `/home/laoc/coding/edufeed/amb-indexer/`. Files created by this plan:

| File | Role |
|---|---|
| `go.mod`, `go.sum` | Module `git.edufeed.org/edufeed/amb-indexer`. Go 1.22+. |
| `main.go` | Top-level wiring: config → stores → nostr subscription → worker pool → graceful shutdown. |
| `config.go` | Env-var struct + loader + validation. |
| `config_test.go` | Validation tests. |
| `store.go` | BoltDB setup + `CursorStore`, `EventHashStore`, `DeadLetterStore`. |
| `store_test.go` | Store round-trip tests. |
| `nostr_source.go` | Subscribe to relay, resume from cursor, push events onto queue. |
| `nostr_source_test.go` | Subscription + cursor-resume tests. |
| `ssrf.go` | Host & IP deny-list checks. |
| `ssrf_test.go` | Table-driven SSRF unit tests. |
| `politeness.go` | Per-host concurrency + min-interval token bucket. |
| `politeness_test.go` | Concurrency + timing tests. |
| `fetch.go` | Fetcher dispatch (`FetchHTML`, `FetchPDF`, `FetchKind30023`) with size caps & SSRF. |
| `fetch_html.go` | HTML-specific: readability extraction. |
| `fetch_pdf.go` | Tika HTTP client for PDF → XHTML. |
| `fetch_nostr.go` | kind-30023 fetch via nostr REQ. |
| `fetch_test.go` | Integration tests (httptest upstream, real Tika). |
| `license.go` | License allow-list matcher. |
| `license_test.go` | Table-driven matcher tests. |
| `chunker.go` | Structural + recursive chunking with metadata. |
| `chunker_test.go` | Table-driven tests for HTML, PDF (XHTML), markdown, oversize-recursive. |
| `embed.go` | Batched embedder HTTP client. |
| `embed_test.go` | httptest embedder. |
| `tsclient.go` | Thin Typesense HTTP client: ensure schema, upsert, filter-delete, raw search. |
| `tsclient_test.go` | Tests against a real Typesense container (started by test harness). |
| `schema.go` | The `amb_chunks_30142` schema literal + helper to build chunk documents. |
| `schema_test.go` | Schema literal validation + chunk-doc builder test. |
| `relay_client.go` | NIP-98-signed HTTP POST to relay's NIP-86 `setcontent`. |
| `relay_client_test.go` | Request shaping tests; integration test against real relay. |
| `worker.go` | Per-event pipeline: skip → license → fetch → chunk → embed → upsert → setcontent → cursor-advance → (retry|dead-letter). |
| `worker_test.go` | End-to-end pipeline tests with all externals mocked. |
| `Dockerfile` | Multi-stage build → distroless runtime. |
| `.env.example` | All indexer env vars with comments. |
| `README.md` | Build/run instructions; wire to relay. |
| `docker-compose.override.yml` (in amb-relay) | Adds `amb-indexer` and `tika` services to the existing compose stack. |
| `e2e_test.sh` | Integration suite: start stack, publish one 30142 per content type, assert chunks in TS and fulltext on relay. |

A total of ~30 files, ~3000 lines of Go (estimate).

---

## Task 1: Project skeleton & config loader

**Files:**
- Create: `amb-indexer/go.mod`, `amb-indexer/main.go`, `amb-indexer/config.go`, `amb-indexer/config_test.go`, `amb-indexer/Dockerfile`, `amb-indexer/.env.example`, `amb-indexer/README.md`

- [ ] **Step 1: Create the directory and initialize the module**

```bash
mkdir -p /home/laoc/coding/edufeed/amb-indexer
cd /home/laoc/coding/edufeed/amb-indexer
git init
go mod init git.edufeed.org/edufeed/amb-indexer
```

- [ ] **Step 2: Write the failing config tests**

Create `config_test.go`:

```go
package main

import (
	"testing"
)

func TestConfig_ValidateRequired(t *testing.T) {
	c := Config{}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestConfig_ValidateOK(t *testing.T) {
	c := Config{
		RelayURL:         "wss://relay.example.org",
		RelayNIP86URL:    "https://relay.example.org/",
		IndexerNsec:      "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqg9ddmsy", // any valid nsec
		TSHost:           "http://typesense:8108",
		TSAPIKey:         "xyz",
		TSChunksColl:     "amb_chunks_30142",
		EmbedEndpoint:    "https://embed.example.org/embed",
		TikaURL:          "http://tika:9998",
		DBPath:           "/data/indexer.db",
		Workers:          4,
		FetchMaxBytes:    10_485_760,
		ExtractMaxBytes:  2_097_152,
		FetchTimeout:     30,
		TikaTimeout:      60,
		EmbedTimeout:     30,
		LicenseAllowlist: []string{"CC0", "CC-BY", "CC-BY-SA", "publicdomain"},
		HostMaxConc:      2,
		HostMinInterval:  500,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfig_LoadFromEnv(t *testing.T) {
	t.Setenv("RELAY_URL", "wss://relay.example.org")
	t.Setenv("RELAY_NIP86_URL", "https://relay.example.org/")
	t.Setenv("INDEXER_NSEC", "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqg9ddmsy")
	t.Setenv("TS_HOST", "http://typesense:8108")
	t.Setenv("TS_APIKEY", "xyz")
	t.Setenv("TS_CHUNKS_COLLECTION", "amb_chunks_30142")
	t.Setenv("EMBED_ENDPOINT", "https://embed.example.org/embed")
	t.Setenv("TIKA_URL", "http://tika:9998")
	t.Setenv("DB_PATH", "/data/indexer.db")
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Workers != 4 {
		t.Errorf("default Workers=4, got %d", c.Workers)
	}
	if c.FetchMaxBytes != 10_485_760 {
		t.Errorf("default FetchMaxBytes=10485760, got %d", c.FetchMaxBytes)
	}
}
```

- [ ] **Step 3: Write `config.go`**

```go
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// Upstream
	RelayURL      string
	RelayNIP86URL string
	IndexerNsec   string

	// Typesense (chunk collection)
	TSHost       string
	TSAPIKey     string
	TSChunksColl string

	// Embedding
	EmbedEndpoint string
	EmbedToken    string

	// Tika
	TikaURL string

	// Storage
	DBPath string

	// Workers & limits
	Workers         int
	FetchMaxBytes   int64
	ExtractMaxBytes int64
	FetchTimeout    int // seconds
	TikaTimeout     int // seconds
	EmbedTimeout    int // seconds

	// License policy
	LicenseAllowlist []string

	// SSRF / egress
	EgressDenyCIDRs []string
	EgressDenyHosts []string
	EgressDenyTLDs  []string

	// Politeness
	HostMaxConc     int
	HostMinInterval int // ms
}

func LoadConfig() (Config, error) {
	c := Config{
		RelayURL:        os.Getenv("RELAY_URL"),
		RelayNIP86URL:   os.Getenv("RELAY_NIP86_URL"),
		IndexerNsec:     os.Getenv("INDEXER_NSEC"),
		TSHost:          os.Getenv("TS_HOST"),
		TSAPIKey:        os.Getenv("TS_APIKEY"),
		TSChunksColl:    envDefault("TS_CHUNKS_COLLECTION", "amb_chunks_30142"),
		EmbedEndpoint:   os.Getenv("EMBED_ENDPOINT"),
		EmbedToken:      os.Getenv("EMBED_TOKEN"),
		TikaURL:         os.Getenv("TIKA_URL"),
		DBPath:          envDefault("DB_PATH", "/data/indexer.db"),
		Workers:         envInt("WORKERS", 4),
		FetchMaxBytes:   envInt64("FETCH_MAX_BYTES", 10_485_760),
		ExtractMaxBytes: envInt64("EXTRACT_MAX_BYTES", 2_097_152),
		FetchTimeout:    envInt("FETCH_TIMEOUT_SEC", 30),
		TikaTimeout:     envInt("TIKA_TIMEOUT_SEC", 60),
		EmbedTimeout:    envInt("EMBED_TIMEOUT_SEC", 30),
		LicenseAllowlist: splitCSV(envDefault("LICENSE_ALLOWLIST",
			"CC0,CC-BY,CC-BY-SA,publicdomain")),
		EgressDenyCIDRs: splitCSV(envDefault("EGRESS_DENY_CIDRS",
			"127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,::1/128,fc00::/7,fe80::/10")),
		EgressDenyHosts: splitCSV(envDefault("EGRESS_DENY_HOSTS",
			"metadata.google.internal,metadata.azure.com")),
		EgressDenyTLDs:  splitCSV(envDefault("EGRESS_DENY_TLDS", "onion")),
		HostMaxConc:     envInt("HOST_MAX_CONCURRENCY", 2),
		HostMinInterval: envInt("HOST_MIN_INTERVAL_MS", 500),
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) Validate() error {
	required := map[string]string{
		"RELAY_URL":       c.RelayURL,
		"RELAY_NIP86_URL": c.RelayNIP86URL,
		"INDEXER_NSEC":    c.IndexerNsec,
		"TS_HOST":         c.TSHost,
		"TS_APIKEY":       c.TSAPIKey,
		"EMBED_ENDPOINT":  c.EmbedEndpoint,
		"TIKA_URL":        c.TikaURL,
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if c.Workers < 1 {
		return fmt.Errorf("WORKERS must be >= 1")
	}
	return nil
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envInt64(k string, d int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return d
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 4: Write a minimal `main.go` stub**

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("amb-indexer starting: workers=%d relay=%s\n", cfg.Workers, cfg.RelayURL)
	// Rest wired in Task 17.
	select {}
}
```

- [ ] **Step 5: Write `.env.example`**

```
RELAY_URL=ws://amb-relay:3334
RELAY_NIP86_URL=http://amb-relay:3334/
INDEXER_NSEC=nsec1...

TS_HOST=http://typesense:8108
TS_APIKEY=xyz
TS_CHUNKS_COLLECTION=amb_chunks_30142

EMBED_ENDPOINT=https://embed.edufeed.org/embed
EMBED_TOKEN=

TIKA_URL=http://tika:9998

DB_PATH=/data/indexer.db

WORKERS=4
FETCH_MAX_BYTES=10485760
EXTRACT_MAX_BYTES=2097152
FETCH_TIMEOUT_SEC=30
TIKA_TIMEOUT_SEC=60
EMBED_TIMEOUT_SEC=30

LICENSE_ALLOWLIST=CC0,CC-BY,CC-BY-SA,publicdomain

EGRESS_DENY_CIDRS=127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,::1/128,fc00::/7,fe80::/10
EGRESS_DENY_HOSTS=metadata.google.internal,metadata.azure.com
EGRESS_DENY_TLDS=onion

HOST_MAX_CONCURRENCY=2
HOST_MIN_INTERVAL_MS=500
```

- [ ] **Step 6: Write `Dockerfile`**

```Dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/amb-indexer .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/amb-indexer /amb-indexer
ENTRYPOINT ["/amb-indexer"]
```

- [ ] **Step 7: Write a minimal `README.md`**

Three sections: "What is this", "Run locally", "Configuration" (point at `.env.example`).

- [ ] **Step 8: Run tests + build**

```bash
go test ./...
go build .
```
Expected: tests pass, binary exists.

- [ ] **Step 9: Commit**

```bash
git add .
git commit -m "feat: project skeleton and config loader"
```

---

## Task 2: BoltDB stores (cursor, event_hash, dead_letter)

**Files:**
- Create: `store.go`, `store_test.go`

- [ ] **Step 1: Write failing tests for `CursorStore`**

Test: `Get` before any `Set` returns `(0, false, nil)`. After `Set(1700000000)`, `Get` returns `(1700000000, true, nil)`. Subsequent `Set(1700000050)` overwrites.

- [ ] **Step 2: Write failing tests for `EventHashStore`**

Test: `Get("evt1")` before any `Put` returns `("", false, nil)`. After `Put("evt1", "hash-abc")`, `Get` returns `("hash-abc", true, nil)`.

- [ ] **Step 3: Write failing tests for `DeadLetterStore`**

Test: `Put("evt1", DeadLetterEntry{Error: "dns timeout", SourceURL: "https://...", RetryCount: 3})` then `Get("evt1")` returns the same struct (after JSON round-trip). `List()` returns all entries; `Delete("evt1")` removes one.

- [ ] **Step 4: Write `store.go`**

```go
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketCursor     = []byte("cursor")
	bucketEventHash  = []byte("event_hash")
	bucketDeadLetter = []byte("dead_letter")
	keyCursor        = []byte("nostr_cursor")
)

// OpenBolt opens/creates the bbolt database at path and ensures all buckets exist.
func OpenBolt(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketCursor, bucketEventHash, bucketDeadLetter} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type CursorStore struct{ DB *bbolt.DB }

func (c *CursorStore) Get() (int64, bool, error) {
	var val int64
	var found bool
	err := c.DB.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketCursor).Get(keyCursor)
		if v == nil {
			return nil
		}
		if len(v) != 8 {
			return errors.New("corrupt cursor value")
		}
		val = int64(binary.BigEndian.Uint64(v))
		found = true
		return nil
	})
	return val, found, err
}

func (c *CursorStore) Set(ts int64) error {
	return c.DB.Update(func(tx *bbolt.Tx) error {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(ts))
		return tx.Bucket(bucketCursor).Put(keyCursor, buf[:])
	})
}

type EventHashStore struct{ DB *bbolt.DB }

func (e *EventHashStore) Get(eventID string) (string, bool, error) {
	var val string
	var found bool
	err := e.DB.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketEventHash).Get([]byte(eventID))
		if v != nil {
			val = string(v)
			found = true
		}
		return nil
	})
	return val, found, err
}

func (e *EventHashStore) Put(eventID, hash string) error {
	return e.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketEventHash).Put([]byte(eventID), []byte(hash))
	})
}

type DeadLetterEntry struct {
	Error      string `json:"error"`
	FirstSeen  int64  `json:"first_seen"`
	LastRetry  int64  `json:"last_retry"`
	RetryCount int    `json:"retry_count"`
	SourceURL  string `json:"source_url,omitempty"`
}

type DeadLetterStore struct{ DB *bbolt.DB }

func (d *DeadLetterStore) Put(eventID string, entry DeadLetterEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return d.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeadLetter).Put([]byte(eventID), raw)
	})
}

func (d *DeadLetterStore) Get(eventID string) (DeadLetterEntry, bool, error) {
	var entry DeadLetterEntry
	var found bool
	err := d.DB.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketDeadLetter).Get([]byte(eventID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &entry)
	})
	return entry, found, err
}

func (d *DeadLetterStore) List() (map[string]DeadLetterEntry, error) {
	out := make(map[string]DeadLetterEntry)
	err := d.DB.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeadLetter).ForEach(func(k, v []byte) error {
			var entry DeadLetterEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			out[string(k)] = entry
			return nil
		})
	})
	return out, err
}

func (d *DeadLetterStore) Delete(eventID string) error {
	return d.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeadLetter).Delete([]byte(eventID))
	})
}
```

- [ ] **Step 5: Run tests, see them pass**

```bash
go test -run 'TestCursor|TestEventHash|TestDeadLetter' -v
```

- [ ] **Step 6: Commit**

```bash
git add store.go store_test.go
git commit -m "feat: BoltDB stores for cursor, event hashes, dead letters"
```

---

## Task 3: SSRF guard

**Files:**
- Create: `ssrf.go`, `ssrf_test.go`

- [ ] **Step 1: Write failing table-driven tests**

Cases: loopback IPv4 (`127.0.0.1`) → block; private range (`10.0.0.5`, `192.168.1.10`, `172.16.0.1`) → block; link-local (`169.254.169.254`) → block; IPv6 loopback `::1` → block; public (`1.1.1.1`, `8.8.8.8`) → allow; host in deny list (`metadata.google.internal`) → block before resolve; `.onion` TLD → block; hostnames that resolve to multiple IPs where any is private → block.

- [ ] **Step 2: Write `ssrf.go`**

Interface:

```go
type SSRFGuard struct {
	denyNets  []*net.IPNet
	denyHosts map[string]bool
	denyTLDs  map[string]bool
	resolver  *net.Resolver // nil → default
}

func NewSSRFGuard(cidrs []string, hosts []string, tlds []string) (*SSRFGuard, error)

// CheckURL returns nil if the URL's host is allowed, error otherwise.
// Resolves DNS and rejects if ANY resolved IP matches a deny CIDR.
func (g *SSRFGuard) CheckURL(rawurl string) error
```

Implementation: parse URL, extract host. Reject if host ∈ `denyHosts` (exact match) or host ends with `.<tld>` for any tld ∈ `denyTLDs`. Else `LookupIPAddr(ctx, host)` and reject if any returned IP is inside any `denyNets` entry.

- [ ] **Step 3: Run tests, pass**

Use a fake resolver for deterministic tests. The `net.Resolver` supports a `Dial` hook; wire a stub that returns preset records per query name.

- [ ] **Step 4: Commit**

```bash
git add ssrf.go ssrf_test.go
git commit -m "feat: SSRF guard with CIDR, host, TLD deny lists"
```

---

## Task 4: Per-host politeness limiter

**Files:**
- Create: `politeness.go`, `politeness_test.go`

- [ ] **Step 1: Write failing tests**

Test 1: three goroutines call `Acquire("example.org")` simultaneously with `maxConc=2`; exactly two proceed immediately, the third waits until one `Release`s.

Test 2: sequential acquire+release on the same host with `minInterval=100ms` — the second `Acquire` blocks for at least 100ms after the first `Release`.

Test 3: different hosts do not interfere.

- [ ] **Step 2: Write `politeness.go`**

Interface:

```go
type PolitenessLimiter struct {
	maxConc     int
	minInterval time.Duration
	mu          sync.Mutex
	// per-host: semaphore channel + last-release timestamp
	hosts map[string]*hostState
}

type hostState struct {
	sem      chan struct{}
	lastDone time.Time
}

func NewPolitenessLimiter(maxConc int, minInterval time.Duration) *PolitenessLimiter

// Acquire blocks until both concurrency and min-interval conditions are met.
// Call Release when the request finishes. ctx cancellation aborts the wait.
func (p *PolitenessLimiter) Acquire(ctx context.Context, host string) error
func (p *PolitenessLimiter) Release(host string)
```

Implementation: per-host `chan struct{}` sized `maxConc` for concurrency; on Acquire also sleep until `now >= lastDone + minInterval`. Use a `*hostState` under `p.mu` (create on first use; never delete — bounded churn, acceptable memory).

- [ ] **Step 3: Run tests**

Use `time.Sleep` tolerances of ±20ms for interval assertions.

- [ ] **Step 4: Commit**

```bash
git add politeness.go politeness_test.go
git commit -m "feat: per-host concurrency and min-interval limiter"
```

---

## Task 5: License gate

**Files:**
- Create: `license.go`, `license_test.go`

- [ ] **Step 1: Write failing table-driven tests**

Cases: `LicensePermissive(["CC0","CC-BY"], "https://creativecommons.org/licenses/by/4.0/")` → true (substring "CC-BY" after normalization); `"ALL"` in allowlist → always true regardless of input; `""` license → false unless `"ALL"` is in allowlist; empty allowlist → always false; case-insensitive match (`cc-by` in input, `CC-BY` in list).

- [ ] **Step 2: Write `license.go`**

```go
package main

import "strings"

// LicensePermissive reports whether the given license string matches any
// entry in the allowlist (case-insensitive substring match). The special
// value "ALL" in the allowlist matches everything.
func LicensePermissive(allowlist []string, license string) bool {
	lic := strings.ToUpper(license)
	for _, a := range allowlist {
		au := strings.ToUpper(strings.TrimSpace(a))
		if au == "ALL" {
			return true
		}
		if au == "" {
			continue
		}
		if lic != "" && strings.Contains(lic, au) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run tests, pass**

- [ ] **Step 4: Commit**

```bash
git add license.go license_test.go
git commit -m "feat: license allowlist matcher"
```

---

## Task 6: HTML fetcher with readability

**Files:**
- Create: `fetch.go`, `fetch_html.go`, `fetch_test.go`

- [ ] **Step 1: Define the shared fetch types in `fetch.go`**

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ExtractedDoc is the output of any fetcher: normalized text + light structure.
// Text may be truncated to Config.ExtractMaxBytes; Truncated indicates so.
type ExtractedDoc struct {
	Text       string
	Kind       string   // "html" | "pdf" | "nostr-long"
	Title      string
	Headings   []Heading
	Pages      []Page   // PDF only
	SourceURL  string
	MIMEType   string
	Truncated  bool
}

type Heading struct {
	Level int    // 1-6
	Text  string
	// Offset is the byte offset into ExtractedDoc.Text where this heading starts.
	Offset int
}

type Page struct {
	Number int
	Offset int // byte offset of page start in ExtractedDoc.Text
}

var (
	ErrUnsupportedType = errors.New("unsupported content type")
	ErrTooLarge        = errors.New("response exceeds FETCH_MAX_BYTES")
	ErrBlocked         = errors.New("blocked by SSRF guard")
)

type Fetcher struct {
	HTTPClient *http.Client
	SSRF       *SSRFGuard
	Politeness *PolitenessLimiter
	MaxBytes   int64         // FETCH_MAX_BYTES — download cap
	ExtractMax int64         // EXTRACT_MAX_BYTES — post-extraction text cap
	Timeout    time.Duration
	TikaClient *TikaClient  // Task 7
	Nostr      NostrFetcher // Task 8
}

// Fetch dispatches by content type detected from HTTP HEAD/GET headers.
// For kind-30023 nostr refs (scheme "nostr:"), dispatches to Nostr.
func (f *Fetcher) Fetch(ctx context.Context, url string) (ExtractedDoc, error) {
	// Implementation in Step 2.
}
```

- [ ] **Step 2: Write `fetch_html.go`**

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
)

// fetchHTML downloads the URL (respecting MaxBytes), runs readability
// extraction, and converts the resulting HTML AST to an ExtractedDoc.
func (f *Fetcher) fetchHTML(ctx context.Context, url string, body io.Reader, contentType string) (ExtractedDoc, error) {
	art, err := readability.FromReader(body, mustParseURL(url))
	if err != nil {
		return ExtractedDoc{}, fmt.Errorf("readability: %w", err)
	}
	text, headings := walkReadability(art.Content, art.Title)
	if int64(len(text)) > f.ExtractMax {
		text = text[:f.ExtractMax]
		return ExtractedDoc{
			Text: text, Kind: "html", Title: art.Title, Headings: headings,
			SourceURL: url, MIMEType: contentType, Truncated: true,
		}, nil
	}
	return ExtractedDoc{
		Text: text, Kind: "html", Title: art.Title, Headings: headings,
		SourceURL: url, MIMEType: contentType,
	}, nil
}

// walkReadability produces normalized plain text with heading offsets.
// Implementation: walk the article HTML, emit text under each <h2>/<h3>
// and record the byte offset at the start of the heading.
func walkReadability(htmlStr string, title string) (string, []Heading) {
	// Parse with golang.org/x/net/html, walk the tree, emit:
	//   - Title first (as H1 at offset 0)
	//   - For each element: whitespace-collapsed text
	//   - Record Heading{Level, Text, Offset} when entering <h1..h6>
	// Return the concatenated string and the heading list.
	// (Detailed implementation left to implementer following the interface contract.)
	return "", nil
}
```

- [ ] **Step 3: Write the dispatcher in `fetch.go`**

```go
func (f *Fetcher) Fetch(ctx context.Context, url string) (ExtractedDoc, error) {
	if strings.HasPrefix(url, "nostr:") {
		return f.Nostr.Fetch(ctx, url)
	}
	if err := f.SSRF.CheckURL(url); err != nil {
		return ExtractedDoc{}, fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	host, err := hostOf(url)
	if err != nil {
		return ExtractedDoc{}, err
	}
	if err := f.Politeness.Acquire(ctx, host); err != nil {
		return ExtractedDoc{}, err
	}
	defer f.Politeness.Release(host)

	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "amb-indexer/1.0 (+https://git.edufeed.org/edufeed/amb-indexer)")
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return ExtractedDoc{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ExtractedDoc{}, fmt.Errorf("http %d", resp.StatusCode)
	}
	reader := io.LimitReader(resp.Body, f.MaxBytes+1)
	buf, err := io.ReadAll(reader)
	if err != nil {
		return ExtractedDoc{}, err
	}
	if int64(len(buf)) > f.MaxBytes {
		return ExtractedDoc{}, ErrTooLarge
	}
	ctype := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ctype, "text/html"):
		return f.fetchHTML(ctx, url, bytes.NewReader(buf), ctype)
	case strings.Contains(ctype, "application/pdf"):
		return f.fetchPDF(ctx, url, buf, ctype) // Task 7
	default:
		return ExtractedDoc{}, fmt.Errorf("%w: %s", ErrUnsupportedType, ctype)
	}
}
```

- [ ] **Step 4: Write integration tests using `httptest`**

Serve a static HTML document (pick a realistic OER example with h2/h3 structure, ~5KB inline). Assert:
- `Text` is non-empty and does NOT contain boilerplate (nav, footer, "Cookie Policy").
- `Headings` contains expected h2 text at the right offsets.
- Oversize test: serve `FetchMaxBytes+1` bytes → `ErrTooLarge`.
- SSRF test: URL resolving to `127.0.0.1` → `ErrBlocked`.

- [ ] **Step 5: Run tests**

```bash
go test -run TestFetchHTML -v
```

- [ ] **Step 6: Commit**

```bash
git add fetch.go fetch_html.go fetch_test.go go.mod go.sum
git commit -m "feat: HTML fetcher with readability extraction and size cap"
```

---

## Task 7: PDF fetcher via Tika

**Files:**
- Create: `fetch_pdf.go`; add to `fetch_test.go`

- [ ] **Step 1: Write failing integration tests**

Use a small checked-in fixture PDF (e.g., `testdata/sample.pdf` — 2-page, with detectable headings). Start a real Tika container for the test (fail the test with a clear skip message if Docker is unavailable). Assert:
- `ExtractedDoc.Kind == "pdf"`.
- `Text` contains known phrases from the PDF.
- `Pages` contains entries for pages 1 and 2 with increasing offsets.
- `Headings` non-empty if the PDF has headings.

- [ ] **Step 2: Write `fetch_pdf.go`**

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/html"
)

type TikaClient struct {
	URL     string // e.g. http://tika:9998
	Timeout time.Duration
	Client  *http.Client
}

// extract posts the PDF body to Tika's /tika endpoint and returns XHTML.
func (t *TikaClient) extract(ctx context.Context, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, t.URL+"/tika", bytes.NewReader(body))
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Content-Type", "application/pdf")
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("tika status %d: %s", resp.StatusCode, snippet)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (f *Fetcher) fetchPDF(ctx context.Context, url string, body []byte, contentType string) (ExtractedDoc, error) {
	xhtml, err := f.TikaClient.extract(ctx, body)
	if err != nil {
		return ExtractedDoc{}, err
	}
	text, headings, pages := walkTikaXHTML(xhtml)
	if int64(len(text)) > f.ExtractMax {
		text = text[:f.ExtractMax]
		return ExtractedDoc{
			Text: text, Kind: "pdf", Headings: headings, Pages: pages,
			SourceURL: url, MIMEType: contentType, Truncated: true,
		}, nil
	}
	return ExtractedDoc{
		Text: text, Kind: "pdf", Headings: headings, Pages: pages,
		SourceURL: url, MIMEType: contentType,
	}, nil
}

// walkTikaXHTML parses Tika's XHTML output:
//   - each <div class="page"> emits a Page with byte offset of its first text
//   - <h1>..<h3> emit Headings with offsets
//   - inline text is whitespace-collapsed and concatenated
func walkTikaXHTML(xhtmlStr string) (text string, headings []Heading, pages []Page) {
	// Parse with golang.org/x/net/html, walk the tree tracking:
	//   - current byte offset in the output buffer
	//   - emit Page on encountering <div class="page">
	//   - emit Heading on encountering <h1..h6>
	//   - emit whitespace-collapsed text on encountering text nodes outside script/style
	return
}
```

- [ ] **Step 3: Run tests against a local Tika container**

```bash
docker run --rm -d -p 9998:9998 --name tika-test apache/tika:latest-full
go test -run TestFetchPDF -v
docker stop tika-test
```

- [ ] **Step 4: Commit**

```bash
git add fetch_pdf.go fetch_test.go testdata/sample.pdf go.mod go.sum
git commit -m "feat: PDF fetcher via Tika XHTML extraction"
```

---

## Task 8: kind-30023 fetcher via nostr REQ

**Files:**
- Create: `fetch_nostr.go`; add to `fetch_test.go`

- [ ] **Step 1: Define `NostrFetcher`**

```go
type NostrFetcher interface {
	Fetch(ctx context.Context, nostrURL string) (ExtractedDoc, error)
}

type nostrFetcher struct {
	RelayURL string
	Pool     *nostr.SimplePool // or equivalent from fiatjaf.com/nostr
}

func (n *nostrFetcher) Fetch(ctx context.Context, nostrURL string) (ExtractedDoc, error) {
	// 1. Decode nostrURL (naddr or nevent) → filter.
	// 2. Connect to n.RelayURL, send REQ, collect first matching event.
	// 3. Treat event.Content as markdown text.
	// 4. Parse ATX headers (# / ## / ###) into Headings with offsets.
	// 5. Return ExtractedDoc{Kind: "nostr-long", ...}.
}
```

- [ ] **Step 2: Write tests**

Use an in-process khatru relay (or start the dev relay via docker compose). Publish a kind-30023 event with known markdown, fetch by its naddr, assert content and headings.

- [ ] **Step 3: Run and commit**

```bash
go test -run TestFetchNostr -v
git add fetch_nostr.go fetch_test.go
git commit -m "feat: kind-30023 fetcher via nostr REQ"
```

---

## Task 9: Chunker — structural pass

**Files:**
- Create: `chunker.go`, `chunker_test.go`

- [ ] **Step 1: Define the interface**

```go
// Chunk is the unit produced by the chunker.
type Chunk struct {
	Idx         int
	Text        string
	Heading     string
	SectionPath []string
	Page        int // 0 if not applicable
}

// ChunkConfig controls size/overlap thresholds.
type ChunkConfig struct {
	TargetChars  int // ~1000 (≈256 tokens for Latin script)
	OverlapChars int // ~200 (≈50 tokens)
}

// Chunk returns the chunk stream for an ExtractedDoc. Structural first;
// oversize sections are handed to the recursive splitter (Task 10).
func Chunk(cfg ChunkConfig, doc ExtractedDoc) []Chunk
```

- [ ] **Step 2: Write tests driving the structural layer only**

Cases:
1. HTML with three h2 sections, each ~300 chars → exactly three chunks, each with correct `Heading` and `SectionPath: [Heading]`.
2. PDF (XHTML via Tika) with two pages, each containing one h2 → two or more chunks with non-zero `Page` and the expected heading.
3. Markdown (kind-30023) with two `##` sections → two chunks.
4. Nested headings: `# A / ## B / ### C` → chunk under C gets `SectionPath: ["A","B","C"]`.

- [ ] **Step 3: Implement the structural layer**

Use `ExtractedDoc.Headings` (for HTML/markdown/PDF) and `Pages` (for PDF) to derive section boundaries. Build a tree of headings where each heading's range = `[Offset, nextSiblingOrAncestorBoundary)`. Emit one `Chunk` per leaf section. If a section exceeds `TargetChars`, hand its text to the recursive splitter (stub for now — returns the whole text as one chunk; Task 10 refines).

- [ ] **Step 4: Run tests**

- [ ] **Step 5: Commit**

```bash
git add chunker.go chunker_test.go
git commit -m "feat: structural chunker for HTML, PDF, markdown"
```

---

## Task 10: Chunker — recursive splitter

**Files:**
- Modify: `chunker.go`; add to `chunker_test.go`

- [ ] **Step 1: Write tests for the recursive splitter**

Cases:
1. A 3000-char paragraph with `TargetChars=1000`, `OverlapChars=200` → ~4 chunks, adjacent chunks share ~200 char overlap.
2. Input containing paragraph breaks (`\n\n`): split on paragraph boundaries first.
3. Input containing only sentence breaks (`. `): fall back to sentence split.
4. Input with no breaks at all: hard cut at `TargetChars` with `OverlapChars` overlap.
5. Heading metadata must be preserved across recursive chunks: each carries the enclosing section's heading + section path.

- [ ] **Step 2: Implement `recursiveSplit`**

```go
// recursiveSplit splits oversize text into chunks of approx TargetChars
// with OverlapChars overlap between adjacent chunks. Tries paragraph
// boundaries, then sentences, then hard cuts.
func recursiveSplit(text string, cfg ChunkConfig) []string
```

Strategy:
- If `len(text) <= cfg.TargetChars`, return `[text]`.
- Split on `\n\n`. If any piece is still oversize, split it on `. ` (keeping the period). If still oversize, hard-cut at `TargetChars`.
- Merge pieces greedily into chunks ≤ `TargetChars`.
- For each adjacent pair of chunks, prepend the last `OverlapChars` of the previous chunk to the next.

- [ ] **Step 3: Wire into `Chunk`**

When a structural section exceeds `TargetChars`, run `recursiveSplit` and emit one `Chunk` per piece (all sharing the same heading + section path).

- [ ] **Step 4: Run tests and commit**

```bash
go test -run TestChunk -v
git add chunker.go chunker_test.go
git commit -m "feat: recursive chunker with paragraph/sentence/hard fallbacks"
```

---

## Task 11: Embedder client

**Files:**
- Create: `embed.go`, `embed_test.go`

- [ ] **Step 1: Probe the real embedder**

```bash
curl -X POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $EMBED_TOKEN" \
  -d '{"inputs":["hello world"]}' \
  "$EMBED_ENDPOINT"
```

Note the actual response shape. If it differs from `{"embeddings":[[...]]}`, update the struct and tests in Step 2.

- [ ] **Step 2: Write failing tests**

Use `httptest.NewServer` returning a canned `{"embeddings":[[0.1,0.2,...384 dims...]]}`. Assert:
- Single input → returns `[]float32` of length 384.
- 40 inputs, batch size 32 → two HTTP calls made (inspect via counter).
- 5xx response → returns error (transient; retry handled at worker level).
- Mismatched embedding count (server returned 3 for 5 inputs) → error.

- [ ] **Step 3: Implement `embed.go`**

```go
type Embedder struct {
	Endpoint string
	Token    string
	BatchSize int
	Timeout   time.Duration
	Client    *http.Client
}

// Embed encodes the batch via the /embed endpoint. Batches internally
// when len(inputs) > BatchSize. Returns one []float32 per input, in order.
func (e *Embedder) Embed(ctx context.Context, inputs []string) ([][]float32, error)
```

Request body: `{"inputs": [string,...]}`. Authorization: `Bearer <Token>` (omit header if Token empty). Response: `{"embeddings": [[float,...],...]}` — length must equal `len(inputs)` per batch.

- [ ] **Step 4: Run tests and commit**

```bash
go test -run TestEmbed -v
git add embed.go embed_test.go
git commit -m "feat: batched embedder HTTP client"
```

---

## Task 12: Typesense thin client + schema

**Files:**
- Create: `tsclient.go`, `schema.go`, `tsclient_test.go`, `schema_test.go`

- [ ] **Step 1: Write `schema.go`**

```go
package main

// ChunksSchema is the literal for Typesense collection amb_chunks_30142.
// Field order matches the design spec.
var ChunksSchema = map[string]any{
	"name": "amb_chunks_30142",
	"fields": []map[string]any{
		{"name": "id", "type": "string"},
		{"name": "event_id", "type": "string", "facet": true},
		{"name": "event_coord", "type": "string", "facet": true},
		{"name": "pubkey", "type": "string", "facet": true},
		{"name": "chunk_idx", "type": "int32"},
		{"name": "text", "type": "string", "optional": true},
		{"name": "snippet", "type": "string"},
		{"name": "embedding", "type": "float[]", "num_dim": 384, "vec_dist_metric": "cosine"},
		{"name": "heading", "type": "string", "optional": true},
		{"name": "section_path", "type": "string[]", "optional": true},
		{"name": "page", "type": "int32", "optional": true},
		{"name": "source_url", "type": "string"},
		{"name": "content_type", "type": "string", "facet": true},
		{"name": "license", "type": "string", "facet": true, "optional": true},
		{"name": "license_permissive", "type": "bool", "facet": true},
		{"name": "about_id", "type": "string[]", "facet": true, "optional": true},
		{"name": "learning_resource_type", "type": "string[]", "facet": true, "optional": true},
		{"name": "created_at", "type": "int64"},
	},
	"default_sorting_field": "created_at",
}

// ChunkDoc is the Typesense document shape for a single chunk.
type ChunkDoc struct {
	ID                   string    `json:"id"`
	EventID              string    `json:"event_id"`
	EventCoord           string    `json:"event_coord"`
	Pubkey               string    `json:"pubkey"`
	ChunkIdx             int       `json:"chunk_idx"`
	Text                 string    `json:"text,omitempty"`
	Snippet              string    `json:"snippet"`
	Embedding            []float32 `json:"embedding"`
	Heading              string    `json:"heading,omitempty"`
	SectionPath          []string  `json:"section_path,omitempty"`
	Page                 int       `json:"page,omitempty"`
	SourceURL            string    `json:"source_url"`
	ContentType          string    `json:"content_type"`
	License              string    `json:"license,omitempty"`
	LicensePermissive    bool      `json:"license_permissive"`
	AboutID              []string  `json:"about_id,omitempty"`
	LearningResourceType []string  `json:"learning_resource_type,omitempty"`
	CreatedAt            int64     `json:"created_at"`
}

// BuildChunkDocs assembles chunk documents from the given inputs.
// Snippet is the first 200 chars of chunk.Text (fair-use fragment).
// If !permissive, Text is omitted (empty string + json omitempty).
func BuildChunkDocs(
	event nostr.Event,           // the kind 30142 source event
	sourceURL, contentType string,
	permissive bool,
	chunks []Chunk,
	embeddings [][]float32,
	amb AMBMetadata,             // about_id, learning_resource_type, license
) ([]ChunkDoc, error)
```

- [ ] **Step 2: Write schema literal test**

Validate JSON-marshalability, field count, and that `num_dim` is `384` on `embedding`.

- [ ] **Step 3: Write `tsclient.go`**

```go
type TSClient struct {
	Host       string
	APIKey     string
	HTTPClient *http.Client
}

// EnsureCollection creates the collection if it doesn't exist; no-op otherwise.
// Does NOT drop-and-recreate (destructive operations happen only via NIP-86 on the relay).
func (t *TSClient) EnsureCollection(ctx context.Context, schema map[string]any) error

// Upsert POSTs the documents via the batch import endpoint with action=upsert.
func (t *TSClient) Upsert(ctx context.Context, collection string, docs []ChunkDoc) error

// FilterDelete runs `DELETE /collections/{c}/documents?filter_by=<filter>`.
// Used on replacement to drop prior chunks for an event_coord.
func (t *TSClient) FilterDelete(ctx context.Context, collection, filter string) (deletedCount int, err error)
```

All calls use `X-TYPESENSE-API-KEY: t.APIKey`. Errors read up to 512 bytes of the response body for diagnostic context.

- [ ] **Step 4: Write integration tests**

Start a real Typesense via testcontainers-go or docker compose, then:
1. `EnsureCollection` on a fresh TS → collection exists.
2. `EnsureCollection` a second time → no error, no change.
3. `Upsert` three `ChunkDoc`s → GET `/collections/c/documents/<id>` returns them.
4. `FilterDelete(c, "event_coord:=30142:abc:d1")` → removes only those docs.

- [ ] **Step 5: Run tests and commit**

```bash
go test -run 'TestTS|TestSchema' -v
git add tsclient.go schema.go tsclient_test.go schema_test.go
git commit -m "feat: thin Typesense client and chunks schema"
```

---

## Task 13: Relay client — NIP-86 setcontent callback

**Files:**
- Create: `relay_client.go`, `relay_client_test.go`

- [ ] **Step 1: Define the interface**

```go
type RelayClient struct {
	NIP86URL   string // e.g. http://amb-relay:3334/
	SecretKey  nostr.SecretKey
	HTTPClient *http.Client
}

// SetContent calls the relay's NIP-86 `setcontent` method.
// Params order: [event_id, text, fetched_at, status, source_url].
func (r *RelayClient) SetContent(ctx context.Context, eventID, text string, fetchedAt int64, status, sourceURL string) error
```

Status is one of: `"fetched"`, `"truncated"`, `"license_denied"`, `"unsupported"`, `"failed"`.

- [ ] **Step 2: Write tests**

Using `httptest.NewServer`:
1. Assert request method is POST, Content-Type is `application/nostr+json+rpc`.
2. Assert Authorization header starts with `Nostr ` and base64-decodes to a valid NIP-98 event (kind 27235) whose `u` tag = server URL and `payload` tag = SHA-256 of body.
3. Assert request body parses as `{"method":"setcontent", "params":[<id>, <text>, <fetched_at>, <status>, <source_url>]}`.
4. Server returns `{"result":true}` → no error.
5. Server returns `{"error":"event not found"}` → error with that message.
6. Server returns HTTP 500 → error.

- [ ] **Step 3: Implement `relay_client.go`**

```go
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"fiatjaf.com/nostr"
)

type rpcRequest struct {
	Method string          `json:"method"`
	Params []any           `json:"params"`
}

type rpcResponse struct {
	Result any    `json:"result"`
	Error  string `json:"error,omitempty"`
}

func (r *RelayClient) callNIP86(ctx context.Context, method string, params []any) (any, error) {
	body, err := json.Marshal(rpcRequest{Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	payloadHash := sha256.Sum256(body)
	auth := buildNIP98Event(r.SecretKey, r.NIP86URL, "POST", hex.EncodeToString(payloadHash[:]))
	authJSON, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	authB64 := base64.StdEncoding.EncodeToString(authJSON)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.NIP86URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/nostr+json+rpc")
	req.Header.Set("Authorization", "Nostr "+authB64)

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("nip86 %s status %d: %s", method, resp.StatusCode, respBody)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(respBody, &rpc); err != nil {
		return nil, fmt.Errorf("decode rpc response: %w", err)
	}
	if rpc.Error != "" {
		return nil, fmt.Errorf("nip86 %s: %s", method, rpc.Error)
	}
	return rpc.Result, nil
}

func (r *RelayClient) SetContent(ctx context.Context, eventID, text string, fetchedAt int64, status, sourceURL string) error {
	_, err := r.callNIP86(ctx, "setcontent", []any{eventID, text, fetchedAt, status, sourceURL})
	return err
}

// buildNIP98Event constructs a signed kind-27235 event with u/method/payload tags.
// Returns the event struct (for JSON marshal → base64).
func buildNIP98Event(sec nostr.SecretKey, url, httpMethod, payloadHashHex string) nostr.Event {
	evt := nostr.Event{
		Kind:      27235,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Content:   "",
		Tags: nostr.Tags{
			{"u", url},
			{"method", httpMethod},
			{"payload", payloadHashHex},
		},
	}
	evt.Sign(sec)
	return evt
}
```

Note: If the precise `nostr.Event.Sign` API in the edufeed fork differs, adapt — check `/home/laoc/coding/edufeed/nostrlib` during implementation.

- [ ] **Step 4: Run tests**

```bash
go test -run TestRelayClient -v
```

- [ ] **Step 5: Integration test against a real relay**

Start amb-relay in docker (with the indexer's pubkey in `ADMIN_PUBKEYS`), publish a kind-30142 event, then call `RelayClient.SetContent` and assert the `content` field appears on the Typesense event document.

- [ ] **Step 6: Commit**

```bash
git add relay_client.go relay_client_test.go
git commit -m "feat: relay client with NIP-98 signed setcontent calls"
```

---

## Task 14: Nostr source — subscription + cursor resume

**Files:**
- Create: `nostr_source.go`, `nostr_source_test.go`

- [ ] **Step 1: Define the source**

```go
type NostrSource struct {
	RelayURL string
	Cursor   *CursorStore
	Pool     *nostr.SimplePool
	Kinds    []nostr.Kind // [30142]
}

// Run subscribes to relay with since=cursor, pushes events onto out channel,
// blocks until ctx canceled. Does NOT advance the cursor — workers do that
// after successful processing.
func (s *NostrSource) Run(ctx context.Context, out chan<- nostr.Event) error
```

- [ ] **Step 2: Write tests**

Start a local khatru relay in-process (see `fiatjaf.com/nostr/khatru` test helpers) or via docker. Publish three events at `t=1, 2, 3`. Run source with `cursor=2` → receive events at `t=2, 3` (since is inclusive in the edufeed fork — verify and adjust `since=cursor+1` if needed). On ctx-cancel, `Run` returns nil.

- [ ] **Step 3: Implement**

Use `nostr.SimplePool.SubscribeMany` (or the fork's equivalent). Filter: `{Kinds: s.Kinds, Since: <cursor_or_zero>}`.

- [ ] **Step 4: Commit**

```bash
git add nostr_source.go nostr_source_test.go
git commit -m "feat: nostr source with cursor resume"
```

---

## Task 15: Worker pipeline + replacement semantics + retries

**Files:**
- Create: `worker.go`, `worker_test.go`

- [ ] **Step 1: Define the worker**

```go
type Worker struct {
	Cfg        Config
	ChunkCfg   ChunkConfig // passed to Chunk(cfg, doc) at call site
	Fetcher    *Fetcher
	Embedder   *Embedder
	TS         *TSClient
	Relay      *RelayClient
	Hashes     *EventHashStore
	DeadLetter *DeadLetterStore
	Cursor     *CursorStore
}

// Process runs the pipeline for one event. Idempotent on re-delivery.
func (w *Worker) Process(ctx context.Context, ev nostr.Event) error
```

Steps inside `Process`:
1. Extract source URL: first `url` tag on the event, or `nostr:` scheme if the event references kind-30023 via an `e` tag — follow spec § "Ingest pipeline".
2. Compute `hash := sha256(sourceURL)`. If `Hashes.Get(ev.ID) == hash`, return nil (no-op skip).
3. Check license: find `license` tag on the event, `permissive := LicensePermissive(Cfg.LicenseAllowlist, license)`.
4. Delete prior chunks for this event coord: `TS.FilterDelete(Cfg.TSChunksColl, "event_coord:="+coord(ev))`.
5. Fetch: `doc, err := Fetcher.Fetch(ctx, sourceURL)`. On transient error, retry 3× with exponential backoff (1s, 4s, 16s); on permanent error or after retries, dead-letter + advance cursor + return nil.
6. Determine `status`:
   - `doc.Truncated` → `"truncated"`
   - fetch returned `ErrUnsupportedType` → `"unsupported"` + dead-letter
   - `!permissive` → `"license_denied"` (still embed, but don't persist text)
   - otherwise → `"fetched"`
7. Chunk: `chunks := Chunker(doc)`.
8. Embed: `embeddings := Embedder.Embed(ctx, chunkTexts(chunks))`.
9. Build `ChunkDoc`s (text empty when `!permissive`) and `TS.Upsert(Cfg.TSChunksColl, docs)`.
10. Callback relay: `Relay.SetContent(ctx, ev.ID, textOrEmpty(doc, permissive), now, status, sourceURL)`.
11. Record hash: `Hashes.Put(ev.ID, hash)`.
12. Advance cursor: `Cursor.Set(ev.CreatedAt.Unix())`.

Retry classification:
- Transient: `context.DeadlineExceeded`, `net.Error.Timeout() || .Temporary()`, HTTP 5xx, 429, DNS resolution failure, Tika 5xx.
- Permanent: HTTP 4xx (excl. 429), `ErrUnsupportedType`, `ErrTooLarge`, `ErrBlocked`, JSON/extraction failures on small responses.

- [ ] **Step 2: Write tests**

All externals mocked. Cases:
1. Happy path HTML: one event → cursor advanced, chunks upserted, setcontent called with text + status=fetched.
2. Same event re-delivered → no-op skip (only `Hashes.Get` and `Cursor.Set` are called; no fetch).
3. Replacement: publish a newer event with same `(pubkey, d)` → `FilterDelete` called with the event coord, then re-process.
4. Transient 503 → three retries → eventual success.
5. Permanent 404 → straight to dead-letter, no retries.
6. License non-permissive → upsert has `Text:""`, `LicensePermissive:false`; setcontent called with empty text and status=`license_denied`.

- [ ] **Step 3: Implement**

Straightforward sequencing with a small `retry(ctx, attempts, fn, isTransient)` helper.

- [ ] **Step 4: Commit**

```bash
git add worker.go worker_test.go
git commit -m "feat: worker pipeline with retries, replacement, dead letter"
```

---

## Task 16: Main wiring + graceful shutdown

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Rewrite `main.go`**

Startup order:
1. `LoadConfig()`.
2. `OpenBolt(cfg.DBPath)`; defer close. Construct `CursorStore`, `EventHashStore`, `DeadLetterStore`.
3. `NewSSRFGuard(...)`, `NewPolitenessLimiter(...)`.
4. Construct `TikaClient`, `nostrFetcher`, `Fetcher`.
5. `TSClient.EnsureCollection(ctx, ChunksSchema)`.
6. `Embedder`, `RelayClient`.
7. `NostrSource`.
8. Event channel (buffered at `Workers*4`).
9. Start `Workers` goroutines, each looping on the channel calling `Worker.Process`.
10. Install signal handler for SIGINT/SIGTERM: cancel context → source Run returns → close channel → workers drain → `bbolt.Close`.
11. Run source in the main goroutine; on exit, wait for workers via `sync.WaitGroup`.

- [ ] **Step 2: Smoke test**

`go build . && ./amb-indexer` (with all env vars set + relay + TS + tika running locally). Expect log lines: config loaded, collection ensured, nostr subscribed, N workers started.

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: top-level wiring and graceful shutdown"
```

---

## Task 17: Docker-compose integration with amb-relay

**Files:**
- Modify: `/home/laoc/coding/edufeed/amb-relay/docker-compose.yml`
- Create: `amb-indexer/.env.indexer.example`

- [ ] **Step 1: Extend amb-relay's `docker-compose.yml`**

Add two services to the existing file (do not touch `typesense` or `amb-relay` except env — add the indexer's pubkey to `amb-relay`'s `ADMIN_PUBKEYS`):

```yaml
  tika:
    image: apache/tika:latest-full
    restart: unless-stopped
    # No port exposure to host; reachable only on the internal network.

  amb-indexer:
    build: ../amb-indexer
    depends_on: [amb-relay, typesense, tika]
    env_file: ../amb-indexer/.env.indexer
    restart: unless-stopped
    volumes:
      - indexer_data:/data

volumes:
  indexer_data:
```

If amb-relay's compose doesn't yet declare a `volumes:` block, add one alongside its existing `data:` (or equivalent) volume.

- [ ] **Step 2: Document in amb-indexer's README**

Three lines: "Build & run" (`docker compose up --build`), "Where the data lives", "How to check progress" (read indexer logs, query TS directly).

- [ ] **Step 3: Commit**

On amb-indexer:
```bash
git add .env.indexer.example README.md
git commit -m "docs: docker compose integration"
```

On amb-relay (separate worktree as usual):
```bash
git add docker-compose.yml
git commit -m "feat: wire amb-indexer and tika into compose stack"
```

---

## Task 18: End-to-end test script

**Files:**
- Create: `amb-indexer/e2e_test.sh`

- [ ] **Step 1: Write the test script**

Responsibilities:
1. `docker compose up -d typesense amb-relay tika`.
2. Build + run the indexer (`go run .` or `docker compose up -d amb-indexer`).
3. Publish a kind-30142 event whose `url` tag points to a locally-served HTML page (spun up by the script via `python -m http.server` or a tiny Go httptest binary; use a fixture with an h2 structure and a detectable term).
4. Publish a kind-30142 event whose `url` tag points to a PDF served locally (fixture PDF with detectable term).
5. Publish a kind-30142 event whose only reference is a kind-30023 `naddr` (publish a fixture kind-30023 first).
6. Wait up to 60s for the indexer pipeline.
7. Assert:
   a. `content` field on the relay's TS event docs is non-empty for each event (via `curl` to TS API).
   b. Searchable: `q=<known_term>&query_by=content` returns the right event.
   c. Chunks are present in `amb_chunks_30142`: `q=<known_term>&query_by=text` returns chunks with correct `event_coord`.
   d. `license_permissive` flag matches the event's license.
8. Teardown.

Use the same PASS/FAIL/counter pattern as amb-relay's `test_e2e.sh` for consistency.

- [ ] **Step 2: Run it**

```bash
./e2e_test.sh
```
Expected: PASS count equals assertion count, exit 0.

- [ ] **Step 3: Commit**

```bash
git add e2e_test.sh testdata/
git commit -m "test: e2e integration covering HTML, PDF, and kind-30023 paths"
```

---

## Task 19: Final review and cleanup

- [ ] **Step 1: Re-read the spec "Phase 1" bullet list**

Verify each in-scope item is covered:
- service skeleton ✓ (Task 1)
- nostr subscription with cursor ✓ (Task 14)
- HTML / PDF / kind-30023 fetchers ✓ (Tasks 6, 7, 8)
- license gate ✓ (Task 5)
- structural + recursive chunker ✓ (Tasks 9, 10)
- embedding batching ✓ (Task 11)
- NIP-86 setcontent callback ✓ (Task 13)
- chunk collection schema + writes ✓ (Task 12, 15)
- docker compose wiring ✓ (Task 17)

Out of scope (deferred to Plan 2b) — confirm **none** of these landed:
- `/search_chunks` HTTP
- MCP server
- `/admin/*` endpoints
- `/metrics`, `/health`, `/ready`
- Refetch-signal consumer

- [ ] **Step 2: Run full test suite**

```bash
go test ./...
./e2e_test.sh
```

- [ ] **Step 3: Static checks**

```bash
go vet ./...
gofmt -l .  # should be empty
```

- [ ] **Step 4: Security self-review**

- SSRF guard covers all configured deny lists — verify manually with one targeted attempt (URL pointing at `http://127.0.0.1/…` → blocked with `ErrBlocked`).
- NIP-98 auth event is freshly signed per request (no replay).
- `INDEXER_NSEC` is never logged.
- Bearer tokens (EMBED_TOKEN) never logged.

- [ ] **Step 5: Commit any fix-ups**

```bash
git commit -am "chore: final review fixups"
```

---

## Post-plan: execution handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-22-amb-indexer-ingest-pipeline.md`. Two execution options:

1. **Subagent-Driven (recommended)** — fresh subagent per task + two-stage review per task.
2. **Inline Execution** — batch execution with checkpoints.

After Plan 2a ships and the indexer is consuming events in production, Plan 2b will cover: `/search_chunks` HTTP endpoint, MCP server, admin dead-letter + refetch endpoints, Prometheus `/metrics`, `/health`/`/ready`, and takedown-delete helpers.
