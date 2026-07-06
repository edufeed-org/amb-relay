package main

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func tkEvent(kind nostr.Kind, tags nostr.Tags, content string) *nostr.Event {
	e := &nostr.Event{Kind: kind, Tags: tags, Content: content}
	// deterministic non-zero pubkey for GenerateDocumentID
	e.PubKey = nostr.PubKey{1, 2, 3}
	return e
}

func TestNostrToTransferkioskProjekt(t *testing.T) {
	ev := tkEvent(30143, nostr.Tags{
		{"d", "https://transferkiosk.net/p/101205"},
		{"type", "Project"},
		{"name", "Edu-Lab"},
		{"description", "Aufbau eines Edu-Labs."},
		{"status", "veroeffentlicht"},
		{"startDate", "2025-04-01"},
		{"endDate", "2027-03-31"},
		{"funder:name", "Freiraum 2025"},
		{"funder:program:name", "Freiraum"},
		{"host:name", "FernUniversität Hagen"},
		{"host:location:name", "Nordrhein-Westfalen"},
		{"about:id", "https://transferkiosk.net/vocab/tk-fachbereiche/1"},
		{"about:prefLabel:de", "Bildungswissenschaft"},
		{"about:type", "Concept"},
		{"objective:prefLabel:de", "Aktivierung von Studierenden"},
		{"objective:type", "Concept"},
	}, "Aufbau eines Edu-Labs.")

	doc, err := nostrToTransferkiosk(ev)
	if err != nil {
		t.Fatalf("nostrToTransferkiosk: %v", err)
	}
	if doc.D != "https://transferkiosk.net/p/101205" {
		t.Errorf("D = %q", doc.D)
	}
	if doc.Type != "Project" || doc.Name != "Edu-Lab" {
		t.Errorf("type/name = %q/%q", doc.Type, doc.Name)
	}
	if doc.EventKind != 30143 {
		t.Errorf("EventKind = %d", doc.EventKind)
	}
	if doc.Status != "veroeffentlicht" || doc.StartDate != "2025-04-01" || doc.EndDate != "2027-03-31" {
		t.Errorf("scalar facets wrong: %+v", doc)
	}
	if doc.FunderName != "Freiraum 2025" || doc.HostName != "FernUniversität Hagen" || doc.HostBundesland != "Nordrhein-Westfalen" {
		t.Errorf("funder/host wrong: %+v", doc)
	}
	if len(doc.About) != 1 || doc.About[0] != "Bildungswissenschaft" {
		t.Errorf("About = %v", doc.About)
	}
	if len(doc.Objective) != 1 || doc.Objective[0] != "Aktivierung von Studierenden" {
		t.Errorf("Objective = %v", doc.Objective)
	}
	// CRITICAL: concept labels are folded into searchText for semantic/BM25 reach.
	if !strings.Contains(doc.SearchText, "Aktivierung von Studierenden") ||
		!strings.Contains(doc.SearchText, "Bildungswissenschaft") {
		t.Errorf("SearchText missing folded labels: %q", doc.SearchText)
	}
}

func TestNostrToTransferkioskMassnahme(t *testing.T) {
	ev := tkEvent(30144, nostr.Tags{
		{"d", "https://transferkiosk.net/p/101560/act/100613"},
		{"type", "TeachingMeasure"},
		{"name", "Digitale Prüfungsumgebung"},
		{"description", "Kurzbeschreibung."},
		{"a", "30143:abc:https://transferkiosk.net/p/101560", "wss://relay.edufeed.org", "isPartOf"},
		{"audience:prefLabel:de", "Studierende"},
		{"audience:type", "Concept"},
		{"activity:prefLabel:de", "Prüfen"},
		{"activity:type", "Concept"},
		{"challenge", "Sichere Prüfungsumgebung schaffen."},
		{"outcome", "Hybride Prüfungen erprobt."},
		{"ext:tk:qualityDevelopment:prefLabel:de", "Formative Evaluation"},
		{"ext:tk:qualityDevelopment:type", "Concept"},
	}, "Body text.")

	doc, err := nostrToTransferkiosk(ev)
	if err != nil {
		t.Fatalf("nostrToTransferkiosk: %v", err)
	}
	if len(doc.PartOf) != 1 || doc.PartOf[0] != "30143:abc:https://transferkiosk.net/p/101560" {
		t.Errorf("PartOf = %v", doc.PartOf)
	}
	if len(doc.Audience) != 1 || doc.Audience[0] != "Studierende" {
		t.Errorf("Audience = %v", doc.Audience)
	}
	if len(doc.Activity) != 1 || doc.Activity[0] != "Prüfen" {
		t.Errorf("Activity = %v", doc.Activity)
	}
	// Narrative tags AND ext labels fold into searchText (ext has no named slice).
	for _, want := range []string{"Sichere Prüfungsumgebung", "Hybride Prüfungen", "Formative Evaluation", "Studierende"} {
		if !strings.Contains(doc.SearchText, want) {
			t.Errorf("SearchText missing %q: %q", want, doc.SearchText)
		}
	}
}

func TestNostrToTransferkioskPublikation(t *testing.T) {
	ev := tkEvent(30145, nostr.Tags{
		{"d", "https://doi.org/10.21240/zfhe/18-03/04"},
		{"type", "ScholarlyArticle"},
		{"name", "Wissenschaftsgeleitete Wirkungsreflexion"},
		{"a", "30143:abc:https://transferkiosk.net/p/101498", "wss://relay.edufeed.org", "isOutputOf"},
		{"publisher:name", "ZFHE"},
		{"author:name", "Benjamin Ditzel"},
		{"author:type", "Person"},
		{"publicationType:prefLabel:de", "Zeitschriftenartikel"},
		{"publicationType:type", "Concept"},
		{"datePublished", "2023-01-01"},
	}, "")

	doc, err := nostrToTransferkiosk(ev)
	if err != nil {
		t.Fatalf("nostrToTransferkiosk: %v", err)
	}
	if len(doc.PartOf) != 1 || doc.PartOf[0] != "30143:abc:https://transferkiosk.net/p/101498" {
		t.Errorf("PartOf = %v", doc.PartOf)
	}
	if doc.Publisher != "ZFHE" || len(doc.Author) != 1 || doc.Author[0] != "Benjamin Ditzel" {
		t.Errorf("publisher/author wrong: %+v", doc)
	}
	if len(doc.PublicationType) != 1 || doc.PublicationType[0] != "Zeitschriftenartikel" {
		t.Errorf("PublicationType = %v", doc.PublicationType)
	}
	if doc.DatePublished != "2023-01-01" {
		t.Errorf("DatePublished = %q", doc.DatePublished)
	}
}

func TestNostrToTransferkioskMissingDRejected(t *testing.T) {
	ev := tkEvent(30143, nostr.Tags{{"type", "Project"}, {"name", "x"}}, "")
	if _, err := nostrToTransferkiosk(ev); err == nil {
		t.Fatal("expected error for missing d tag")
	}
}

func TestTransferkioskSchema(t *testing.T) {
	s := transferkioskSchema("transferkiosk")
	if s.Name != "transferkiosk" || s.DefaultSortingField != "eventCreatedAt" {
		t.Fatalf("schema header wrong: %+v", s)
	}
	byName := map[string]typesense30142.Field{}
	for _, f := range s.Fields {
		byName[f.Name] = f
	}
	emb, ok := byName["embedding"]
	if !ok || emb.Type != "float[]" || emb.NumDim != 768 || emb.VecDistMetric != "cosine" || !emb.Optional {
		t.Errorf("embedding field wrong: %+v", emb)
	}
	for _, name := range []string{"id", "d", "type", "name", "searchText", "about", "audience", "status", "partOf", "eventKind"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing field %q", name)
		}
	}
	if !byName["about"].Facet || byName["about"].Type != "string[]" {
		t.Errorf("about should be a faceted string[]: %+v", byName["about"])
	}
	if !byName["partOf"].Facet || byName["partOf"].Type != "string[]" {
		t.Errorf("partOf should be a faceted string[]: %+v", byName["partOf"])
	}
}

// Multiple isPartOf/isOutputOf links must all survive projection — the earlier
// scalar PartOf silently kept only the last one.
func TestTransferkioskMultiplePartOfLinks(t *testing.T) {
	doc, err := nostrToTransferkiosk(tkEvent(30145, nostr.Tags{
		{"d", "pub-1"},
		{"name", "Paper"},
		{"a", "30143:abc:proj-1", "", "isOutputOf"},
		{"a", "30143:abc:proj-2", "", "isOutputOf"},
	}, ""))
	if err != nil {
		t.Fatalf("nostrToTransferkiosk: %v", err)
	}
	if len(doc.PartOf) != 2 || doc.PartOf[0] != "30143:abc:proj-1" || doc.PartOf[1] != "30143:abc:proj-2" {
		t.Errorf("PartOf = %v", doc.PartOf)
	}
}

// All three kinds share one collection, so the doc id must fold in the kind:
// a Projekt and a Maßnahme with the same (pubkey, d) must not overwrite each other.
func TestTransferkioskDocIDsDistinctAcrossKinds(t *testing.T) {
	tags := nostr.Tags{{"d", "same-slug"}, {"name", "Same Name"}}
	projekt, err := nostrToTransferkiosk(tkEvent(30143, tags, ""))
	if err != nil {
		t.Fatalf("projekt: %v", err)
	}
	massnahme, err := nostrToTransferkiosk(tkEvent(30144, tags, ""))
	if err != nil {
		t.Fatalf("massnahme: %v", err)
	}
	if projekt.ID == massnahme.ID {
		t.Fatalf("doc ID collision across kinds: %q", projekt.ID)
	}
	want := "30143:" + projekt.EventPubKey + ":same-slug"
	if projekt.ID != want {
		t.Errorf("projekt ID = %q, want %q", projekt.ID, want)
	}
}
