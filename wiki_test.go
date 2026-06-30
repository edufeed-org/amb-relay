package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestNostrToWiki(t *testing.T) {
	evt := nostr.Event{
		Kind:      30818,
		PubKey:    nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000003"),
		CreatedAt: 1700000600,
		Tags: nostr.Tags{
			{"d", "fractions"},
			{"title", "Fractions"},
			{"summary", "An encyclopedia entry."},
		},
		Content: "Fractions represent parts of a whole.",
	}
	evt.ID = evt.GetID()

	doc, err := nostrToWiki(&evt)
	if err != nil {
		t.Fatalf("nostrToWiki: %v", err)
	}
	if doc.ID != evt.PubKey.Hex()+":fractions" {
		t.Errorf("ID = %q", doc.ID)
	}
	if doc.D != "fractions" {
		t.Errorf("D = %q", doc.D)
	}
	if doc.Title != "Fractions" {
		t.Errorf("Title = %q", doc.Title)
	}
	if doc.Summary != "An encyclopedia entry." {
		t.Errorf("Summary = %q", doc.Summary)
	}
	if doc.Content != evt.Content {
		t.Errorf("Content not carried")
	}
	if doc.EventKind != 30818 {
		t.Errorf("EventKind = %d", doc.EventKind)
	}
	if doc.EventID != evt.ID.Hex() {
		t.Errorf("EventID = %q", doc.EventID)
	}
}

// Wiki title is optional (NIP-54): a d-only event projects fine.
func TestNostrToWikiTitleOptional(t *testing.T) {
	evt := nostr.Event{
		Kind:    30818,
		PubKey:  nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000003"),
		Tags:    nostr.Tags{{"d", "no-title"}},
		Content: "body",
	}
	evt.ID = evt.GetID()
	doc, err := nostrToWiki(&evt)
	if err != nil {
		t.Fatalf("nostrToWiki: %v", err)
	}
	if doc.Title != "" {
		t.Errorf("Title = %q, want empty", doc.Title)
	}
}

func TestNostrToWikiMissingDTag(t *testing.T) {
	evt := nostr.Event{Kind: 30818, Tags: nostr.Tags{{"title", "x"}}}
	if _, err := nostrToWiki(&evt); err == nil {
		t.Fatal("expected error for missing d tag")
	}
}

func TestWikiSchemaFields(t *testing.T) {
	s := wikiSchema("wiki_30818")
	if s.Name != "wiki_30818" {
		t.Errorf("Name = %q", s.Name)
	}
	want := map[string]bool{
		"id": true, "d": true, "title": true, "summary": true, "content": true,
		"eventID": true, "eventKind": true, "eventPubKey": true,
		"eventCreatedAt": true, "eventRaw": true,
	}
	got := map[string]bool{}
	for _, f := range s.Fields {
		got[f.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("schema missing field %q", name)
		}
	}
}

func TestWikiEmbedText(t *testing.T) {
	doc := &WikiDocument{Title: "T", Summary: "S", Content: "C"}
	if got := doc.EmbedText(); got != "T S C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestWikiEmbedTextSkipsEmpty(t *testing.T) {
	doc := &WikiDocument{Content: "C"} // title + summary empty
	if got := doc.EmbedText(); got != "C" {
		t.Errorf("EmbedText = %q", got)
	}
}

func TestWikiSetEmbedding(t *testing.T) {
	doc := &WikiDocument{}
	doc.SetEmbedding([]float32{7})
	if len(doc.Embedding) != 1 || doc.Embedding[0] != 7 {
		t.Errorf("Embedding = %v", doc.Embedding)
	}
}

func TestWikiSchemaHasEmbedding(t *testing.T) {
	for _, f := range wikiSchema("wiki_30818").Fields {
		if f.Name == "embedding" {
			if f.Type != "float[]" || f.NumDim != 768 || f.VecDistMetric != "cosine" || !f.Optional {
				t.Errorf("embedding field = %+v", f)
			}
			return
		}
	}
	t.Fatal("wiki schema missing embedding field")
}
