package main

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// PublicationDocument is the Typesense shape for the two NKBIP-01 kinds —
// publication index (30040) and publication content/section (30041). Both
// share one collection, distinguished by EventKind. 30040 carries the
// bibliographic facets; 30041 carries the section body in Content. The 30040
// Content field is empty at write time (NKBIP-01 MUST) and is later patched
// with the article PDF's extracted text via kind-routed NIP-86 setcontent.
//
// SearchText folds author/creator names, affiliations, keywords, concept
// labels, and the venue so topical NIP-50 search reaches them; title and
// summary are separate search fields.
type PublicationDocument struct {
	ID      string `json:"id"`
	D       string `json:"d"`
	Type    string `json:"type,omitempty"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`

	SearchText string `json:"searchText,omitempty"`
	Content    string `json:"content,omitempty"`

	Author      []string `json:"author,omitempty"`
	CreatorName []string `json:"creatorName,omitempty"`
	Doi         string   `json:"doi,omitempty"`
	Source      string   `json:"source,omitempty"`
	PublishedOn string   `json:"published_on,omitempty"`
	PublishedBy string   `json:"published_by,omitempty"`
	Image       string   `json:"image,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	About       []string `json:"about,omitempty"`
	InLanguage  string   `json:"inLanguage,omitempty"`
	License     string   `json:"license,omitempty"`
	EncodingURL string   `json:"encodingUrl,omitempty"`

	// Sections holds every `a` tag coord in index order — 30041 sections,
	// nested 30040 indices (the "Reihe" case), or 30023/30818 leaves. Never
	// assume these are 30041.
	Sections []string `json:"sections,omitempty"`

	// PartOf holds a-tag coords with an isPartOf/isOutputOf marker — e.g. a
	// transferkiosk-born publication's parent-project link. Kept out of
	// Sections so the parts list stays purely the publication's content.
	PartOf []string `json:"partOf,omitempty"`

	Embedding []float32 `json:"embedding,omitempty"`
	structuredEnvelope
}

// nostrToPublication projects a kind-30040/30041 event into a
// PublicationDocument.
func nostrToPublication(event *nostr.Event) (*PublicationDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("publication event %s missing required 'd' tag", event.ID.Hex())
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	doc := &PublicationDocument{
		ID:                 docIDFor(event.Kind, event.PubKey.Hex(), dTag),
		D:                  dTag,
		Content:            event.Content,
		structuredEnvelope: env,
	}

	var fold []string // names, affiliations, keywords, labels, venue → SearchText

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		key, val := tag[0], tag[1]

		// Concept labels ("about:prefLabel:de", any language) fold into
		// SearchText; the about facet holds the concept ids for filtering.
		if strings.HasPrefix(key, "about:prefLabel:") {
			fold = append(fold, val)
			continue
		}

		switch key {
		case "type":
			doc.Type = val
		case "title":
			doc.Title = val
		case "summary":
			doc.Summary = val
		case "author":
			doc.Author = append(doc.Author, val)
			fold = append(fold, val)
		case "creator:name":
			doc.CreatorName = append(doc.CreatorName, val)
			fold = append(fold, val)
		case "creator:affiliation:name":
			fold = append(fold, val)
		case "editor:name":
			fold = append(fold, val)
		case "i":
			if doi, ok := strings.CutPrefix(val, "doi:"); ok && doc.Doi == "" {
				doc.Doi = doi
			}
		case "source":
			doc.Source = val
		case "published_on":
			doc.PublishedOn = val
		case "published_by":
			doc.PublishedBy = val
			fold = append(fold, val)
		case "image":
			doc.Image = val
		case "t":
			doc.Keywords = append(doc.Keywords, val)
			fold = append(fold, val)
		case "about:id":
			doc.About = append(doc.About, val)
		case "inLanguage":
			doc.InLanguage = val
		case "license:id":
			doc.License = val
		case "encoding:contentUrl":
			doc.EncodingURL = val
		case "a":
			marker := ""
			if len(tag) >= 4 {
				marker = tag[3]
			}
			switch {
			case marker == "isPartOf" || marker == "isOutputOf":
				doc.PartOf = append(doc.PartOf, val)
			case marker == "" || isHex64(marker):
				// NKBIP-01 puts an optional event id in position 3; bare or
				// event-id-hinted a-tags are the publication's parts. Any
				// other word marker (vocab concept refs, `documents`) is a
				// typed link — eventRaw only.
				doc.Sections = append(doc.Sections, val)
			}
		}
	}

	doc.SearchText = strings.Join(fold, " ")
	return doc, nil
}

// EmbedText returns the passage text to embed: title + abstract + folded
// names/labels, plus the section body for 30041 (whose other fields are
// sparse). embedAndAttach truncates to EmbedMaxLength.
func (d *PublicationDocument) EmbedText() string {
	parts := make([]string, 0, 4)
	for _, s := range []string{d.Title, d.Summary, d.SearchText, d.Content} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// SetEmbedding stores the computed dense vector.
func (d *PublicationDocument) SetEmbedding(v []float32) { d.Embedding = v }

// isHex64 reports whether s is a 64-char hex string — the shape of an
// NKBIP-01 optional event-id hint in an a-tag's 4th position, as opposed to
// a word marker like isOutputOf.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// storePublication projects and upserts a kind-30040/30041 event to the shared
// publications collection via the structured-collection helper.
func storePublication(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "publication", nostrToPublication)
}

// publicationsSchema returns the Typesense schema for the shared publications
// collection. content/content_fetched_at/content_status mirror the AMB
// collection's content triple so PatchContent/ClearContent (kind-routed
// setcontent) work against this collection unchanged.
func publicationsSchema(name string) typesense30142.CollectionSchema {
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
			str("type", true),
			{Name: "title", Type: "string"},
			str("summary", false),
			str("searchText", false),
			str("content", false),
			strArr("author"),
			strArr("creatorName"),
			str("doi", true),
			str("source", false),
			str("published_on", true),
			str("published_by", true),
			str("image", false),
			strArr("keywords"),
			strArr("about"),
			str("inLanguage", true),
			str("license", true),
			str("encodingUrl", false),
			strArr("sections"),
			strArr("partOf"),
			{Name: "content_fetched_at", Type: "int64", Optional: true},
			str("content_status", false),
			{Name: "embedding", Type: "float[]", NumDim: 768, VecDistMetric: "cosine", Optional: true},
		}, structuredEnvelopeFields()...),
	}
}

// setPublicationContent persists indexer-extracted fulltext for a kind-30040
// event and patches the publications doc directly. Direct PATCH is safe here
// (unlike the AMB path's buffered QueueContent): structured docs are upserted
// synchronously on write, so the doc exists before any setcontent arrives.
func setPublicationContent(ts *typesense30142.TSBackend, cs *ContentStore, event nostr.Event, entry ContentEntry) error {
	if err := cs.Put(event.ID.Hex(), entry); err != nil {
		return fmt.Errorf("content store put: %w", err)
	}
	docID := docIDFor(event.Kind, event.PubKey.Hex(), event.Tags.GetD())
	return PatchContent(ts.Host, ts.ApiKey, ts.CollectionName, docID, entry)
}

// clearPublicationContent removes stored fulltext for a kind-30040 event and
// clears the doc's content triple (refetchcontent path).
func clearPublicationContent(ts *typesense30142.TSBackend, cs *ContentStore, event nostr.Event) error {
	if err := cs.Delete(event.ID.Hex()); err != nil {
		return fmt.Errorf("content store delete: %w", err)
	}
	docID := docIDFor(event.Kind, event.PubKey.Hex(), event.Tags.GetD())
	return ClearContent(ts.Host, ts.ApiKey, ts.CollectionName, docID)
}
