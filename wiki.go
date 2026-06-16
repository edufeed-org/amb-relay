package main

import (
	"fmt"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// WikiDocument is the Typesense document shape for NIP-54 kind-30818 wiki
// articles stored in the wiki_30818 collection. It shares the structured
// envelope with LongformDocument; unlike long-form, title is optional (a wiki
// article's display title defaults to its d identifier).
type WikiDocument struct {
	ID      string `json:"id"`
	D       string `json:"d"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Content string `json:"content,omitempty"`
	structuredEnvelope
}

// nostrToWiki projects a kind-30818 event into a WikiDocument.
func nostrToWiki(event *nostr.Event) (*WikiDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("wiki event %s missing required 'd' tag", event.ID.Hex())
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	doc := &WikiDocument{
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
		}
	}
	return doc, nil
}

// storeWiki projects and upserts a kind-30818 event to the wiki collection.
func storeWiki(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "wiki", nostrToWiki)
}

// wikiSchema returns the Typesense collection schema for kind-30818 wiki
// articles. Mirrors longformSchema's envelope naming so the shared query path
// reconstructs events identically; title is optional here.
func wikiSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "title", Type: "string", Optional: true},
			{Name: "summary", Type: "string", Optional: true},
			{Name: "content", Type: "string", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}
