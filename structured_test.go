package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func TestNewStructuredEnvelope(t *testing.T) {
	evt := nostr.Event{
		Kind:      30023,
		PubKey:    nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"),
		CreatedAt: 1700000500,
		Tags:      nostr.Tags{{"d", "x"}},
		Content:   "body",
	}
	evt.ID = evt.GetID()
	env, err := newStructuredEnvelope(&evt)
	if err != nil {
		t.Fatalf("newStructuredEnvelope: %v", err)
	}
	if env.EventKind != 30023 {
		t.Errorf("EventKind = %d", env.EventKind)
	}
	if env.EventID != evt.ID.Hex() {
		t.Errorf("EventID = %q", env.EventID)
	}
	if env.EventPubKey != evt.PubKey.Hex() {
		t.Errorf("EventPubKey = %q", env.EventPubKey)
	}
	if env.EventCreatedAt != 1700000500 {
		t.Errorf("EventCreatedAt = %d", env.EventCreatedAt)
	}
	if !strings.Contains(env.EventRaw, `"content":"body"`) {
		t.Errorf("EventRaw missing content: %s", env.EventRaw)
	}
}

func TestStructuredEnvelopeFields(t *testing.T) {
	want := map[string]bool{"eventID": true, "eventKind": true, "eventPubKey": true, "eventCreatedAt": true, "eventRaw": true}
	got := map[string]bool{}
	for _, f := range structuredEnvelopeFields() {
		got[f.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing envelope field %q", name)
		}
	}
}

func TestUpsertStructuredDocPostsUpsert(t *testing.T) {
	var gotQuery, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Path + "?" + r.URL.RawQuery
		gotKey = r.Header.Get("X-TYPESENSE-API-KEY")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "c", ApiKey: "k"}
	if err := upsertStructuredDoc(ts, map[string]string{"id": "p:d", "title": "x"}); err != nil {
		t.Fatalf("upsertStructuredDoc: %v", err)
	}
	if !strings.Contains(gotQuery, "/collections/c/documents/import") || !strings.Contains(gotQuery, "action=upsert") {
		t.Errorf("query = %q", gotQuery)
	}
	if gotKey != "k" {
		t.Errorf("api key = %q", gotKey)
	}
	if !strings.Contains(string(gotBody), `"title":"x"`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestNewStructuredEnvelope_Community(t *testing.T) {
	evt := nostr.Event{
		Kind:    30023,
		Content: "body",
		Tags: nostr.Tags{
			{"d", "x"},
			{"h", "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083"},
		},
	}
	env, err := newStructuredEnvelope(&evt)
	if err != nil {
		t.Fatalf("newStructuredEnvelope: %v", err)
	}
	if len(env.Community) != 1 || env.Community[0] != "660d8c78651f70487ec9b8ddc283e29cf2561693dda3ba246d3fd3c08dbb7083" {
		t.Errorf("Community = %v", env.Community)
	}
}

func TestStructuredEnvelopeFields_Community(t *testing.T) {
	found := false
	for _, f := range structuredEnvelopeFields() {
		if f.Name == "community" {
			found = true
			if f.Type != "string[]" || !f.Optional {
				t.Errorf("community field = %+v", f)
			}
		}
	}
	if !found {
		t.Fatal("community field missing from structuredEnvelopeFields")
	}
}

// Locks the long-form document JSON so the structured-envelope refactor
// stays byte-identical (embedded-struct field promotion preserves order).
func TestLongformDocumentJSONStable(t *testing.T) {
	doc := &LongformDocument{
		ID: "p:d", D: "d", Title: "T", Summary: "S", Content: "C",
		PublishedAt: 5, Topics: []string{"a"}, Image: "img",
		structuredEnvelope: structuredEnvelope{
			EventID: "eid", EventKind: 30023, EventPubKey: "pk", EventCreatedAt: 9, EventRaw: "{}",
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"p:d","d":"d","title":"T","summary":"S","content":"C","published_at":5,"t":["a"],"image":"img","eventID":"eid","eventKind":30023,"eventPubKey":"pk","eventCreatedAt":9,"eventRaw":"{}"}`
	if string(b) != want {
		t.Errorf("JSON drift:\n got=%s\nwant=%s", b, want)
	}
}

// fakeEmbedder returns a fixed vector and records the input role.
type fakeEmbedder struct {
	vec      []float32
	gotInput typesense30142.EmbedInput
	gotTexts []string
	err      error
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string, input typesense30142.EmbedInput) ([][]float32, error) {
	f.gotInput = input
	f.gotTexts = texts
	if f.err != nil {
		return nil, f.err
	}
	return [][]float32{f.vec}, nil
}

// fakeDoc implements embeddable for helper tests.
type fakeDoc struct {
	text string
	vec  []float32
}

func (d *fakeDoc) EmbedText() string        { return d.text }
func (d *fakeDoc) SetEmbedding(v []float32) { d.vec = v }

func TestEmbedAndAttach_SetsVector(t *testing.T) {
	fe := &fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	ts := &typesense30142.TSBackend{Embedder: fe}
	doc := &fakeDoc{text: "title summary content"}
	if err := embedAndAttach(ts, doc); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if fe.gotInput != typesense30142.EmbedPassage {
		t.Errorf("input role = %q, want passage", fe.gotInput)
	}
	if len(fe.gotTexts) != 1 || fe.gotTexts[0] != "title summary content" {
		t.Errorf("embed texts = %v", fe.gotTexts)
	}
	if len(doc.vec) != 3 || doc.vec[0] != 0.1 {
		t.Errorf("vector = %v", doc.vec)
	}
}

func TestEmbedAndAttach_NilEmbedderNoop(t *testing.T) {
	ts := &typesense30142.TSBackend{} // Embedder nil
	doc := &fakeDoc{text: "x"}
	if err := embedAndAttach(ts, doc); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if doc.vec != nil {
		t.Errorf("vector set despite nil embedder: %v", doc.vec)
	}
}

func TestEmbedAndAttach_NonEmbeddableNoop(t *testing.T) {
	fe := &fakeEmbedder{vec: []float32{1}}
	ts := &typesense30142.TSBackend{Embedder: fe}
	// a plain map does not implement embeddable
	if err := embedAndAttach(ts, map[string]string{"id": "x"}); err != nil {
		t.Fatalf("embedAndAttach: %v", err)
	}
	if fe.gotTexts != nil {
		t.Errorf("embedder called for non-embeddable doc")
	}
}

func TestReprojectStructured_EmbedsWhenEmbedderSet(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	fe := &fakeEmbedder{vec: []float32{0.5, 0.6}}
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "c", ApiKey: "k", Embedder: fe}
	evt := nostr.Event{Kind: 30818, Content: "body", Tags: nostr.Tags{{"d", "x"}, {"title", "T"}}}
	if err := reprojectStructured(ts, evt, nostrToWiki); err != nil {
		t.Fatalf("reprojectStructured: %v", err)
	}
	if !strings.Contains(string(gotBody), `"embedding":[0.5,0.6]`) {
		t.Errorf("upserted body missing embedding vector: %s", gotBody)
	}
	if fe.gotInput != typesense30142.EmbedPassage {
		t.Errorf("input role = %q, want passage", fe.gotInput)
	}
}
