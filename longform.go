package main

import (
	"encoding/json"
	"fmt"
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
