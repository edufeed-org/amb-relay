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
		q := r.URL.Query()
		facet := q.Get("facet_by")
		page := q.Get("page")

		switch {
		case strings.Contains(path, "/community_shares/") && facet == "community":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found": 3,
				"facet_counts": []any{map[string]any{
					"field_name": "community",
					"counts": []any{
						map[string]any{"value": "cpk1", "count": 5},
						map[string]any{"value": "cpk2", "count": 2},
					},
				}},
			})
		case strings.Contains(path, "/amb/") && page == "1":
			// One AMB doc for the client-side aggregate (publisher/about/community).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found": 1,
				"hits": []any{map[string]any{"document": map[string]any{
					"publisher": []any{map[string]any{"name": "e-teaching.org"}},
					"about":     []any{map[string]any{"id": "s1017", "prefLabel": map[string]any{"de": "Chemie"}}},
					"community": []any{"cpk1"},
				}}},
			})
		case strings.Contains(path, "/transferkiosk/") && page == "1":
			// A project + a measure that references it via partOf.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"found": 2,
				"hits": []any{
					map[string]any{"document": map[string]any{
						"id": "pk:proj", "eventKind": float64(30143), "name": "OER-Transfer",
						"publisher": "e-teaching.org", "community": []any{"cpk1"},
					}},
					map[string]any{"document": map[string]any{
						"id": "pk:meas", "eventKind": float64(30144), "name": "Maßnahme A",
						"partOf": "30143:pk:proj",
					}},
				},
			})
		default:
			// plain count (per_page=0), empty later pages, or empty searches.
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
	if s.Totals.Authors != 1 {
		t.Errorf("authors = %d, want 1", s.Totals.Authors)
	}
	if s.Totals.Communities != 2 {
		t.Errorf("communities = %d, want 2", s.Totals.Communities)
	}
	if len(s.Subjects) == 0 || s.Subjects[0].Label != "Chemie" {
		t.Errorf("subjects = %+v, want first label Chemie", s.Subjects)
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

	// Author node, both communities, and the project→measure part_of edge.
	var haveAuthor, haveProject, havePartOf bool
	for _, n := range g.Nodes {
		if n.ID == "author:e-teaching.org" {
			haveAuthor = true
		}
		if n.ID == "tk:30143:pk:proj" && n.Type == "project" {
			haveProject = true
		}
	}
	for _, e := range g.Edges {
		if e.Kind == "part_of" && e.Source == "tk:30144:pk:meas" && e.Target == "tk:30143:pk:proj" {
			havePartOf = true
		}
	}
	if !haveAuthor {
		t.Errorf("missing author node; nodes=%+v", g.Nodes)
	}
	if !haveProject {
		t.Errorf("missing project node; nodes=%+v", g.Nodes)
	}
	if !havePartOf {
		t.Errorf("missing part_of edge; edges=%+v", g.Edges)
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

func TestVizNodeBadRef(t *testing.T) {
	srv := fakeTSMux(t)
	defer srv.Close()
	mux := http.NewServeMux()
	vizSetup(mux, testCfg(srv.URL))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/viz/node/author", nil) // missing id segment
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad node ref status = %d, want 400", rr.Code)
	}
}
