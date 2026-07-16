package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func pubEvent(kind nostr.Kind, tags nostr.Tags, content string) *nostr.Event {
	e := &nostr.Event{Kind: kind, Tags: tags, Content: content}
	e.PubKey = nostr.PubKey{1, 2, 3} // deterministic non-zero pubkey for doc ids
	return e
}

func TestValidatePublication(t *testing.T) {
	cases := []struct {
		name    string
		event   *nostr.Event
		reject  bool
		msgPart string
	}{
		{"valid 30040", pubEvent(30040, nostr.Tags{{"d", "a1b2c3d4"}, {"title", "Effects of X on Y"}}, ""), false, ""},
		{"valid 30041", pubEvent(30041, nostr.Tags{{"d", "sec-1"}, {"title", "Introduction"}}, "Section body text."), false, ""},
		{"30040 missing d", pubEvent(30040, nostr.Tags{{"title", "T"}}, ""), true, "'d' tag"},
		{"30040 missing title", pubEvent(30040, nostr.Tags{{"d", "x"}}, ""), true, "'title' tag"},
		{"30041 missing title", pubEvent(30041, nostr.Tags{{"d", "x"}}, "body"), true, "'title' tag"},
		{"30040 non-empty content", pubEvent(30040, nostr.Tags{{"d", "x"}, {"title", "T"}}, "not allowed"), true, "empty content"},
		{"30041 non-empty content ok", pubEvent(30041, nostr.Tags{{"d", "x"}, {"title", "T"}}, "chapter text"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reject, msg := validatePublication(*tc.event)
			if reject != tc.reject {
				t.Fatalf("reject = %v, want %v (msg %q)", reject, tc.reject, msg)
			}
			if tc.reject && !strings.Contains(msg, tc.msgPart) {
				t.Errorf("msg %q missing %q", msg, tc.msgPart)
			}
		})
	}
}

func TestNostrToPublication30040(t *testing.T) {
	// Fixture mirrors edufeed-app buildPublicationTags output (publicationTags.js).
	ev := pubEvent(30040, nostr.Tags{
		{"d", "a1b2c3d4"},
		{"title", "Effects of X on Y"},
		{"type", "academic"},
		{"author", "Jane Doe"},
		{"author", "John Smith"},
		{"i", "doi:10.1234/abcd.5678"},
		{"source", "https://journal.example/article/123"},
		{"published_on", "2025-03-14"},
		{"published_by", "Journal of Examples"},
		{"summary", "We investigate the effects."},
		{"image", "https://img.example/cover.png"},
		{"t", "open education"},
		{"creator:type", "Person"},
		{"creator:name", "Jane Doe"},
		{"creator:honorificPrefix", "Dr."},
		{"creator:affiliation:name", "Example University"},
		{"creator:id", "0000-0002-1825-0097"},
		{"about:id", "https://w3id.org/kim/hochschulfaechersystematik/n123"},
		{"about:prefLabel:de", "Informatik"},
		{"inLanguage", "de"},
		{"license:id", "https://creativecommons.org/licenses/by/4.0/"},
		{"encoding:contentUrl", "https://files.example/article.pdf"},
		{"encoding:encodingFormat", "application/pdf"},
		{"p", strings.Repeat("aa", 32), "wss://relay.example", "creator"},
		{"a", "30041:" + strings.Repeat("bb", 32) + ":sec-1", "wss://relay.example"},
		{"a", "30040:" + strings.Repeat("bb", 32) + ":nested-pub", "wss://relay.example"},
		{"h", strings.Repeat("cc", 32)},
	}, "")

	doc, err := nostrToPublication(ev)
	if err != nil {
		t.Fatalf("nostrToPublication: %v", err)
	}
	if doc.D != "a1b2c3d4" || doc.Title != "Effects of X on Y" || doc.Type != "academic" {
		t.Errorf("d/title/type = %q/%q/%q", doc.D, doc.Title, doc.Type)
	}
	if doc.EventKind != 30040 {
		t.Errorf("EventKind = %d", doc.EventKind)
	}
	// Doc id folds the kind (shared two-kind collection).
	if !strings.HasPrefix(doc.ID, "30040:") {
		t.Errorf("ID = %q, want 30040: prefix", doc.ID)
	}
	if len(doc.Author) != 2 || doc.Author[0] != "Jane Doe" {
		t.Errorf("Author = %v", doc.Author)
	}
	if doc.Doi != "10.1234/abcd.5678" {
		t.Errorf("Doi = %q (must strip doi: prefix)", doc.Doi)
	}
	if doc.PublishedOn != "2025-03-14" || doc.PublishedBy != "Journal of Examples" {
		t.Errorf("published = %q/%q", doc.PublishedOn, doc.PublishedBy)
	}
	if doc.Summary != "We investigate the effects." {
		t.Errorf("Summary = %q", doc.Summary)
	}
	if len(doc.Keywords) != 1 || doc.Keywords[0] != "open education" {
		t.Errorf("Keywords = %v", doc.Keywords)
	}
	if len(doc.About) != 1 || doc.About[0] != "https://w3id.org/kim/hochschulfaechersystematik/n123" {
		t.Errorf("About = %v (facet holds concept ids)", doc.About)
	}
	if len(doc.CreatorName) != 1 || doc.CreatorName[0] != "Jane Doe" {
		t.Errorf("CreatorName = %v", doc.CreatorName)
	}
	if doc.InLanguage != "de" || doc.License != "https://creativecommons.org/licenses/by/4.0/" {
		t.Errorf("inLanguage/license = %q/%q", doc.InLanguage, doc.License)
	}
	if doc.EncodingURL != "https://files.example/article.pdf" {
		t.Errorf("EncodingURL = %q", doc.EncodingURL)
	}
	// Sections keep ALL a-tag coords regardless of referenced kind (Reihe case:
	// nested 30040 children must be kept alongside 30041 sections).
	if len(doc.Sections) != 2 || !strings.HasPrefix(doc.Sections[1], "30040:") {
		t.Errorf("Sections = %v", doc.Sections)
	}
	if len(doc.Community) != 1 {
		t.Errorf("Community = %v (h tag must fold into envelope)", doc.Community)
	}
	// searchText folds names, affiliations, keywords, concept labels, venue.
	for _, want := range []string{"Jane Doe", "John Smith", "Example University", "open education", "Informatik", "Journal of Examples"} {
		if !strings.Contains(doc.SearchText, want) {
			t.Errorf("SearchText missing %q: %q", want, doc.SearchText)
		}
	}
	// EmbedText must carry title + abstract + folded names/labels.
	for _, want := range []string{"Effects of X on Y", "We investigate the effects.", "Informatik"} {
		if !strings.Contains(doc.EmbedText(), want) {
			t.Errorf("EmbedText missing %q", want)
		}
	}
}

func TestNostrToPublication30041(t *testing.T) {
	ev := pubEvent(30041, nostr.Tags{
		{"d", "aesops-fables-the-farmer-and-the-snake"},
		{"title", "The Farmer and The Snake"},
	}, "ONE WINTER a Farmer found a Snake stiff and frozen with cold.")

	doc, err := nostrToPublication(ev)
	if err != nil {
		t.Fatalf("nostrToPublication: %v", err)
	}
	if !strings.HasPrefix(doc.ID, "30041:") {
		t.Errorf("ID = %q, want 30041: prefix", doc.ID)
	}
	if doc.Content != "ONE WINTER a Farmer found a Snake stiff and frozen with cold." {
		t.Errorf("Content = %q (30041 body must be BM25-searchable at write time)", doc.Content)
	}
	if !strings.Contains(doc.EmbedText(), "ONE WINTER a Farmer") {
		t.Errorf("EmbedText missing section body")
	}
}

func TestNostrToPublicationMissingD(t *testing.T) {
	ev := pubEvent(30040, nostr.Tags{{"title", "T"}}, "")
	if _, err := nostrToPublication(ev); err == nil {
		t.Fatal("expected error for missing d tag")
	}
}

func TestPublicationsSchemaFields(t *testing.T) {
	schema := publicationsSchema("publications")
	fields := map[string]bool{}
	for _, f := range schema.Fields {
		fields[f.Name] = true
	}
	// content_fetched_at / content_status are REQUIRED by PatchContent /
	// ClearContent (setcontent kind-routing, Task 4).
	for _, name := range []string{"id", "d", "type", "title", "summary", "searchText", "content",
		"author", "creatorName", "doi", "source", "published_on", "published_by",
		"keywords", "about", "inLanguage", "license", "sections", "partOf", "embedding",
		"content_fetched_at", "content_status", "eventKind", "community"} {
		if !fields[name] {
			t.Errorf("schema missing field %q", name)
		}
	}
}

// TestPublicationATagRouting: the 4th a-tag element is ambiguous across
// specs — NKBIP-01 uses it for an OPTIONAL EVENT ID (64-hex), NIP-DIDACTIC
// for word markers. isPartOf/isOutputOf → partOf; bare or event-id-hinted →
// sections; any other word marker (vocab refs, `documents`) → neither.
func TestPublicationATagRouting(t *testing.T) {
	evID := strings.Repeat("ab", 32)
	ev := pubEvent(30040, nostr.Tags{
		{"d", "tk-p101-pub7"},
		{"title", "Paper"},
		{"a", "30143:pk:proj-1", "wss://r", "isOutputOf"},
		{"a", "30144:pk:m-1", "wss://r", "isPartOf"},
		{"a", "30041:pk:sec-1", "wss://r"},
		{"a", "30041:pk:sec-2", "wss://r", evID},
		{"a", "39738:pk:tk-publikationsart/5", "wss://r", "publicationType"},
	}, "")
	doc, err := nostrToPublication(ev)
	if err != nil {
		t.Fatalf("nostrToPublication: %v", err)
	}
	if len(doc.PartOf) != 2 || doc.PartOf[0] != "30143:pk:proj-1" || doc.PartOf[1] != "30144:pk:m-1" {
		t.Errorf("PartOf = %v", doc.PartOf)
	}
	if len(doc.Sections) != 2 || doc.Sections[0] != "30041:pk:sec-1" || doc.Sections[1] != "30041:pk:sec-2" {
		t.Errorf("Sections = %v (must keep bare and event-id-hinted a-tags only)", doc.Sections)
	}
}

func TestPublicationEditorInSearchText(t *testing.T) {
	ev := pubEvent(30040, nostr.Tags{
		{"d", "tk-p101-pub8"},
		{"title", "Paper"},
		{"editor:name", "Eckhard Liebscher"},
		{"editor:type", "Person"},
	}, "")
	doc, err := nostrToPublication(ev)
	if err != nil {
		t.Fatalf("nostrToPublication: %v", err)
	}
	if !strings.Contains(doc.SearchText, "Eckhard Liebscher") {
		t.Errorf("SearchText missing editor name: %q", doc.SearchText)
	}
}

func TestIsHex64(t *testing.T) {
	if !isHex64(strings.Repeat("ab", 32)) {
		t.Error("64-hex must pass")
	}
	for _, s := range []string{"isOutputOf", "publicationType", "", strings.Repeat("ab", 31), strings.Repeat("zz", 32)} {
		if isHex64(s) {
			t.Errorf("isHex64(%q) = true", s)
		}
	}
}

func TestSetPublicationContentPatchesPublicationsDoc(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(200)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	ev := *pubEvent(30040, nostr.Tags{{"d", "a1b2c3d4"}, {"title", "T"}}, "")
	entry := ContentEntry{Text: "extracted pdf text", FetchedAt: 1700000000, Status: "fetched"}

	ts := &typesense30142.TSBackend{Host: srv.URL, ApiKey: "k", CollectionName: "publications"}
	if err := setPublicationContent(ts, cs, ev, entry); err != nil {
		t.Fatalf("setPublicationContent: %v", err)
	}

	// PATCH must target the kind-folded doc id, path-escaped.
	wantDocID := docIDFor(30040, ev.PubKey.Hex(), "a1b2c3d4")
	if !strings.Contains(gotPath, url.PathEscape(wantDocID)) {
		t.Errorf("patch path %q missing doc id %q", gotPath, wantDocID)
	}
	if gotBody["content"] != "extracted pdf text" || gotBody["content_status"] != "fetched" {
		t.Errorf("patch body = %v", gotBody)
	}
	// ContentStore row persisted for reindex replay.
	if got, ok, _ := cs.Get(ev.ID.Hex()); !ok || got.Text != "extracted pdf text" {
		t.Errorf("content store row = %+v ok=%v", got, ok)
	}
}
