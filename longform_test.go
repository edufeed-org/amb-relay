package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func TestNostrToLongform(t *testing.T) {
	evt := nostr.Event{
		Kind:      30023,
		PubKey:    nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"),
		CreatedAt: 1700000500,
		Tags: nostr.Tags{
			{"d", "my-article"},
			{"title", "Understanding Fractions"},
			{"summary", "A long-form intro."},
			{"published_at", "1699990000"},
			{"t", "math"},
			{"t", "education"},
		},
		Content: "# Fractions\n\nA fraction represents part of a whole.",
	}
	evt.ID = evt.GetID()

	doc, err := nostrToLongform(&evt)
	if err != nil {
		t.Fatalf("nostrToLongform: %v", err)
	}
	if doc.ID != evt.PubKey.Hex()+":my-article" {
		t.Errorf("ID = %q", doc.ID)
	}
	if doc.Title != "Understanding Fractions" {
		t.Errorf("Title = %q", doc.Title)
	}
	if doc.Summary != "A long-form intro." {
		t.Errorf("Summary = %q", doc.Summary)
	}
	if doc.PublishedAt != 1699990000 {
		t.Errorf("PublishedAt = %d", doc.PublishedAt)
	}
	if len(doc.Topics) != 2 || doc.Topics[0] != "math" {
		t.Errorf("Topics = %v", doc.Topics)
	}
	if doc.EventKind != 30023 {
		t.Errorf("EventKind = %d", doc.EventKind)
	}
	if doc.EventID != evt.ID.Hex() {
		t.Errorf("EventID = %q", doc.EventID)
	}
	if doc.Content != evt.Content {
		t.Errorf("Content not carried")
	}
}

func TestNostrToLongformMissingDTag(t *testing.T) {
	evt := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"title", "x"}}}
	if _, err := nostrToLongform(&evt); err == nil {
		t.Fatal("expected error for missing d tag")
	}
}

func TestUpsertLongformPostsUpsert(t *testing.T) {
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
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "longform_30023", ApiKey: "k"}
	doc := &LongformDocument{ID: "p:d", Title: "x"}
	if err := upsertLongform(ts, doc); err != nil {
		t.Fatalf("upsertLongform: %v", err)
	}
	if !strings.Contains(gotQuery, "/collections/longform_30023/documents/import") || !strings.Contains(gotQuery, "action=upsert") {
		t.Errorf("query = %q", gotQuery)
	}
	if gotKey != "k" {
		t.Errorf("api key header = %q", gotKey)
	}
	if !strings.Contains(string(gotBody), `"title":"x"`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestLongformSchemaFields(t *testing.T) {
	s := longformSchema("longform_30023")
	if s.Name != "longform_30023" {
		t.Errorf("Name = %q", s.Name)
	}
	want := map[string]bool{
		"id": true, "d": true, "title": true, "summary": true, "content": true,
		"published_at": true, "t": true, "eventID": true, "eventKind": true,
		"eventPubKey": true, "eventCreatedAt": true, "eventRaw": true,
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
