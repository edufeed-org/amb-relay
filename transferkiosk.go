package main

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// TransferkioskDocument is the Typesense shape for the two NIP-DIDACTIC
// transferkiosk kinds — Projekt (30143), Maßnahme (30144). Publikationen
// moved to NKBIP-01 kind 30040 (see publications.go); kind 30145 is retired.
// Both kinds share one collection, distinguished by EventKind (envelope) and
// the schema.org Type tag. Kind-specific fields are optional, so each kind
// populates only the subset it carries.
//
// SearchText is the load-bearing field: it concatenates description, every
// narrative tag, and every concept prefLabel:de so a topical NIP-50 search hits
// the German concept labels and narrative bodies (not just the short
// description). It is both the BM25 search field and the embedded passage text.
type TransferkioskDocument struct {
	ID          string `json:"id"`
	D           string `json:"d"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SearchText  string `json:"searchText,omitempty"`
	Content     string `json:"content,omitempty"`

	// Project↔measure graph: parent project coords (30143:<pub>:<d>).
	// Multi-valued: an event may carry several isPartOf/isOutputOf `a` tags.
	PartOf []string `json:"partOf,omitempty"`

	// High-coverage concept facets (prefLabel:de values, directly filterable).
	About           []string `json:"about,omitempty"`
	Audience        []string `json:"audience,omitempty"`
	Activity        []string `json:"activity,omitempty"`
	ActionField     []string `json:"actionField,omitempty"`
	ActionScope     []string `json:"actionScope,omitempty"`
	StudyModel      []string `json:"studyModel,omitempty"`
	Objective       []string `json:"objective,omitempty"`
	Transferability []string `json:"transferability,omitempty"`

	// Scalar facets.
	Status         string `json:"status,omitempty"`
	FunderName     string `json:"funderName,omitempty"`
	FunderProgram  string `json:"funderProgram,omitempty"`
	HostName       string `json:"hostName,omitempty"`
	HostBundesland string `json:"hostBundesland,omitempty"`
	StartDate      string `json:"startDate,omitempty"`
	EndDate        string `json:"endDate,omitempty"`

	Embedding []float32 `json:"embedding,omitempty"`
	structuredEnvelope
}

// tkNarrativeTags are free-text narrative fields folded into SearchText.
var tkNarrativeTags = map[string]bool{
	"summary": true, "challenge": true, "approach": true, "context": true,
	"prerequisite": true, "procedure": true, "outcome": true, "learnings": true,
	"recommendation": true, "tips": true,
}

// nostrToTransferkiosk projects a kind-30143/30144 event into a
// TransferkioskDocument. Concept facets follow the AMB flat-triple shape: a
// `<facet>:prefLabel:de` tag carries the human label. Every such label folds
// into SearchText; the high-value facets additionally land in a named slice.
func nostrToTransferkiosk(event *nostr.Event) (*TransferkioskDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("transferkiosk event %s missing required 'd' tag", event.ID.Hex())
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	doc := &TransferkioskDocument{
		ID:                 docIDFor(event.Kind, event.PubKey.Hex(), dTag),
		D:                  dTag,
		Content:            event.Content,
		structuredEnvelope: env,
	}

	var labels []string    // every prefLabel:de, for SearchText
	var narrative []string // narrative tag values, for SearchText

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		key, val := tag[0], tag[1]

		// Concept label: "<facet>:prefLabel:de".
		if facet, ok := strings.CutSuffix(key, ":prefLabel:de"); ok {
			labels = append(labels, val)
			switch facet {
			case "about":
				doc.About = append(doc.About, val)
			case "audience":
				doc.Audience = append(doc.Audience, val)
			case "activity":
				doc.Activity = append(doc.Activity, val)
			case "actionField":
				doc.ActionField = append(doc.ActionField, val)
			case "actionScope":
				doc.ActionScope = append(doc.ActionScope, val)
			case "studyModel":
				doc.StudyModel = append(doc.StudyModel, val)
			case "objective":
				doc.Objective = append(doc.Objective, val)
			case "transferability":
				doc.Transferability = append(doc.Transferability, val)
			}
			continue
		}

		if tkNarrativeTags[key] {
			narrative = append(narrative, val)
			continue
		}

		switch key {
		case "type":
			doc.Type = val
		case "name":
			doc.Name = val
		case "description":
			doc.Description = val
		case "status":
			doc.Status = val
		case "funder:name":
			doc.FunderName = val
		case "funder:program:name":
			doc.FunderProgram = val
		case "host:name":
			doc.HostName = val
		case "host:location:name":
			doc.HostBundesland = val
		case "startDate":
			doc.StartDate = val
		case "endDate":
			doc.EndDate = val
		case "a":
			// parent project link via isPartOf / isOutputOf marker (tag[3]).
			if len(tag) >= 4 && (tag[3] == "isPartOf" || tag[3] == "isOutputOf") {
				doc.PartOf = append(doc.PartOf, val)
			}
		}
	}

	parts := make([]string, 0, len(narrative)+len(labels)+1)
	if doc.Description != "" {
		parts = append(parts, doc.Description)
	}
	parts = append(parts, narrative...)
	parts = append(parts, labels...)
	doc.SearchText = strings.Join(parts, " ")

	return doc, nil
}

// EmbedText returns the passage text to embed: the folded SearchText, falling
// back to content then name when no labels/narrative/description exist.
func (d *TransferkioskDocument) EmbedText() string {
	for _, s := range []string{d.SearchText, d.Content, d.Name} {
		if s != "" {
			return s
		}
	}
	return ""
}

// SetEmbedding stores the computed dense vector.
func (d *TransferkioskDocument) SetEmbedding(v []float32) { d.Embedding = v }

// storeTransferkiosk projects and upserts a kind-30143/30144 event to the
// shared transferkiosk collection via the structured-collection helper.
func storeTransferkiosk(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "transferkiosk", nostrToTransferkiosk)
}

// transferkioskSchema returns the Typesense schema for the shared transferkiosk
// collection. Concept facets are faceted string[]; scalar facets are faceted
// strings; searchText/content are the searchable bodies. Envelope fields carry
// eventKind so the two kinds are distinguishable in a query.
func transferkioskSchema(name string) typesense30142.CollectionSchema {
	str := func(n string, facet bool) typesense30142.Field {
		return typesense30142.Field{Name: n, Type: "string", Facet: facet, Optional: true}
	}
	strArr := func(n string) typesense30142.Field {
		return typesense30142.Field{Name: n, Type: "string[]", Facet: true, Optional: true}
	}
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "type", Type: "string", Facet: true},
			{Name: "name", Type: "string"},
			str("description", false),
			str("searchText", false),
			str("content", false),
			strArr("partOf"),
			strArr("about"),
			strArr("audience"),
			strArr("activity"),
			strArr("actionField"),
			strArr("actionScope"),
			strArr("studyModel"),
			strArr("objective"),
			strArr("transferability"),
			str("status", true),
			str("funderName", true),
			str("funderProgram", true),
			str("hostName", true),
			str("hostBundesland", true),
			str("startDate", true),
			str("endDate", true),
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}
