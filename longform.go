package main

import (
	"fmt"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// LongformDocument is the Typesense document shape for NIP-23 kind-30023
// long-form events stored in the longform_30023 collection. It embeds the
// shared structuredEnvelope so the raw-event fields are promoted to top-level
// JSON keys (byte-identical to the pre-extraction layout).
type LongformDocument struct {
	ID          string    `json:"id"`
	D           string    `json:"d"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary,omitempty"`
	Content     string    `json:"content,omitempty"`
	PublishedAt int64     `json:"published_at,omitempty"`
	Topics      []string  `json:"t,omitempty"`
	Image       string    `json:"image,omitempty"`
	Embedding   []float32 `json:"embedding,omitempty"`
	structuredEnvelope
}

// nostrToLongform projects a kind-30023 event into a LongformDocument.
func nostrToLongform(event *nostr.Event) (*LongformDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("longform event %s missing required 'd' tag", event.ID.Hex())
	}

	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}

	doc := &LongformDocument{
		ID:                 typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag),
		D:                  dTag,
		Content:            event.Content,
		structuredEnvelope: env,
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

// EmbedText returns the passage text: non-empty title, summary, content.
func (d *LongformDocument) EmbedText() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{d.Title, d.Summary, d.Content} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// SetEmbedding stores the computed dense vector.
func (d *LongformDocument) SetEmbedding(v []float32) { d.Embedding = v }

// storeLongform projects and upserts a kind-30023 event to the long-form
// Typesense collection via the shared structured-collection helper.
func storeLongform(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "longform", nostrToLongform)
}

// longformSchema returns the Typesense collection schema for kind-30023
// long-form events. Mirrors the envelope-field naming of DefaultSchema() so
// the shared query path reconstructs events identically.
func longformSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "title", Type: "string"},
			{Name: "summary", Type: "string", Optional: true},
			{Name: "content", Type: "string", Optional: true},
			{Name: "published_at", Type: "int64", Optional: true, Facet: true},
			{Name: "t", Type: "string[]", Optional: true, Facet: true},
			{Name: "image", Type: "string", Optional: true},
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}
