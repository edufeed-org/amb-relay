package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// LongformDocument is the Typesense document shape for NIP-23 kind-30023
// long-form events stored in the longform_30023 collection. It deliberately
// duplicates the envelope fields of AMBMetadata rather than sharing a type —
// see Phase 2 of the multi-content-search plan.
type LongformDocument struct {
	ID          string   `json:"id"`
	D           string   `json:"d"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary,omitempty"`
	Content     string   `json:"content,omitempty"`
	PublishedAt int64    `json:"published_at,omitempty"`
	Topics      []string `json:"t,omitempty"`
	Image       string   `json:"image,omitempty"`

	EventID        string `json:"eventID"`
	EventKind      int    `json:"eventKind"`
	EventPubKey    string `json:"eventPubKey"`
	EventCreatedAt int64  `json:"eventCreatedAt"`
	EventRaw       string `json:"eventRaw"`
}

// nostrToLongform projects a kind-30023 event into a LongformDocument.
func nostrToLongform(event *nostr.Event) (*LongformDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("longform event %s missing required 'd' tag", event.ID.Hex())
	}

	raw, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal raw event: %w", err)
	}

	doc := &LongformDocument{
		ID:             typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag),
		D:              dTag,
		Content:        event.Content,
		EventID:        event.ID.Hex(),
		EventKind:      int(event.Kind),
		EventPubKey:    event.PubKey.Hex(),
		EventCreatedAt: int64(event.CreatedAt),
		EventRaw:       string(raw),
	}

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "title":
			doc.Title = tag[1]
		case "summary":
			doc.Summary = tag[1]
		case "image":
			doc.Image = tag[1]
		case "published_at":
			if n, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
				doc.PublishedAt = n
			}
		case "t":
			doc.Topics = append(doc.Topics, tag[1])
		}
	}
	return doc, nil
}

// upsertLongform synchronously upserts a LongformDocument into the long-form
// Typesense collection via the import API. Mirrors typesense30142.upsertDocument
// but lives here because that lib helper (and its X-TYPESENSE-API-KEY-setting
// HTTP client) is unexported. Long-form write volume is low, so a synchronous
// POST per event is fine — no write buffer needed.
func upsertLongform(ts *typesense30142.TSBackend, doc *LongformDocument) error {
	jsonData, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal longform doc: %w", err)
	}
	url := fmt.Sprintf("%s/collections/%s/documents/import?action=upsert", ts.Host, ts.CollectionName)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("build longform upsert request: %w", err)
	}
	req.Header.Set("X-TYPESENSE-API-KEY", ts.ApiKey)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("longform upsert request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("longform upsert failed, status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err == nil && !result.Success && result.Error != "" {
		return fmt.Errorf("longform upsert failed: %s", result.Error)
	}
	return nil
}

// storeLongform projects and upserts a kind-30023 event to the long-form
// Typesense collection. No-op when long-form is disabled. Errors are logged,
// not returned — the event is already durably in BoltDB; a TS blip must not
// reject the write (mirrors the AMB buffer's fire-and-forget semantics).
func storeLongform(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	if !enabled || ts == nil {
		return
	}
	doc, err := nostrToLongform(&event)
	if err != nil {
		fmt.Printf("longform project %s: %v\n", event.ID.Hex(), err)
		return
	}
	if err := upsertLongform(ts, doc); err != nil {
		fmt.Printf("longform upsert %s: %v\n", event.ID.Hex(), err)
	}
}

// longformSchema returns the Typesense collection schema for kind-30023
// long-form events. Mirrors the envelope-field naming of DefaultSchema() so
// the shared query path reconstructs events identically.
func longformSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: []typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "title", Type: "string"},
			{Name: "summary", Type: "string", Optional: true},
			{Name: "content", Type: "string", Optional: true},
			{Name: "published_at", Type: "int64", Optional: true, Facet: true},
			{Name: "t", Type: "string[]", Optional: true, Facet: true},
			{Name: "image", Type: "string", Optional: true},
			{Name: "eventID", Type: "string"},
			{Name: "eventKind", Type: "int32", Facet: true},
			{Name: "eventPubKey", Type: "string", Facet: true},
			{Name: "eventCreatedAt", Type: "int64"},
			{Name: "eventRaw", Type: "string", Optional: true},
		},
	}
}
