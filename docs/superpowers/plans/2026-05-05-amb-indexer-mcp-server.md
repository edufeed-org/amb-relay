# amb-indexer MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the existing `POST /search_chunks` engine as an MCP server so LLM tools (Claude Code, Claude Desktop, Cursor, etc.) can register the indexer as a context source in one config line. Closes the last Phase 1 gap from `docs/superpowers/specs/2026-04-21-amb-resource-fulltext-indexing-design.md` (line 407: "`/search_chunks` HTTP + MCP server").

**Architecture:** A new standalone Go binary `amb-indexer/cmd/mcp` that speaks MCP over **stdio** (the only transport LLM desktop clients use today) and proxies tool calls to the indexer's existing `POST /search_chunks` HTTP endpoint over a configured bearer token. No direct DB/Typesense/embedder access — the MCP server is a thin client that depends only on the indexer's public HTTP surface. This keeps it independently deployable (LLM tools spawn it as a subprocess) and decoupled from the indexer's process lifecycle.

**Tech Stack:**
- `github.com/modelcontextprotocol/go-sdk@v1.2.0` (official MCP Go SDK)
- Go 1.25
- Standard library `net/http` for the indexer client

---

## Background context for the implementer

The amb-indexer is a Nostr-driven RAG pipeline. It ingests kind-30142 AMB events, fetches the referenced learning resources, chunks + embeds them, and exposes a search HTTP API. Read these in order before starting:

1. `amb-indexer/CLAUDE.md` — project overview
2. `amb-indexer/README.md` — operational view, especially the **HTTP API → Search** section that documents `POST /search_chunks`
3. `amb-indexer/search.go` — the `QueryEngine` implementation. The MCP server does NOT call this directly; it calls the HTTP endpoint that wraps it
4. `amb-indexer/cmd/mirror-prod/main.go` — example of an existing `cmd/` subbinary in this repo (file layout pattern to mirror)

The existing `/search_chunks` request and response shapes (which the MCP tool will mirror) are in `amb-indexer/search.go`:

```go
type SearchRequest struct {
    Q      string         `json:"q"`
    K      int            `json:"k"`
    Filter map[string]any `json:"filter,omitempty"`
}

type SearchHit struct {
    ChunkID     string            `json:"chunk_id"`
    EventID     string            `json:"event_id"`
    EventCoord  string            `json:"event_coord"`
    ChunkIdx    int               `json:"chunk_idx"`
    Text        string            `json:"text,omitempty"`
    Snippet     string            `json:"snippet"`
    Heading     string            `json:"heading,omitempty"`
    SectionPath []string          `json:"section_path,omitempty"`
    Page        int               `json:"page,omitempty"`
    SourceURL   string            `json:"source_url"`
    Score       float64           `json:"score"`
    AMB         *AMBCacheMetadata `json:"amb,omitempty"`
}

type SearchResponse struct {
    Hits  []SearchHit `json:"hits"`
    Total int         `json:"total"`
}
```

The existing filter allowlist (from `buildFilterBy` in `search.go:200`):
- `about_id` — `[]string`
- `learning_resource_type` — `[]string`
- `license` — `[]string`
- `license_permissive` — `bool`

Only these four keys are honored; anything else is silently dropped server-side. The MCP tool's `filter` schema must restrict callers to exactly this set — this both protects the server and gives the LLM a precise schema to reason about.

## File Structure

| Path | Purpose |
|------|---------|
| `cmd/mcp/main.go` | Binary entry point: parses flags, builds the client, registers the tool, runs stdio transport |
| `cmd/mcp/client.go` | Thin HTTP client for `POST /search_chunks` with bearer auth |
| `cmd/mcp/client_test.go` | Unit tests for the client against `httptest.Server` |
| `cmd/mcp/tool.go` | The `search_educational_chunks` tool: input/output structs and handler |
| `cmd/mcp/tool_test.go` | Unit tests for the handler with a fake client |
| `cmd/mcp/main_test.go` | Integration test: spawn the binary, drive it as an MCP client over stdio, assert tool round-trip |
| `cmd/mcp/README.md` | Stub pointer back to the section in `amb-indexer/README.md` |

Files modified:
- `go.mod` / `go.sum` — add `github.com/modelcontextprotocol/go-sdk` dependency
- `amb-indexer/README.md` — new section under HTTP API documenting the MCP server and registration recipe
- `amb-indexer/CLAUDE.md` — update the "Plan 2c is deferred" note (line 17) to reflect that it has now landed

## Tasks

### Task 1: Add MCP SDK dependency and `cmd/mcp` skeleton

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `cmd/mcp/main.go` (placeholder so build works)

- [ ] **Step 1: Add the dependency**

Run from the `amb-indexer` repo root:

```bash
go get github.com/modelcontextprotocol/go-sdk@v1.2.0
```

This updates `go.mod` and `go.sum`.

- [ ] **Step 2: Create the placeholder main**

Create `cmd/mcp/main.go`:

```go
// Command mcp is an MCP (Model Context Protocol) server that exposes the
// amb-indexer's /search_chunks engine as a tool consumable by LLM clients
// (Claude Code, Claude Desktop, Cursor, etc.). It speaks MCP over stdio and
// proxies tool calls to the indexer's HTTP API.
package main

import "log"

func main() {
	log.Fatal("not implemented yet")
}
```

- [ ] **Step 3: Verify the build**

Run: `go build ./cmd/mcp`
Expected: builds cleanly, produces a binary in `./mcp` (delete or `git clean` after).

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum cmd/mcp/main.go
git commit -m "chore: scaffold cmd/mcp with MCP SDK dependency"
```

---

### Task 2: HTTP client for `/search_chunks`

The MCP server's only outbound dependency is the indexer's HTTP API. Build a small typed client first, with tests, before writing any MCP code.

**Files:**
- Create: `cmd/mcp/client.go`
- Create: `cmd/mcp/client_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/mcp/client_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_Search_OK(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":2,"hits":[
			{"chunk_id":"a:0","event_id":"a","event_coord":"30142:pk:dt","chunk_idx":0,
			 "snippet":"hello","source_url":"https://example.org","score":0.9}
		]}`))
	}))
	defer srv.Close()

	c := &Client{
		BaseURL: srv.URL,
		Token:   "tok",
		HTTP:    srv.Client(),
	}
	req := SearchRequest{Q: "hello", K: 3, Filter: map[string]any{"license_permissive": true}}
	resp, err := c.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Total != 2 || len(resp.Hits) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if gotMethod != "POST" || gotPath != "/search_chunks" {
		t.Fatalf("wrong method/path: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("wrong auth: %q", gotAuth)
	}
	var sent SearchRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if sent.Q != "hello" || sent.K != 3 {
		t.Fatalf("body not forwarded: %+v", sent)
	}
}

func TestClient_Search_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "wrong", HTTP: srv.Client()}
	_, err := c.Search(context.Background(), SearchRequest{Q: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 in error, got %v", err)
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./cmd/mcp -run TestClient_Search -v`
Expected: FAIL with "undefined: Client" / "undefined: SearchRequest".

- [ ] **Step 3: Implement the client**

Create `cmd/mcp/client.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SearchRequest mirrors the indexer's /search_chunks request body. Filter
// is left as map[string]any to match the HTTP contract exactly; the MCP
// tool layer (tool.go) is the typed boundary that constrains callers.
type SearchRequest struct {
	Q      string         `json:"q"`
	K      int            `json:"k,omitempty"`
	Filter map[string]any `json:"filter,omitempty"`
}

// SearchHit mirrors the indexer's /search_chunks hit shape.
type SearchHit struct {
	ChunkID     string         `json:"chunk_id"`
	EventID     string         `json:"event_id"`
	EventCoord  string         `json:"event_coord"`
	ChunkIdx    int            `json:"chunk_idx"`
	Text        string         `json:"text,omitempty"`
	Snippet     string         `json:"snippet"`
	Heading     string         `json:"heading,omitempty"`
	SectionPath []string       `json:"section_path,omitempty"`
	Page        int            `json:"page,omitempty"`
	SourceURL   string         `json:"source_url"`
	Score       float64        `json:"score"`
	AMB         map[string]any `json:"amb,omitempty"`
}

// SearchResponse mirrors the indexer's /search_chunks envelope.
type SearchResponse struct {
	Hits  []SearchHit `json:"hits"`
	Total int         `json:"total"`
}

// Client calls the amb-indexer's HTTP API.
type Client struct {
	BaseURL string       // e.g. "http://localhost:8080"
	Token   string       // INDEXER_API_TOKEN
	HTTP    *http.Client // nil → http.DefaultClient
}

// Search posts to /search_chunks and decodes the JSON envelope.
func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("marshal: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/search_chunks"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return SearchResponse{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Read up to 512 bytes of error body for context.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SearchResponse{}, fmt.Errorf("indexer returned %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var out SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SearchResponse{}, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `go test ./cmd/mcp -run TestClient_Search -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
git add cmd/mcp/client.go cmd/mcp/client_test.go
git commit -m "feat(mcp): typed HTTP client for /search_chunks"
```

---

### Task 3: The `search_educational_chunks` tool

This is the core MCP surface: a typed tool definition the LLM client introspects, plus a handler that wires inputs through the client.

**Files:**
- Create: `cmd/mcp/tool.go`
- Create: `cmd/mcp/tool_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/mcp/tool_test.go`:

```go
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeClient implements searcher for tool tests.
type fakeClient struct {
	gotReq SearchRequest
	resp   SearchResponse
	err    error
}

func (f *fakeClient) Search(_ context.Context, r SearchRequest) (SearchResponse, error) {
	f.gotReq = r
	return f.resp, f.err
}

func TestSearchTool_PassesThroughFilters(t *testing.T) {
	fc := &fakeClient{resp: SearchResponse{Total: 1, Hits: []SearchHit{{ChunkID: "x:0", Snippet: "ok"}}}}
	h := searchToolHandler(fc)

	in := SearchToolInput{
		Query: "photosynthesis",
		K:     5,
		Filter: &SearchToolFilter{
			LicensePermissive:    boolPtr(true),
			LearningResourceType: []string{"https://w3id.org/kim/hcrt/worksheet"},
		},
	}
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, in)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out.Total != 1 || len(out.Hits) != 1 {
		t.Fatalf("output not forwarded: %+v", out)
	}

	if fc.gotReq.Q != "photosynthesis" || fc.gotReq.K != 5 {
		t.Fatalf("q/k not forwarded: %+v", fc.gotReq)
	}
	if v, ok := fc.gotReq.Filter["license_permissive"].(bool); !ok || !v {
		t.Fatalf("license_permissive not forwarded: %+v", fc.gotReq.Filter)
	}
	if v, ok := fc.gotReq.Filter["learning_resource_type"].([]string); !ok || len(v) != 1 {
		t.Fatalf("learning_resource_type not forwarded: %+v", fc.gotReq.Filter)
	}
}

func TestSearchTool_OmitsNilFilter(t *testing.T) {
	fc := &fakeClient{resp: SearchResponse{}}
	h := searchToolHandler(fc)

	_, _, err := h(context.Background(), &mcp.CallToolRequest{}, SearchToolInput{Query: "x"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if fc.gotReq.Filter != nil {
		t.Fatalf("expected nil filter when input has none, got %+v", fc.gotReq.Filter)
	}
}

func TestSearchTool_PropagatesClientError(t *testing.T) {
	fc := &fakeClient{err: errors.New("boom")}
	h := searchToolHandler(fc)

	_, _, err := h(context.Background(), &mcp.CallToolRequest{}, SearchToolInput{Query: "x"})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected error, got %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }
```

- [ ] **Step 2: Run the test, verify it fails**

Run: `go test ./cmd/mcp -run TestSearchTool -v`
Expected: FAIL with "undefined: searchToolHandler" / "undefined: SearchToolInput".

- [ ] **Step 3: Implement the tool**

Create `cmd/mcp/tool.go`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// searcher is the subset of *Client the tool depends on. Declared here so
// tests can substitute a fake.
type searcher interface {
	Search(ctx context.Context, req SearchRequest) (SearchResponse, error)
}

// SearchToolFilter is the typed filter exposed to MCP callers. It mirrors
// the four-field allowlist enforced by the indexer's buildFilterBy
// (search.go). Pointers are used for optional booleans so an unset field
// is distinguishable from `false`.
type SearchToolFilter struct {
	AboutID              []string `json:"about_id,omitempty" jsonschema:"OER concept IDs (e.g. https://w3id.org/kim/schulfaecher/s1017)"`
	LearningResourceType []string `json:"learning_resource_type,omitempty" jsonschema:"hcrt or comparable resource-type URIs"`
	License              []string `json:"license,omitempty" jsonschema:"raw license identifiers (e.g. CC-BY-4.0)"`
	LicensePermissive    *bool    `json:"license_permissive,omitempty" jsonschema:"if true, only return chunks whose license is on the operator's permissive allow-list"`
}

// SearchToolInput is the LLM-facing tool argument schema for
// search_educational_chunks. The struct tags drive the JSON Schema the
// MCP client introspects.
type SearchToolInput struct {
	Query  string            `json:"query" jsonschema:"natural-language query against the chunk corpus"`
	K      int               `json:"k,omitempty" jsonschema:"max hits to return (default 10, capped at 100 server-side)"`
	Filter *SearchToolFilter `json:"filter,omitempty" jsonschema:"optional metadata filters"`
}

// SearchToolOutput is the structured response the tool returns. We
// re-export SearchResponse rather than redefining it so the schema stays
// in lockstep with the HTTP contract.
type SearchToolOutput = SearchResponse

// searchToolHandler builds the MCP tool handler bound to a searcher. The
// returned function is the signature mcp.AddTool expects (typed In/Out).
func searchToolHandler(c searcher) func(context.Context, *mcp.CallToolRequest, SearchToolInput) (*mcp.CallToolResult, SearchToolOutput, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchToolInput) (*mcp.CallToolResult, SearchToolOutput, error) {
		req := SearchRequest{Q: in.Query, K: in.K}
		if in.Filter != nil {
			f := map[string]any{}
			if len(in.Filter.AboutID) > 0 {
				f["about_id"] = in.Filter.AboutID
			}
			if len(in.Filter.LearningResourceType) > 0 {
				f["learning_resource_type"] = in.Filter.LearningResourceType
			}
			if len(in.Filter.License) > 0 {
				f["license"] = in.Filter.License
			}
			if in.Filter.LicensePermissive != nil {
				f["license_permissive"] = *in.Filter.LicensePermissive
			}
			if len(f) > 0 {
				req.Filter = f
			}
		}

		resp, err := c.Search(ctx, req)
		if err != nil {
			return nil, SearchToolOutput{}, fmt.Errorf("search: %w", err)
		}
		return nil, resp, nil
	}
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `go test ./cmd/mcp -run TestSearchTool -v`
Expected: PASS (all three subtests).

- [ ] **Step 5: Commit**

```bash
git add cmd/mcp/tool.go cmd/mcp/tool_test.go
git commit -m "feat(mcp): search_educational_chunks tool definition"
```

---

### Task 4: Wire the stdio main

The binary now has a client and a tool. Wire them into an MCP server reading from stdin and writing to stdout.

**Files:**
- Modify: `cmd/mcp/main.go`

- [ ] **Step 1: Replace the placeholder main**

Overwrite `cmd/mcp/main.go`:

```go
// Command mcp is an MCP (Model Context Protocol) server that exposes the
// amb-indexer's /search_chunks engine as a tool consumable by LLM clients
// (Claude Code, Claude Desktop, Cursor, etc.). It speaks MCP over stdio
// and proxies tool calls to the indexer's HTTP API.
//
// Configuration is via flags (preferred for stdio launches by LLM clients
// that pass args explicitly) or env vars (fallback for shell-driven runs).
//
//	-indexer-url     INDEXER_URL          base URL, default http://localhost:8080
//	-indexer-token   INDEXER_API_TOKEN    bearer token, no default (required)
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "amb-indexer"
	serverVersion = "0.1.0"
	toolName      = "search_educational_chunks"
	toolDesc      = "Search the AMB educational-chunk corpus by natural-language query. " +
		"Returns ranked chunks with source URL, surrounding heading, and (where licensing permits) full chunk text. " +
		"Use this tool when the user asks about educational material, OER, learning resources, or content from " +
		"specific publishers or subject areas indexed by amb-indexer."
)

func main() {
	log.SetFlags(0) // stderr only; stdout is reserved for MCP framing
	log.SetOutput(os.Stderr)

	var (
		urlFlag   = flag.String("indexer-url", envOr("INDEXER_URL", "http://localhost:8080"), "amb-indexer HTTP base URL")
		tokenFlag = flag.String("indexer-token", os.Getenv("INDEXER_API_TOKEN"), "amb-indexer bearer token (INDEXER_API_TOKEN)")
	)
	flag.Parse()

	token := strings.TrimSpace(*tokenFlag)
	if token == "" {
		log.Fatal("indexer-token is required (set -indexer-token or INDEXER_API_TOKEN)")
	}

	client := &Client{
		BaseURL: *urlFlag,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)
	mcp.AddTool(server,
		&mcp.Tool{Name: toolName, Description: toolDesc},
		searchToolHandler(client))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

- [ ] **Step 2: Verify the build**

Run: `go build ./cmd/mcp`
Expected: builds cleanly. Delete the produced binary or `git clean -f mcp` after.

- [ ] **Step 3: Smoke-test the missing-token path**

Run: `./mcp 2>&1 | head -1; echo exit=$?`

(You'll need to build first: `go build ./cmd/mcp`.)

Expected output: line ending in "indexer-token is required..." and `exit=1`. This proves the binary boots, parses flags, and rejects misconfiguration before opening stdio.

- [ ] **Step 4: Commit**

```bash
git add cmd/mcp/main.go
git commit -m "feat(mcp): wire stdio main with flag-driven config"
```

---

### Task 5: End-to-end integration test

Spawn the binary, drive it as an MCP client over stdio against a fake `/search_chunks`. This verifies framing, tool registration, and the full request → response round-trip.

**Files:**
- Create: `cmd/mcp/main_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/mcp/main_test.go`:

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEndToEnd_StdioToolCall builds the binary, runs it with a fake
// indexer URL, and drives it as a real MCP client over stdio. This is
// the only test that exercises framing + handler wiring together.
func TestEndToEnd_StdioToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search_chunks" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer e2e-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1,"hits":[
			{"chunk_id":"e:0","event_id":"e","event_coord":"30142:pk:dt","chunk_idx":0,
			 "snippet":"hi","source_url":"https://example.org","score":0.5}
		]}`))
	}))
	defer srv.Close()

	binPath := buildBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	transport := &mcp.CommandTransport{
		Command: exec.Command(binPath, "-indexer-url", srv.URL, "-indexer-token", "e2e-tok"),
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	// 1. ListTools should advertise our one tool.
	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "search_educational_chunks" {
		t.Fatalf("unexpected tools: %+v", tools.Tools)
	}

	// 2. CallTool should round-trip through the fake server.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "search_educational_chunks",
		Arguments: map[string]any{
			"query": "hi",
			"k":     1,
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported error: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatalf("expected structured content, got nil")
	}
	// StructuredContent is map[string]any after JSON round-trip.
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want map", res.StructuredContent)
	}
	if total, _ := sc["total"].(float64); total != 1 {
		t.Fatalf("unexpected total: %v", sc["total"])
	}
}

// buildBinary compiles cmd/mcp into a temp directory and returns the path.
// Built once per test run (t.TempDir scopes lifetime).
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "mcp")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = newTestWriter(t)
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return out
}

// testWriter forwards writes to t.Log for visibility on test failures.
type testWriter struct{ t *testing.T }

func newTestWriter(t *testing.T) *testWriter { return &testWriter{t: t} }
func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./cmd/mcp -run TestEndToEnd -v`
Expected: PASS. The test builds the binary, spawns it, and drives a tool call through stdio.

(If the binary build inside the test is too slow on CI, this is acceptable for now — the indexer's `cmd/mirror-prod` does not have an integration test of this form. Add a `-short` skip in a follow-up if needed.)

- [ ] **Step 3: Run the whole suite to confirm nothing else broke**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/mcp/main_test.go
git commit -m "test(mcp): end-to-end stdio round-trip against fake indexer"
```

---

### Task 6: Documentation — add MCP section to indexer README

**Files:**
- Modify: `README.md` (in `amb-indexer`)

- [ ] **Step 1: Add the new section**

Open `amb-indexer/README.md`. Locate the line `### Probes` near the end of the HTTP API section. **Immediately after** the closing of the Probes subsection (the paragraph ending with `(default every 30s).`), and **before** `### Operator notes`, insert:

```markdown
### MCP server (LLM context source)

`cmd/mcp` is a standalone binary that exposes the indexer's
`/search_chunks` engine as an [MCP](https://modelcontextprotocol.io)
tool named `search_educational_chunks`. LLM clients (Claude Code,
Claude Desktop, Cursor, etc.) can register the indexer as a context
source in one config line.

Build:

```bash
go build -o ./bin/amb-indexer-mcp ./cmd/mcp
```

The binary speaks MCP over stdio. It needs:

- `-indexer-url` (or `INDEXER_URL`) — defaults to `http://localhost:8080`
- `-indexer-token` (or `INDEXER_API_TOKEN`) — same value the operator
  configured on the indexer for `/search_chunks`. Required.

#### Register with Claude Code

In your Claude Code config (`~/.claude/mcp.json` or per-project):

```json
{
  "mcpServers": {
    "amb-indexer": {
      "command": "/abs/path/to/amb-indexer-mcp",
      "args": ["-indexer-url", "http://localhost:8080"],
      "env": {
        "INDEXER_API_TOKEN": "your-token-here"
      }
    }
  }
}
```

Restart Claude Code. The `search_educational_chunks` tool will appear
in the tool list. Ask "find me OER worksheets about photosynthesis"
and the model will call the tool with the appropriate filter.

#### Tool schema

```jsonc
search_educational_chunks({
  "query": "natural-language query",       // required
  "k": 10,                                 // optional, server-capped at 100
  "filter": {                              // optional, all fields optional
    "about_id":              ["https://w3id.org/kim/schulfaecher/s1017"],
    "learning_resource_type":["https://w3id.org/kim/hcrt/worksheet"],
    "license":               ["CC-BY-4.0"],
    "license_permissive":    true
  }
})
// → {"total": N, "hits": [ ... same shape as POST /search_chunks ... ]}
```

The tool's response schema is identical to the HTTP endpoint's
`SearchResponse`. Non-permissive chunks have `text` elided but retain
`snippet` so the LLM can still ground its answer on a fair-use fragment.
```

- [ ] **Step 2: Verify rendering**

Run a markdown linter or just inspect: `cat README.md | head -240 | tail -80`. Confirm the new heading sits between Probes and Operator notes, with the expected indentation.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document MCP server and Claude Code registration"
```

---

### Task 7: Update CLAUDE.md to mark Plan 2c as landed

**Files:**
- Modify: `CLAUDE.md` (in `amb-indexer`)

- [ ] **Step 1: Update the deferred-plan note**

In `amb-indexer/CLAUDE.md`, find the line (around line 17) that reads:

```
The MCP server wrapper is Plan 2c.
```

Replace it with:

```
The MCP server wrapper (Plan 2c) lives in `cmd/mcp` — see README.md § "MCP server (LLM context source)".
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: point CLAUDE.md at MCP server now that Plan 2c has landed"
```

---

## Verification

After all tasks, run:

```bash
go build ./...
go test ./...
go vet ./...
```

Then a real round-trip against the running indexer:

```bash
# Assume amb-indexer is running on :8080 with INDEXER_API_TOKEN=<TOK>
go build -o /tmp/amb-mcp ./cmd/mcp

# Quick sanity check using the inspector or a one-shot client:
go run github.com/modelcontextprotocol/go-sdk/cmd/mcp-inspector@latest \
  /tmp/amb-mcp -indexer-url http://localhost:8080 -indexer-token "$TOK"
```

If the inspector lists `search_educational_chunks` and a sample query
returns hits, the implementation is end-to-end working.

## Out of scope (defer to a follow-up plan)

- **Streamable HTTP transport.** Stdio is sufficient for all desktop LLM
  tools today. HTTP transport is useful for hosted/multi-tenant scenarios
  (a single MCP server serving many clients over the network) and adds
  bearer-token middleware via `auth.RequireBearerToken`. Add when
  there's a concrete use case.
- **Resource exposure** (e.g. `nostr://event/<id>` resolving to the AMB
  metadata). Tools-only is the simplest viable surface; resources can
  follow once we know what LLMs actually want to fetch.
- **Prompt templates.** No clear demand yet.
- **Per-tenant token rotation / OAuth.** Single bearer token matches the
  indexer's existing model.

## Self-review notes

**Spec coverage:** spec line 200–219 ("MCP: an MCP server wrapping the same engine, exposing a tool `search_educational_chunks(query, k, filters)` with equivalent schema") → covered by tasks 3–5. Filter allowlist matches `buildFilterBy` exactly.

**Type consistency:** `SearchRequest`/`SearchHit`/`SearchResponse` in `cmd/mcp/client.go` mirror the indexer's `search.go` shapes by JSON tag, not by Go import — `cmd/mcp` is a separate `main` package and cannot reach into the root package without a refactor we're not doing here. The end-to-end test in Task 5 catches drift if the JSON contract changes.

**Placeholder scan:** clean — every step has explicit code or commands.
