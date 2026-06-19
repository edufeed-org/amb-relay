package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// structuredEnvelope holds the raw-event fields every simple structured
// collection (long-form, wiki, …) carries so the shared query path can
// reconstruct the original nostr event. Embedded (anonymously) into each
// per-kind document so its fields are promoted to top-level JSON keys.
type structuredEnvelope struct {
	EventID        string   `json:"eventID"`
	EventKind      int      `json:"eventKind"`
	EventPubKey    string   `json:"eventPubKey"`
	EventCreatedAt int64    `json:"eventCreatedAt"`
	EventRaw       string   `json:"eventRaw"`
	Community      []string `json:"community,omitempty"`
}

// newStructuredEnvelope marshals the raw event and fills the envelope fields.
func newStructuredEnvelope(event *nostr.Event) (structuredEnvelope, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return structuredEnvelope{}, fmt.Errorf("marshal raw event: %w", err)
	}
	var community []string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "h" {
			community = append(community, tag[1])
		}
	}
	return structuredEnvelope{
		EventID:        event.ID.Hex(),
		EventKind:      int(event.Kind),
		EventPubKey:    event.PubKey.Hex(),
		EventCreatedAt: int64(event.CreatedAt),
		EventRaw:       string(raw),
		Community:      community,
	}, nil
}

// structuredEnvelopeFields returns the Typesense field defs for the envelope,
// shared by every simple structured collection's schema.
func structuredEnvelopeFields() []typesense30142.Field {
	return []typesense30142.Field{
		{Name: "eventID", Type: "string"},
		{Name: "eventKind", Type: "int32", Facet: true},
		{Name: "eventPubKey", Type: "string", Facet: true},
		{Name: "eventCreatedAt", Type: "int64"},
		{Name: "eventRaw", Type: "string", Optional: true},
		{Name: "community", Type: "string[]", Facet: true, Optional: true},
	}
}

// upsertStructuredDoc synchronously upserts a document into a simple structured
// Typesense collection via the import API. Mirrors typesense30142's unexported
// upsertDocument. Write volume for these collections is low, so a synchronous
// POST per event is fine — no write buffer needed (unlike the AMB path).
func upsertStructuredDoc(ts *typesense30142.TSBackend, doc any) error {
	jsonData, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal structured doc: %w", err)
	}
	url := fmt.Sprintf("%s/collections/%s/documents/import?action=upsert", ts.Host, ts.CollectionName)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("build structured upsert request: %w", err)
	}
	req.Header.Set("X-TYPESENSE-API-KEY", ts.ApiKey)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("structured upsert request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("structured upsert failed, status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err == nil && !result.Success && result.Error != "" {
		return fmt.Errorf("structured upsert failed: %s", result.Error)
	}
	return nil
}

// reprojectStructured projects an addressable event and synchronously upserts
// it, returning any error. Unlike storeStructured (fire-and-forget for the live
// write path), the error is propagated so the reindexer can count failures.
func reprojectStructured[T any](ts *typesense30142.TSBackend, event nostr.Event, project func(*nostr.Event) (*T, error)) error {
	doc, err := project(&event)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := upsertStructuredDoc(ts, doc); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}

// storeStructured projects and upserts an addressable event to a simple
// structured Typesense collection. No-op when disabled or ts is nil. Errors are
// logged, not returned — the event is already durable in BoltDB; a TS blip must
// not reject the write (mirrors the AMB buffer's fire-and-forget semantics).
func storeStructured[T any](enabled bool, ts *typesense30142.TSBackend, event nostr.Event, label string, project func(*nostr.Event) (*T, error)) {
	if !enabled || ts == nil {
		return
	}
	if err := reprojectStructured(ts, event, project); err != nil {
		fmt.Printf("%s %s: %v\n", label, event.ID.Hex(), err)
	}
}
