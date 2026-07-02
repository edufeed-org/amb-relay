# Relay Content Visualization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only, `VIZ_ENABLED`-gated `/viz` page to the relay that shows a corpus stat strip plus a force-directed graph of communities, authors, and the transferkiosk project hierarchy.

**Architecture:** Approach A — endpoints and an embedded static page live inside the existing Go relay. HTTP handlers register on khatru's `relay.Router()`. Data comes from the relay's Typesense collections via plain REST reads (reusing `TSBackend.Host`/`ApiKey`), aggregated **entities-as-nodes, content-as-weight** and cached in-memory with a short TTL. The frontend is one embedded HTML/JS/CSS page using a vendored canvas force-graph library.

**Tech Stack:** Go (khatru relay, `net/http`, `//go:embed`), Typesense REST API, vanilla JS + `force-graph` (vendored UMD build).

## Global Constraints

- Go module path: `github.com/edufeed-org/amb-relay`. Feature code is top-level `package main` (like `calendar.go`, `transferkiosk.go`); only shared CLI logic lives under `internal/`.
- Typesense is read via plain REST (`GET /collections/<name>/documents/search`), reusing the existing `TSBackend` fields `Host`, `ApiKey`, `CollectionName`. No Typesense SDK.
- All `/viz*` routes are read-only, unauthenticated, and registered ONLY when `VIZ_ENABLED=true`. When off, no routes exist.
- Responses expose only aggregated counts and capped (≤50) lists of events already served publicly over Nostr. Never expose admin/NIP-86/BoltDB internals.
- New env vars: `VIZ_ENABLED` (default `false`), `VIZ_TOP_AUTHORS` (default `40`), `VIZ_CACHE_TTL` (default `60s`).
- Content kinds in scope: AMB `30142`, long-form `30023`, wiki `30818`, calendar `31922/31923`, plus transferkiosk `30143/30144/30145` and community shares. Only enabled collections are queried.
- Tests use `net/http/httptest` fake Typesense servers (mirror `internal/hydrate/run_test.go`) and table-driven Go tests (mirror `transferkiosk_test.go`).
- Run all Go commands from the repo root with the workspace: `go test ./...` and `go build .` (local dev uses the parent `go.work`).

---

## File Structure

- `viz_ts.go` (new) — Typesense read helpers: collection count, facet counts, doc search. Pure HTTP, no relay deps.
- `viz_graph.go` (new) — pure graph aggregator: `GraphInput` → `Graph` (nodes/edges) with top-N author fold and `partOf` tree wiring. No HTTP.
- `viz_stats.go` (new) — pure stats assembler: raw counts/facets → `Stats` JSON struct.
- `viz.go` (new) — HTTP handlers, in-memory TTL cache, and `vizSetup(...)` that fetches from Typesense, calls the pure assemblers, and registers routes on `relay.Router()`.
- `web/viz/index.html`, `web/viz/app.js`, `web/viz/style.css`, `web/viz/force-graph.min.js` (new) — embedded frontend.
- `viz_ts_test.go`, `viz_graph_test.go`, `viz_stats_test.go`, `viz_test.go` (new) — tests.
- `main.go` (modify) — read env vars, call `vizSetup` when enabled, after `landing.Setup(relay)`.
- `.env.example`, `CLAUDE.md` (modify) — document the flag and vars.

---

## Task 1: Typesense read helpers (`viz_ts.go`)

**Files:**
- Create: `viz_ts.go`
- Test: `viz_ts_test.go`

**Interfaces:**
- Consumes: nothing (leaf module).
- Produces:
  - `type FacetValue struct { Value string; Count int }`
  - `type VizDoc map[string]any`
  - `func vizCount(ctx context.Context, host, apiKey, collection string) (int, error)`
  - `func vizFacet(ctx context.Context, host, apiKey, collection, field string, maxValues int) ([]FacetValue, error)`
  - `func vizSearch(ctx context.Context, host, apiKey, collection, filterBy, includeFields string, perPage int) ([]VizDoc, error)`

- [ ] **Step 1: Write the failing test**

```go
// viz_ts_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeTS(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TYPESENSE-API-KEY") != "k" {
			t.Errorf("missing api key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestVizCount(t *testing.T) {
	srv := fakeTS(t, map[string]any{"found": 42, "hits": []any{}})
	defer srv.Close()
	got, err := vizCount(context.Background(), srv.URL, "k", "amb")
	if err != nil {
		t.Fatalf("vizCount: %v", err)
	}
	if got != 42 {
		t.Errorf("count = %d, want 42", got)
	}
}

func TestVizFacet(t *testing.T) {
	body := map[string]any{
		"found": 3,
		"facet_counts": []any{
			map[string]any{
				"field_name": "about",
				"counts": []any{
					map[string]any{"value": "Chemie", "count": 2},
					map[string]any{"value": "Physik", "count": 1},
				},
			},
		},
	}
	srv := fakeTS(t, body)
	defer srv.Close()
	got, err := vizFacet(context.Background(), srv.URL, "k", "amb", "about", 20)
	if err != nil {
		t.Fatalf("vizFacet: %v", err)
	}
	if len(got) != 2 || got[0].Value != "Chemie" || got[0].Count != 2 {
		t.Errorf("facet = %+v, want Chemie=2 first", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestViz(Count|Facet)' -v`
Expected: FAIL — `undefined: vizCount` / `undefined: vizFacet`.

- [ ] **Step 3: Write minimal implementation**

```go
// viz_ts.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// vizHTTPClient pools connections for the read-only viz Typesense calls.
var vizHTTPClient = &http.Client{Timeout: 15 * time.Second}

// FacetValue is one bucket of a Typesense facet_counts result.
type FacetValue struct {
	Value string
	Count int
}

// VizDoc is a decoded Typesense document (arbitrary fields).
type VizDoc map[string]any

// tsSearchURL builds a /documents/search URL with q=* and the given params.
func tsSearchURL(host, collection string, extra url.Values) string {
	q := url.Values{}
	q.Set("q", "*")
	q.Set("query_by", "name")
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return fmt.Sprintf("%s/collections/%s/documents/search?%s", host, collection, q.Encode())
}

func tsGet(ctx context.Context, endpoint, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)
	resp, err := vizHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("typesense status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// vizCount returns the total document count of a collection via found on a
// q=* search with per_page=0.
func vizCount(ctx context.Context, host, apiKey, collection string) (int, error) {
	u := tsSearchURL(host, collection, url.Values{"per_page": {"0"}})
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Found int `json:"found"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("decode count: %w", err)
	}
	return parsed.Found, nil
}

// vizFacet returns the facet buckets for one field, most-frequent first
// (Typesense default ordering).
func vizFacet(ctx context.Context, host, apiKey, collection, field string, maxValues int) ([]FacetValue, error) {
	u := tsSearchURL(host, collection, url.Values{
		"per_page":         {"0"},
		"facet_by":         {field},
		"max_facet_values": {strconv.Itoa(maxValues)},
	})
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		FacetCounts []struct {
			FieldName string `json:"field_name"`
			Counts    []struct {
				Value string `json:"value"`
				Count int    `json:"count"`
			} `json:"counts"`
		} `json:"facet_counts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode facet: %w", err)
	}
	out := []FacetValue{}
	for _, fc := range parsed.FacetCounts {
		if fc.FieldName != field {
			continue
		}
		for _, c := range fc.Counts {
			out = append(out, FacetValue{Value: c.Value, Count: c.Count})
		}
	}
	return out, nil
}

// vizSearch runs a q=* search with an optional filter_by and returns the raw
// documents (capped by perPage). includeFields is a Typesense include_fields
// CSV; empty means all fields.
func vizSearch(ctx context.Context, host, apiKey, collection, filterBy, includeFields string, perPage int) ([]VizDoc, error) {
	extra := url.Values{"per_page": {strconv.Itoa(perPage)}}
	if filterBy != "" {
		extra.Set("filter_by", filterBy)
	}
	if includeFields != "" {
		extra.Set("include_fields", includeFields)
	}
	u := tsSearchURL(host, collection, extra)
	body, err := tsGet(ctx, u, apiKey)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Hits []struct {
			Document VizDoc `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode search: %w", err)
	}
	out := make([]VizDoc, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		out = append(out, h.Document)
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run 'TestViz(Count|Facet)' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add viz_ts.go viz_ts_test.go
git commit -m "feat(viz): typesense read helpers (count/facet/search)"
```

---

## Task 2: Graph aggregator (`viz_graph.go`)

**Files:**
- Create: `viz_graph.go`
- Test: `viz_graph_test.go`

**Interfaces:**
- Consumes: nothing (pure logic).
- Produces:
  - `type Node struct { ID string; Type string; Label string; Weight int }` (JSON tags `id,type,label,weight`)
  - `type Edge struct { Source string; Target string; Weight int; Kind string }` (JSON tags `source,target,weight,kind`)
  - `type Graph struct { Nodes []Node; Edges []Edge }` (JSON tags `nodes,edges`)
  - `type AuthorStat struct { Pubkey string; Label string; Count int }`
  - `type CommunityStat struct { Pubkey string; Label string; Content int }`
  - `type AuthorCommunity struct { Author string; Community string; Weight int }`
  - `type TKItem struct { Coord string; Kind int; Label string; Author string; ParentCoord string; Community string }`
  - `type GraphInput struct { Authors []AuthorStat; Communities []CommunityStat; AuthorCommunities []AuthorCommunity; TK []TKItem; TopAuthors int }`
  - `func buildGraph(in GraphInput) Graph`

- [ ] **Step 1: Write the failing test**

```go
// viz_graph_test.go
package main

import "testing"

func nodeByID(g Graph, id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func hasEdge(g Graph, src, dst, kind string) bool {
	for _, e := range g.Edges {
		if e.Source == src && e.Target == dst && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestBuildGraph_TopAuthorFold(t *testing.T) {
	in := GraphInput{
		TopAuthors: 2,
		Authors: []AuthorStat{
			{Pubkey: "a1", Label: "A1", Count: 100},
			{Pubkey: "a2", Label: "A2", Count: 50},
			{Pubkey: "a3", Label: "A3", Count: 10},
			{Pubkey: "a4", Label: "A4", Count: 5},
		},
	}
	g := buildGraph(in)
	if _, ok := nodeByID(g, "author:a1"); !ok {
		t.Errorf("top author a1 missing")
	}
	if _, ok := nodeByID(g, "author:a3"); ok {
		t.Errorf("a3 should have been folded into others")
	}
	others, ok := nodeByID(g, "author:others")
	if !ok {
		t.Fatalf("others node missing")
	}
	if others.Weight != 15 {
		t.Errorf("others weight = %d, want 15", others.Weight)
	}
}

func TestBuildGraph_CommunityEdgesAndTree(t *testing.T) {
	in := GraphInput{
		TopAuthors: 10,
		Authors:    []AuthorStat{{Pubkey: "a1", Label: "A1", Count: 10}},
		Communities: []CommunityStat{{Pubkey: "c1", Label: "C1", Content: 5}},
		AuthorCommunities: []AuthorCommunity{{Author: "a1", Community: "c1", Weight: 3}},
		TK: []TKItem{
			{Coord: "30143:p:proj", Kind: 30143, Label: "Proj", Author: "a1", Community: "c1"},
			{Coord: "30144:p:m1", Kind: 30144, Label: "M1", ParentCoord: "30143:p:proj"},
			{Coord: "30145:p:pub", Kind: 30145, Label: "Pub", ParentCoord: "30144:p:m1"},
		},
	}
	g := buildGraph(in)
	if n, ok := nodeByID(g, "community:c1"); !ok || n.Type != "community" || n.Weight != 5 {
		t.Errorf("community node wrong: %+v ok=%v", n, ok)
	}
	if !hasEdge(g, "author:a1", "community:c1", "author_community") {
		t.Errorf("author_community edge missing")
	}
	if n, ok := nodeByID(g, "tk:30143:p:proj"); !ok || n.Type != "project" {
		t.Errorf("project node missing/wrong: %+v", n)
	}
	if _, ok := nodeByID(g, "tk:30144:p:m1"); !ok {
		t.Errorf("measure node missing")
	}
	if !hasEdge(g, "tk:30144:p:m1", "tk:30143:p:proj", "part_of") {
		t.Errorf("measure->project part_of edge missing")
	}
	if !hasEdge(g, "tk:30145:p:pub", "tk:30144:p:m1", "part_of") {
		t.Errorf("pub->measure part_of edge missing")
	}
	if !hasEdge(g, "author:a1", "tk:30143:p:proj", "author_project") {
		t.Errorf("author_project edge missing")
	}
	if !hasEdge(g, "tk:30143:p:proj", "community:c1", "community_project") {
		t.Errorf("community_project edge missing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestBuildGraph -v`
Expected: FAIL — `undefined: GraphInput` / `undefined: buildGraph`.

- [ ] **Step 3: Write minimal implementation**

```go
// viz_graph.go
package main

import "sort"

// Node is one entity in the relationship graph. Weight drives node size.
type Node struct {
	ID     string `json:"id"`
	Type   string `json:"type"` // community|author|project|measure|publication
	Label  string `json:"label"`
	Weight int    `json:"weight"`
}

// Edge connects two nodes. Weight drives line thickness.
type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Weight int    `json:"weight"`
	Kind   string `json:"kind"` // author_community|part_of|author_project|community_project
}

// Graph is the aggregated node/edge payload returned by /viz/graph.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// AuthorStat is one author (publisher pubkey) and how many resources it has.
type AuthorStat struct {
	Pubkey string
	Label  string
	Count  int
}

// CommunityStat is one community and how much content is shared into it.
type CommunityStat struct {
	Pubkey  string
	Label   string
	Content int
}

// AuthorCommunity records that an author contributes content to a community.
type AuthorCommunity struct {
	Author    string
	Community string
	Weight    int
}

// TKItem is one transferkiosk entity (project/measure/publication). ParentCoord
// is the partOf target ("" for a project). Author/Community are only set for
// projects (used to wire author_project / community_project edges).
type TKItem struct {
	Coord       string
	Kind        int
	Label       string
	Author      string
	ParentCoord string
	Community   string
}

// GraphInput bundles the aggregated facts buildGraph turns into a graph.
type GraphInput struct {
	Authors           []AuthorStat
	Communities       []CommunityStat
	AuthorCommunities []AuthorCommunity
	TK                []TKItem
	TopAuthors        int
}

func tkNodeType(kind int) string {
	switch kind {
	case 30143:
		return "project"
	case 30144:
		return "measure"
	case 30145:
		return "publication"
	default:
		return "publication"
	}
}

// buildGraph aggregates entities into nodes+edges. Authors beyond TopAuthors
// (ranked by Count desc) fold into a single author:others node. Content is
// never a node — only weight/thickness.
func buildGraph(in GraphInput) Graph {
	g := Graph{Nodes: []Node{}, Edges: []Edge{}}
	kept := map[string]bool{}

	// Communities — all kept.
	for _, c := range in.Communities {
		g.Nodes = append(g.Nodes, Node{ID: "community:" + c.Pubkey, Type: "community", Label: c.Label, Weight: c.Content})
	}

	// Authors — top-N by count, rest folded into others.
	authors := append([]AuthorStat(nil), in.Authors...)
	sort.SliceStable(authors, func(i, j int) bool { return authors[i].Count > authors[j].Count })
	othersWeight := 0
	for i, a := range authors {
		if in.TopAuthors > 0 && i >= in.TopAuthors {
			othersWeight += a.Count
			continue
		}
		g.Nodes = append(g.Nodes, Node{ID: "author:" + a.Pubkey, Type: "author", Label: a.Label, Weight: a.Count})
		kept["author:"+a.Pubkey] = true
	}
	if othersWeight > 0 {
		g.Nodes = append(g.Nodes, Node{ID: "author:others", Type: "author", Label: "other contributors", Weight: othersWeight})
	}

	// Author→community edges (only for kept author nodes).
	for _, ac := range in.AuthorCommunities {
		src := "author:" + ac.Author
		if !kept[src] {
			continue
		}
		g.Edges = append(g.Edges, Edge{Source: src, Target: "community:" + ac.Community, Weight: ac.Weight, Kind: "author_community"})
	}

	// Transferkiosk nodes + edges.
	for _, t := range in.TK {
		g.Nodes = append(g.Nodes, Node{ID: "tk:" + t.Coord, Type: tkNodeType(t.Kind), Label: t.Label, Weight: 1})
	}
	for _, t := range in.TK {
		if t.ParentCoord != "" {
			g.Edges = append(g.Edges, Edge{Source: "tk:" + t.Coord, Target: "tk:" + t.ParentCoord, Weight: 1, Kind: "part_of"})
		}
		if t.Kind == 30143 {
			if t.Author != "" && kept["author:"+t.Author] {
				g.Edges = append(g.Edges, Edge{Source: "author:" + t.Author, Target: "tk:" + t.Coord, Weight: 1, Kind: "author_project"})
			}
			if t.Community != "" {
				g.Edges = append(g.Edges, Edge{Source: "tk:" + t.Coord, Target: "community:" + t.Community, Weight: 1, Kind: "community_project"})
			}
		}
	}

	// Project weight = number of children (measures + publications).
	childCount := map[string]int{}
	for _, t := range in.TK {
		if t.ParentCoord != "" {
			childCount["tk:"+t.ParentCoord]++
		}
	}
	for i := range g.Nodes {
		if g.Nodes[i].Type == "project" {
			if c := childCount[g.Nodes[i].ID]; c > 0 {
				g.Nodes[i].Weight = c
			}
		}
	}

	return g
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestBuildGraph -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add viz_graph.go viz_graph_test.go
git commit -m "feat(viz): entity graph aggregator with top-N fold + partOf tree"
```

---

## Task 3: Stats assembler (`viz_stats.go`)

**Files:**
- Create: `viz_stats.go`
- Test: `viz_stats_test.go`

**Interfaces:**
- Consumes: `FacetValue` (Task 1).
- Produces:
  - `type TypeCount struct { Kind int; Label string; Count int }` (JSON `kind,label,count`)
  - `type SubjectCount struct { ID string; Label string; Count int }` (JSON `id,label,count`)
  - `type Totals struct { Resources int; ContentTypes int; Authors int; Communities int; Projects int }` (JSON `resources,content_types,authors,communities,projects`)
  - `type Stats struct { Totals Totals; ByType []TypeCount; Subjects []SubjectCount }` (JSON `totals,by_type,subjects`)
  - `type StatsInput struct { TypeCounts []TypeCount; Subjects []FacetValue; Authors int; Communities int; Projects int }`
  - `func buildStats(in StatsInput) Stats`

- [ ] **Step 1: Write the failing test**

```go
// viz_stats_test.go
package main

import "testing"

func TestBuildStats(t *testing.T) {
	in := StatsInput{
		TypeCounts: []TypeCount{
			{Kind: 30142, Label: "AMB resource", Count: 100},
			{Kind: 30023, Label: "Long-form", Count: 20},
			{Kind: 30818, Label: "Wiki", Count: 0},
		},
		Subjects:    []FacetValue{{Value: "Chemie", Count: 40}, {Value: "Physik", Count: 10}},
		Authors:     7,
		Communities: 3,
		Projects:    5,
	}
	s := buildStats(in)
	if s.Totals.Resources != 120 {
		t.Errorf("resources = %d, want 120", s.Totals.Resources)
	}
	if s.Totals.ContentTypes != 2 {
		t.Errorf("content_types = %d, want 2 (kinds with >=1 doc)", s.Totals.ContentTypes)
	}
	if s.Totals.Authors != 7 || s.Totals.Communities != 3 || s.Totals.Projects != 5 {
		t.Errorf("totals wrong: %+v", s.Totals)
	}
	if len(s.Subjects) != 2 || s.Subjects[0].Label != "Chemie" || s.Subjects[0].Count != 40 {
		t.Errorf("subjects wrong: %+v", s.Subjects)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestBuildStats -v`
Expected: FAIL — `undefined: StatsInput` / `undefined: buildStats`.

- [ ] **Step 3: Write minimal implementation**

```go
// viz_stats.go
package main

// TypeCount is one content kind and its document count.
type TypeCount struct {
	Kind  int    `json:"kind"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// SubjectCount is one subject facet bucket.
type SubjectCount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Totals are the stat-strip headline numbers.
type Totals struct {
	Resources    int `json:"resources"`
	ContentTypes int `json:"content_types"`
	Authors      int `json:"authors"`
	Communities  int `json:"communities"`
	Projects     int `json:"projects"`
}

// Stats is the /viz/stats response payload.
type Stats struct {
	Totals   Totals         `json:"totals"`
	ByType   []TypeCount    `json:"by_type"`
	Subjects []SubjectCount `json:"subjects"`
}

// StatsInput bundles the raw facts buildStats turns into the Stats payload.
type StatsInput struct {
	TypeCounts  []TypeCount
	Subjects    []FacetValue
	Authors     int
	Communities int
	Projects    int
}

// buildStats assembles the stat payload. Resources = sum of all type counts;
// ContentTypes = number of kinds with >=1 doc. Subjects fold FacetValue into
// the API shape (Value is used for both id and label — the AMB `about` facet
// stores the human label; a future taxonomy join can split them).
func buildStats(in StatsInput) Stats {
	resources := 0
	types := 0
	for _, tc := range in.TypeCounts {
		resources += tc.Count
		if tc.Count > 0 {
			types++
		}
	}
	subjects := make([]SubjectCount, 0, len(in.Subjects))
	for _, f := range in.Subjects {
		subjects = append(subjects, SubjectCount{ID: f.Value, Label: f.Value, Count: f.Count})
	}
	return Stats{
		Totals: Totals{
			Resources:    resources,
			ContentTypes: types,
			Authors:      in.Authors,
			Communities:  in.Communities,
			Projects:     in.Projects,
		},
		ByType:   in.TypeCounts,
		Subjects: subjects,
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestBuildStats -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add viz_stats.go viz_stats_test.go
git commit -m "feat(viz): stats assembler (totals + by_type + subjects)"
```

---

## Task 4: Handlers, cache, and Typesense wiring (`viz.go`)

**Files:**
- Create: `viz.go`
- Test: `viz_test.go`

**Interfaces:**
- Consumes: `vizCount`, `vizFacet`, `vizSearch`, `FacetValue`, `VizDoc` (Task 1); `buildGraph`, `GraphInput`, `AuthorStat`, `CommunityStat`, `AuthorCommunity`, `TKItem`, `Graph` (Task 2); `buildStats`, `StatsInput`, `TypeCount`, `Stats` (Task 3).
- Produces:
  - `type VizCollections struct { AMB string; Longform string; Wiki string; Calendar string; Shares string; Transferkiosk string }`
  - `type VizConfig struct { Host string; ApiKey string; Collections VizCollections; TopAuthors int; CacheTTL time.Duration }`
  - `func vizSetup(mux *http.ServeMux, cfg VizConfig)`
  - Internal: `type vizCache struct{...}`, `func (c *vizCache) get(key string, ttl time.Duration, build func() (any, error)) (any, error)`

- [ ] **Step 1: Write the failing test**

```go
// viz_test.go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTSMux serves canned responses keyed by which collection is in the path
// and whether facet_by is present. Enough to exercise the handlers end to end.
func fakeTSMux(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		facet := r.URL.Query().Get("facet_by")
		switch {
		case strings.Contains(path, "/amb/") && facet == "about":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found": 10,
				"facet_counts": []any{map[string]any{
					"field_name": "about",
					"counts":     []any{map[string]any{"value": "Chemie", "count": 6}},
				}},
			})
		case strings.Contains(path, "/amb/") && facet == "publisher":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found": 10,
				"facet_counts": []any{map[string]any{
					"field_name": "publisher",
					"counts":     []any{map[string]any{"value": "e-teaching.org", "count": 4}},
				}},
			})
		default:
			// plain count (per_page=0) or doc search
			_ = json.NewEncoder(w).Encode(map[string]any{"found": 10, "hits": []any{}})
		}
	}))
}

func testCfg(url string) VizConfig {
	return VizConfig{
		Host: url, ApiKey: "k",
		Collections: VizCollections{AMB: "amb", Transferkiosk: "transferkiosk", Shares: "community_shares"},
		TopAuthors:  40, CacheTTL: time.Minute,
	}
}

func TestVizStatsHandler(t *testing.T) {
	srv := fakeTSMux(t)
	defer srv.Close()
	mux := http.NewServeMux()
	vizSetup(mux, testCfg(srv.URL))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/viz/stats", nil)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var s Stats
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Totals.Resources == 0 {
		t.Errorf("expected non-zero resources, got %+v", s.Totals)
	}
}

func TestVizGraphHandler(t *testing.T) {
	srv := fakeTSMux(t)
	defer srv.Close()
	mux := http.NewServeMux()
	vizSetup(mux, testCfg(srv.URL))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/viz/graph", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var g Graph
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
}

func TestVizIndexServed(t *testing.T) {
	srv := fakeTSMux(t)
	defer srv.Close()
	mux := http.NewServeMux()
	vizSetup(mux, testCfg(srv.URL))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/viz", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("index status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<html") && !strings.Contains(rr.Body.String(), "<!DOCTYPE") {
		t.Errorf("index did not serve HTML")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestViz -v`
Expected: FAIL — `undefined: vizSetup` / `undefined: VizConfig`.

- [ ] **Step 3: Write minimal implementation**

Create `web/viz/index.html` as a placeholder first so the embed compiles (Task 6 replaces it):

```html
<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Relay Content</title></head>
<body><div id="app">loading…</div></body></html>
```

Then `viz.go`:

```go
// viz.go
package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web/viz
var vizAssets embed.FS

// VizCollections names the Typesense collections the viz endpoints read.
// Empty names are skipped (feature not enabled).
type VizCollections struct {
	AMB           string
	Longform      string
	Wiki          string
	Calendar      string
	Shares        string
	Transferkiosk string
}

// VizConfig bundles everything vizSetup needs. All fields are read-only use.
type VizConfig struct {
	Host        string
	ApiKey      string
	Collections VizCollections
	TopAuthors  int
	CacheTTL    time.Duration
}

// vizCache is a tiny TTL cache for the two expensive endpoints.
type vizCache struct {
	mu      sync.Mutex
	entries map[string]vizCacheEntry
}

type vizCacheEntry struct {
	val    any
	expiry time.Time
}

func newVizCache() *vizCache { return &vizCache{entries: map[string]vizCacheEntry{}} }

func (c *vizCache) get(key string, ttl time.Duration, build func() (any, error)) (any, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Now().Before(e.expiry) {
		c.mu.Unlock()
		return e.val, nil
	}
	c.mu.Unlock()
	val, err := build()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = vizCacheEntry{val: val, expiry: time.Now().Add(ttl)}
	c.mu.Unlock()
	return val, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// vizSetup registers all /viz routes on mux. Call ONLY when VIZ_ENABLED.
func vizSetup(mux *http.ServeMux, cfg VizConfig) {
	cache := newVizCache()

	sub, err := fs.Sub(vizAssets, "web/viz")
	if err != nil {
		panic(err) // embed path is a compile-time constant; this cannot fail
	}
	fileServer := http.FileServer(http.FS(sub))

	// GET /viz and /viz/ → index; /viz/<asset> → static file.
	mux.HandleFunc("/viz", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
	mux.HandleFunc("/viz/", func(w http.ResponseWriter, r *http.Request) {
		// API routes handled below take precedence via longer patterns.
		http.StripPrefix("/viz/", fileServer).ServeHTTP(w, r)
	})

	mux.HandleFunc("/viz/stats", func(w http.ResponseWriter, r *http.Request) {
		v, err := cache.get("stats", cfg.CacheTTL, func() (any, error) {
			return cfg.computeStats(r.Context())
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, v)
	})

	mux.HandleFunc("/viz/graph", func(w http.ResponseWriter, r *http.Request) {
		v, err := cache.get("graph", cfg.CacheTTL, func() (any, error) {
			return cfg.computeGraph(r.Context())
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, v)
	})

	mux.HandleFunc("/viz/node/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/viz/node/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "bad node ref", http.StatusBadRequest)
			return
		}
		items, err := cfg.computeNode(r.Context(), parts[0], parts[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, items)
	})
}

// contentCollections returns the non-empty content collections keyed by kind.
func (cfg VizConfig) contentCollections() map[int]struct {
	name  string
	label string
} {
	out := map[int]struct {
		name  string
		label string
	}{}
	add := func(kind int, name, label string) {
		if name != "" {
			out[kind] = struct {
				name  string
				label string
			}{name, label}
		}
	}
	add(30142, cfg.Collections.AMB, "AMB resource")
	add(30023, cfg.Collections.Longform, "Long-form")
	add(30818, cfg.Collections.Wiki, "Wiki")
	add(31923, cfg.Collections.Calendar, "Calendar")
	return out
}

// computeStats builds the /viz/stats payload from Typesense reads.
func (cfg VizConfig) computeStats(ctx context.Context) (Stats, error) {
	var typeCounts []TypeCount
	for kind, c := range cfg.contentCollections() {
		n, err := vizCount(ctx, cfg.Host, cfg.ApiKey, c.name)
		if err != nil {
			return Stats{}, err
		}
		typeCounts = append(typeCounts, TypeCount{Kind: kind, Label: c.label, Count: n})
	}

	var subjects []FacetValue
	if cfg.Collections.AMB != "" {
		subjects, _ = vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "about", 12)
	}

	authors, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "publisher", 1000)
	if err != nil {
		return Stats{}, err
	}
	communities, projects := cfg.communityAndProjectCounts(ctx)

	return buildStats(StatsInput{
		TypeCounts:  typeCounts,
		Subjects:    subjects,
		Authors:     len(authors),
		Communities: communities,
		Projects:    projects,
	}), nil
}

// communityAndProjectCounts returns distinct community count (from the shares
// collection `community` facet) and project count (30143 docs in transferkiosk).
func (cfg VizConfig) communityAndProjectCounts(ctx context.Context) (int, int) {
	communities := 0
	if cfg.Collections.Shares != "" {
		if f, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Shares, "community", 1000); err == nil {
			communities = len(f)
		}
	}
	projects := 0
	if cfg.Collections.Transferkiosk != "" {
		if docs, err := vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Transferkiosk, "kind:=30143", "id", 250); err == nil {
			projects = len(docs)
		}
	}
	return communities, projects
}

// computeGraph builds the /viz/graph payload. Author→community, transferkiosk
// trees, and project links are derived from Typesense reads. Missing
// collections degrade gracefully (that slice stays empty).
func (cfg VizConfig) computeGraph(ctx context.Context) (Graph, error) {
	in := GraphInput{TopAuthors: cfg.TopAuthors}

	// Authors from the AMB publisher facet.
	if cfg.Collections.AMB != "" {
		facets, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "publisher", 1000)
		if err != nil {
			return Graph{}, err
		}
		for _, f := range facets {
			in.Authors = append(in.Authors, AuthorStat{Pubkey: f.Value, Label: f.Value, Count: f.Count})
		}
	}

	// Communities from the shares `community` facet.
	if cfg.Collections.Shares != "" {
		if facets, err := vizFacet(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Shares, "community", 1000); err == nil {
			for _, f := range facets {
				in.Communities = append(in.Communities, CommunityStat{Pubkey: f.Value, Label: shortLabel(f.Value), Content: f.Count})
			}
		}
	}

	// Transferkiosk tree.
	if cfg.Collections.Transferkiosk != "" {
		docs, err := vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Transferkiosk, "", "id,kind,name,partOf,publisher,community", 1000)
		if err == nil {
			for _, d := range docs {
				in.TK = append(in.TK, tkItemFromDoc(d))
			}
		}
	}

	return buildGraph(in), nil
}

// computeNode returns the capped list of real events behind a node.
func (cfg VizConfig) computeNode(ctx context.Context, typ, id string) ([]VizDoc, error) {
	const cap = 50
	switch typ {
	case "author":
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "publisher:="+tsEscape(id), "id,name,kind,community", cap)
	case "community":
		if cfg.Collections.AMB == "" {
			return []VizDoc{}, nil
		}
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.AMB, "community:="+tsEscape(id), "id,name,kind,community", cap)
	case "project", "measure", "publication":
		if cfg.Collections.Transferkiosk == "" {
			return []VizDoc{}, nil
		}
		return vizSearch(ctx, cfg.Host, cfg.ApiKey, cfg.Collections.Transferkiosk, "partOf:="+tsEscape(id), "id,name,kind,partOf", cap)
	default:
		return []VizDoc{}, nil
	}
}

// tkItemFromDoc maps a transferkiosk Typesense doc to a TKItem.
func tkItemFromDoc(d VizDoc) TKItem {
	coord, _ := d["id"].(string)
	name, _ := d["name"].(string)
	pub, _ := d["publisher"].(string)
	kind := 0
	if k, ok := d["kind"].(float64); ok {
		kind = int(k)
	}
	parent := ""
	if p, ok := d["partOf"].(string); ok {
		parent = p
	}
	community := ""
	if c, ok := d["community"].(string); ok {
		community = c
	} else if cs, ok := d["community"].([]any); ok && len(cs) > 0 {
		community, _ = cs[0].(string)
	}
	return TKItem{Coord: coord, Kind: kind, Label: name, Author: pub, ParentCoord: parent, Community: community}
}

// shortLabel trims a pubkey to a compact display label.
func shortLabel(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}

// tsEscape wraps a filter_by value in backticks and escapes embedded backticks,
// so values containing ':' or spaces are treated literally by Typesense.
func tsEscape(v string) string {
	return "`" + strings.ReplaceAll(v, "`", "") + "`"
}
```

> NOTE for the implementer: `partOf` and `community` field names/shapes are the transferkiosk/shares projections. Confirm the exact Typesense field names in `transferkiosk.go` (`nostrToTransferkiosk`) and `shares.go` (`nostrToShare`) and adjust the `include_fields` CSV + `tkItemFromDoc` accessors if they differ. The `TKItem.Community` for a project comes from whichever field carries its `h`-tag/community set.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestViz -v`
Expected: PASS (all three handler tests).

- [ ] **Step 5: Commit**

```bash
git add viz.go viz_test.go web/viz/index.html
git commit -m "feat(viz): http handlers, ttl cache, typesense wiring"
```

---

## Task 5: Wire into main.go behind `VIZ_ENABLED`

**Files:**
- Modify: `main.go` (env reads near line 63-68; call site near line 1504 after `landing.Setup(relay)`)

**Interfaces:**
- Consumes: `vizSetup`, `VizConfig`, `VizCollections` (Task 4).
- Produces: nothing (wiring only).

- [ ] **Step 1: Add the env flag read (near the other *_ENABLED reads, ~line 68)**

```go
	vizEnabled := os.Getenv("VIZ_ENABLED") == "true"
```

- [ ] **Step 2: Register routes after landing.Setup (replace the `landing.Setup(relay)` line region)**

Find:

```go
	landing.Setup(relay)
```

Replace with:

```go
	landing.Setup(relay)

	if vizEnabled {
		topAuthors := 40
		if v := os.Getenv("VIZ_TOP_AUTHORS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				topAuthors = n
			}
		}
		cacheTTL := 60 * time.Second
		if v := os.Getenv("VIZ_CACHE_TTL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				cacheTTL = d
			}
		}
		vizSetup(relay.Router(), VizConfig{
			Host:   os.Getenv("TS_HOST"),
			ApiKey: os.Getenv("TS_APIKEY"),
			Collections: VizCollections{
				AMB:           os.Getenv("TS_COLLECTION"),
				Longform:      os.Getenv("TS_COLLECTION_LONGFORM"),
				Wiki:          os.Getenv("TS_COLLECTION_WIKI"),
				Calendar:      os.Getenv("TS_COLLECTION_CALENDAR"),
				Shares:        os.Getenv("TS_COLLECTION_SHARES"),
				Transferkiosk: os.Getenv("TS_COLLECTION_TRANSFERKIOSK"),
			},
			TopAuthors: topAuthors,
			CacheTTL:   cacheTTL,
		})
		fmt.Println("viz dashboard enabled at /viz")
	}
```

(`strconv` and `time` are already imported in main.go.)

- [ ] **Step 3: Build to verify it compiles**

Run: `go build .`
Expected: no output (success).

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS (no regressions).

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat(viz): wire /viz routes behind VIZ_ENABLED"
```

---

## Task 6: Frontend (`web/viz/*`)

**Files:**
- Create: `web/viz/force-graph.min.js` (vendored UMD build)
- Create/replace: `web/viz/index.html`, `web/viz/app.js`, `web/viz/style.css`

**Interfaces:**
- Consumes: `GET /viz/stats`, `GET /viz/graph`, `GET /viz/node/{type}/{id}` (Task 4).
- Produces: the rendered page (no Go interface).

- [ ] **Step 1: Vendor the graph library**

Download the self-contained UMD build of `force-graph` (MIT) into `web/viz/force-graph.min.js`:

```bash
curl -fsSL https://unpkg.com/force-graph/dist/force-graph.min.js -o web/viz/force-graph.min.js
test -s web/viz/force-graph.min.js && echo OK
```

Expected: `OK` (non-empty file). If unpkg is unreachable, fetch the same file from the npm tarball (`npm pack force-graph` → extract `dist/force-graph.min.js`).

- [ ] **Step 2: Write `web/viz/index.html`**

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Relay Content</title>
  <link rel="stylesheet" href="/viz/style.css">
</head>
<body>
  <header id="stats" class="strip"><!-- filled by app.js --></header>
  <main id="graph"></main>
  <aside id="panel" class="panel hidden"></aside>
  <div id="legend" class="legend"></div>
  <div id="toggles" class="toggles"></div>
  <script src="/viz/force-graph.min.js"></script>
  <script src="/viz/app.js"></script>
</body>
</html>
```

- [ ] **Step 3: Write `web/viz/style.css`**

```css
:root {
  --bg:#0f1216; --fg:#e8eaed; --muted:#8a8f98; --line:#262b33;
  --community:#5c7fdf; --author:#e39a5c; --project:#4cae7a; --measure:#5fc0b0; --publication:#b98cd8;
}
* { box-sizing:border-box; }
html,body { margin:0; height:100%; background:var(--bg); color:var(--fg); font-family:system-ui,sans-serif; }
body { display:flex; flex-direction:column; }
.strip { display:flex; border-bottom:1px solid var(--line); }
.strip .s { flex:1; text-align:center; padding:12px 6px; border-right:1px solid var(--line); }
.strip .s:last-child { border-right:0; }
.strip .big { font-size:1.4rem; font-weight:700; display:block; }
.strip .lbl { font-size:.62rem; letter-spacing:.08em; color:var(--muted); }
#graph { flex:1; position:relative; overflow:hidden; }
.legend { position:absolute; left:14px; bottom:14px; font-size:.72rem; background:rgba(0,0,0,.5); padding:8px 10px; border-radius:8px; line-height:1.6; }
.legend .dot { display:inline-block; width:9px; height:9px; border-radius:50%; margin-right:6px; }
.toggles { position:absolute; right:14px; top:74px; font-size:.75rem; background:rgba(0,0,0,.5); padding:8px 10px; border-radius:8px; }
.toggles label { display:block; margin:3px 0; cursor:pointer; }
.panel { position:absolute; right:0; top:0; bottom:0; width:280px; background:#151a20; border-left:1px solid var(--line); padding:14px; overflow-y:auto; }
.panel.hidden { display:none; }
.panel h3 { margin:.2rem 0; }
.panel .sub { color:var(--muted); font-size:.72rem; margin-bottom:10px; }
.panel .res { border:1px solid var(--line); border-radius:6px; padding:6px 8px; margin-bottom:6px; font-size:.8rem; }
.panel .res .m { color:var(--muted); font-size:.66rem; }
.panel .close { float:right; cursor:pointer; color:var(--muted); }
```

- [ ] **Step 4: Write `web/viz/app.js`**

```js
const COLORS = {
  community: '#5c7fdf', author: '#e39a5c',
  project: '#4cae7a', measure: '#5fc0b0', publication: '#b98cd8',
};
const TYPE_LABEL = {
  community: 'Community', author: 'Author / publisher',
  project: 'Projekt (30143)', measure: 'Maßnahme (30144)', publication: 'Publikation (30145)',
};
const visible = { community: true, author: true, transferkiosk: true };
const TK_TYPES = new Set(['project', 'measure', 'publication']);

async function j(url) { const r = await fetch(url); if (!r.ok) throw new Error(url + ' ' + r.status); return r.json(); }

function renderStats(s) {
  const t = s.totals;
  document.getElementById('stats').innerHTML = [
    ['RESOURCES', t.resources], ['CONTENT TYPES', t.content_types],
    ['AUTHORS', t.authors], ['COMMUNITIES', t.communities], ['PROJECTS', t.projects],
  ].map(([l, v]) => `<div class="s"><span class="big">${v ?? 0}</span><span class="lbl">${l}</span></div>`).join('');
}

function renderLegend() {
  document.getElementById('legend').innerHTML =
    Object.entries(TYPE_LABEL).map(([k, lbl]) =>
      `<div><span class="dot" style="background:${COLORS[k]}"></span>${lbl}</div>`).join('') +
    `<div style="margin-top:6px;color:var(--muted)">thicker line = more shared content</div>`;
}

function nodeVisible(n) {
  if (n.type === 'community') return visible.community;
  if (n.type === 'author') return visible.author;
  if (TK_TYPES.has(n.type)) return visible.transferkiosk;
  return true;
}

let Graph, fullData;

function apply() {
  const nodes = fullData.nodes.filter(nodeVisible);
  const ids = new Set(nodes.map(n => n.id));
  const links = fullData.edges
    .filter(e => ids.has(e.source.id || e.source) && ids.has(e.target.id || e.target))
    .map(e => ({ source: e.source.id || e.source, target: e.target.id || e.target, weight: e.weight }));
  Graph.graphData({ nodes, links });
}

function renderToggles() {
  const el = document.getElementById('toggles');
  const rows = [['community', 'Communities'], ['author', 'Authors'], ['transferkiosk', 'Transferkiosk']];
  el.innerHTML = rows.map(([k, lbl]) =>
    `<label><input type="checkbox" data-k="${k}" checked> ${lbl}</label>`).join('');
  el.querySelectorAll('input').forEach(cb => cb.addEventListener('change', () => {
    visible[cb.dataset.k] = cb.checked; apply();
  }));
}

async function openPanel(node) {
  const [typ] = node.id.split(':');
  const ref = node.id.slice(typ.length + 1);
  const panel = document.getElementById('panel');
  panel.classList.remove('hidden');
  panel.innerHTML = `<span class="close">✕</span><h3>${node.label}</h3><div class="sub">loading…</div>`;
  panel.querySelector('.close').onclick = () => panel.classList.add('hidden');
  try {
    const items = await j(`/viz/node/${encodeURIComponent(node.type)}/${encodeURIComponent(ref)}`);
    const list = (items || []).map(it =>
      `<div class="res"><div>${it.name || '(untitled)'}</div><div class="m">${it.kind || ''}</div></div>`).join('');
    panel.innerHTML = `<span class="close">✕</span><h3>${node.label}</h3>` +
      `<div class="sub">${node.type} · ${(items || []).length} items</div>${list || '<div class="sub">no items</div>'}`;
    panel.querySelector('.close').onclick = () => panel.classList.add('hidden');
  } catch (e) {
    panel.innerHTML = `<span class="close" onclick="this.parentElement.classList.add('hidden')">✕</span><h3>${node.label}</h3><div class="sub">error: ${e.message}</div>`;
  }
}

async function main() {
  renderLegend(); renderToggles();
  renderStats(await j('/viz/stats'));
  fullData = await j('/viz/graph');

  Graph = ForceGraph()(document.getElementById('graph'))
    .backgroundColor('#0f1216')
    .nodeColor(n => COLORS[n.type] || '#888')
    .nodeVal(n => Math.max(2, Math.sqrt(n.weight || 1) * 2))
    .nodeLabel(n => `${n.label} (${n.weight})`)
    .nodeCanvasObjectMode(() => 'after')
    .nodeCanvasObject((n, ctx, scale) => {
      const label = n.label || '';
      ctx.font = `${Math.max(3, 11 / scale)}px system-ui`;
      ctx.fillStyle = '#c8ccd2';
      ctx.textAlign = 'center';
      ctx.fillText(label, n.x, n.y + 10 / scale);
    })
    .linkWidth(l => Math.max(1, Math.sqrt(l.weight || 1)))
    .linkColor(() => 'rgba(140,160,200,.35)')
    .onNodeClick(openPanel);

  apply();
}
main().catch(e => { document.getElementById('graph').innerHTML = '<p style="padding:20px">' + e.message + '</p>'; });
```

- [ ] **Step 5: Rebuild (embed picks up new assets) and manually verify**

```bash
go build .
```

Then run locally against a Typesense with data:

```bash
docker compose up -d typesense
VIZ_ENABLED=true TRANSFERKIOSK_ENABLED=true COMMUNITY_SHARES_ENABLED=true go run .
```

Open `http://localhost:3334/viz`. Verify (golden path): stat strip shows numbers; graph renders colored nodes with labels; toggles hide/show layers; clicking a node opens the panel with real items. Also verify an author with no community edges still renders (edge case). Note in the commit if Typesense had no data to fully exercise it.

- [ ] **Step 6: Commit**

```bash
git add web/viz/force-graph.min.js web/viz/index.html web/viz/app.js web/viz/style.css
git commit -m "feat(viz): embedded hero-graph frontend (stat strip, force graph, drill-down)"
```

---

## Task 7: Documentation

**Files:**
- Modify: `.env.example`, `CLAUDE.md`

**Interfaces:** none.

- [ ] **Step 1: Add env vars to `.env.example`**

Append near the other feature flags:

```bash
# Content visualization dashboard (read-only /viz page). Default off.
VIZ_ENABLED=false
# Top-N authors shown as individual nodes before folding into an "others" node.
VIZ_TOP_AUTHORS=40
# In-memory cache TTL for /viz/stats and /viz/graph (Go duration).
VIZ_CACHE_TTL=60s
```

- [ ] **Step 2: Add a section to `CLAUDE.md`**

Under the feature descriptions (after the community-shares section), add:

```markdown
**Content visualization (`/viz`), gated behind `VIZ_ENABLED`:**
- When `VIZ_ENABLED=true`, the relay serves a read-only dashboard at `/viz` (embedded via `//go:embed web/viz`, routes on `relay.Router()`). A stat strip (totals + subjects) plus a force-directed graph of **entities-as-nodes, content-as-weight**: community hubs, top-N authors (rest folded into one "others" node, `VIZ_TOP_AUTHORS`), and the transferkiosk project→Maßnahme→Publikation trees. Clicking a node fetches its real resources live.
- Endpoints (all read-only, no auth, aggregated/capped data only): `/viz/stats`, `/viz/graph`, `/viz/node/{type}/{id}`. Backed by plain Typesense `facet_by`/`search` reads (`viz_ts.go`), pure assemblers (`viz_graph.go`, `viz_stats.go`), and an in-memory TTL cache (`VIZ_CACHE_TTL`). Aggregation degrades gracefully when a content collection is disabled.
```

- [ ] **Step 3: Add env var docs to the `## Environment Variables` list in `CLAUDE.md`**

```markdown
- `VIZ_ENABLED`: Set to `true` to serve the read-only `/viz` visualization dashboard (default `false`).
- `VIZ_TOP_AUTHORS`: Top-N authors rendered as individual graph nodes before folding the tail into an "others" node (default `40`).
- `VIZ_CACHE_TTL`: In-memory cache TTL for `/viz/stats` and `/viz/graph` as a Go duration (default `60s`).
```

- [ ] **Step 4: Commit**

```bash
git add .env.example CLAUDE.md
git commit -m "docs(viz): document VIZ_ENABLED dashboard + env vars"
```

---

## Self-Review notes (addressed)

- **Spec coverage:** endpoints (Task 4), graph model + aggregation (Task 2), stats incl. subjects strip (Task 3, Task 6 render), caching/TTL (Task 4), config flags (Task 5, Task 7), security gate (Task 5 conditional registration), frontend Layout A with always-on labels + toggles + drill-down (Task 6), testing (Tasks 1-4). Covered.
- **Field-name risk:** the transferkiosk `partOf`/`community` and shares `community` Typesense field names are called out inline in Task 4 for the implementer to confirm against `transferkiosk.go`/`shares.go` before relying on `tkItemFromDoc` — the one place reality may diverge from the plan.
- **Type consistency:** `Node/Edge/Graph`, `Stats/Totals/TypeCount/SubjectCount`, and `VizConfig/VizCollections` names are used identically across Tasks 2-6.
