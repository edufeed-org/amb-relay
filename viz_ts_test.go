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
