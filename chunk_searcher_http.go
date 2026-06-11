package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpChunkSearcher calls amb-indexer's POST /search_chunks. The timeout is
// deliberately short: this sits on the relay's REQ path, and a slow indexer
// must degrade to plain search (see chunkRerankQuery fallback), not stall
// every search subscription.
type httpChunkSearcher struct {
	baseURL string
	token   string
	client  *http.Client
}

func newHTTPChunkSearcher(baseURL, token string) *httpChunkSearcher {
	return &httpChunkSearcher{
		baseURL: baseURL,
		token:   token,
		client:  &http.Client{Timeout: 2 * time.Second},
	}
}

func (s *httpChunkSearcher) SearchChunks(ctx context.Context, q string, k int) ([]ChunkHit, error) {
	reqBody, err := json.Marshal(map[string]any{"q": q, "k": k})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/search_chunks", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search_chunks request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("search_chunks status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Hits []struct {
			EventID    string  `json:"event_id"`
			EventCoord string  `json:"event_coord"`
			Score      float64 `json:"score"`
			Snippet    string  `json:"snippet"`
			Page       int     `json:"page"`
			Heading    string  `json:"heading"`
			SourceURL  string  `json:"source_url"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode search_chunks response: %w", err)
	}

	hits := make([]ChunkHit, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		hits = append(hits, ChunkHit{
			EventID:    h.EventID,
			EventCoord: h.EventCoord,
			Score:      h.Score,
			Snippet:    h.Snippet,
			Page:       h.Page,
			Heading:    h.Heading,
			SourceURL:  h.SourceURL,
		})
	}
	return hits, nil
}
