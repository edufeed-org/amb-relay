package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// tsPatchTimeout bounds each partial-update HTTP call.
const tsPatchTimeout = 10 * time.Second

// tsHTTPClient is reused so HTTP connections to Typesense are pooled.
var tsHTTPClient = &http.Client{Timeout: tsPatchTimeout}

// tsDocIDFromEvent returns the Typesense document ID used by the
// typesense30142 eventstore for this addressable event. The eventstore
// keys documents by {pubkey}:{d-tag}, not by event hex id — so any
// partial-update path (PatchContent, ClearContent) must resolve the
// d-tag from the event before calling Typesense.
func tsDocIDFromEvent(event nostr.Event) (string, error) {
	dTag := event.Tags.Find("d")
	if dTag == nil || len(dTag) < 2 || dTag[1] == "" {
		return "", fmt.Errorf("event %s has no d-tag", event.ID.Hex())
	}
	return typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag[1]), nil
}

// PatchContent updates the content, content_fetched_at, and content_status
// fields on an existing Typesense document. Returns an error on non-2xx.
// docID must be the Typesense document id (see tsDocIDFromEvent) — the
// typesense30142 eventstore keys by {pubkey}:{d-tag}, not event hex id.
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
