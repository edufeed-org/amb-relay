package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// tsPatchTimeout bounds each partial-update HTTP call.
const tsPatchTimeout = 10 * time.Second

// tsHTTPClient is reused so HTTP connections to Typesense are pooled.
var tsHTTPClient = &http.Client{Timeout: tsPatchTimeout}

// PatchContent updates the content, content_fetched_at, and content_status
// fields on an existing Typesense document. Returns an error on non-2xx.
// Caller supplies the Typesense host, api key, collection name, and doc id
// (which for amb-relay events is the event id hex).
func PatchContent(host, apiKey, collection, docID string, entry ContentEntry) error {
	body := map[string]any{
		"content":            entry.Text,
		"content_fetched_at": entry.FetchedAt,
		"content_status":     entry.Status,
	}
	return patchDoc(host, apiKey, collection, docID, body)
}

// ClearContent sets content to empty and removes the other content metadata.
// Used when refetchcontent is called before an indexer has repopulated.
func ClearContent(host, apiKey, collection, docID string) error {
	body := map[string]any{
		"content":            "",
		"content_fetched_at": 0,
		"content_status":     "",
	}
	return patchDoc(host, apiKey, collection, docID, body)
}

func patchDoc(host, apiKey, collection, docID string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal patch body: %w", err)
	}
	url := fmt.Sprintf("%s/collections/%s/documents/%s", host, collection, docID)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TYPESENSE-API-KEY", apiKey)

	resp, err := tsHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("typesense patch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("typesense patch status %d: %s", resp.StatusCode, string(snippet))
	}
	return nil
}
