package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPChunkSearcher_HappyPath(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{
					"event_id":    "aa11",
					"event_coord": "30142:pk1:doc-1",
					"score":       0.9,
					"snippet":     "…Photosyntheserate…",
					"page":        12,
					"heading":     "Lichtabhängigkeit",
					"source_url":  "https://example.org/skript.pdf",
				},
				{"event_id": "bb22", "score": 0.5},
			},
			"total": 2,
		})
	}))
	defer srv.Close()

	s := newHTTPChunkSearcher(srv.URL, "sekrit")
	hits, err := s.SearchChunks(context.Background(), "mathematik", 40)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}

	if gotPath != "/search_chunks" {
		t.Errorf("path = %s, want /search_chunks", gotPath)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("auth = %q, want Bearer sekrit", gotAuth)
	}
	if gotBody["q"] != "mathematik" || gotBody["k"] != float64(40) {
		t.Errorf("request body = %v, want q=mathematik k=40", gotBody)
	}
	if len(hits) != 2 || hits[0].EventID != "aa11" || hits[0].Score != 0.9 || hits[1].EventID != "bb22" {
		t.Errorf("hits = %+v", hits)
	}
	want := ChunkHit{
		EventID:    "aa11",
		EventCoord: "30142:pk1:doc-1",
		Score:      0.9,
		Snippet:    "…Photosyntheserate…",
		Page:       12,
		Heading:    "Lichtabhängigkeit",
		SourceURL:  "https://example.org/skript.pdf",
	}
	if hits[0] != want {
		t.Errorf("hits[0] = %+v, want %+v", hits[0], want)
	}
	if hits[1].Snippet != "" || hits[1].Page != 0 || hits[1].Heading != "" || hits[1].SourceURL != "" {
		t.Errorf("hits[1] locators should be zero-valued: %+v", hits[1])
	}
}

func TestHTTPChunkSearcher_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := newHTTPChunkSearcher(srv.URL, "wrong")
	_, err := s.SearchChunks(context.Background(), "q", 10)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("want 401 error, got %v", err)
	}
}

func TestHTTPChunkSearcher_MalformedJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	s := newHTTPChunkSearcher(srv.URL, "t")
	_, err := s.SearchChunks(context.Background(), "q", 10)
	if err == nil {
		t.Error("want decode error, got nil")
	}
}

func TestHTTPChunkSearcher_ContextCancelIsError(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	s := newHTTPChunkSearcher(srv.URL, "t")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.SearchChunks(ctx, "q", 10)
	if err == nil {
		t.Error("want timeout error, got nil")
	}
}
