package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

func TestShareCommunities(t *testing.T) {
	repost := nostr.Event{
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "abc"},
			{"k", "30142"},
			{"p", "AUTHOR_pubkey_not_a_community"},
			{"h", "comm1"},
			{"h", "comm2"},
		},
	}
	got := shareCommunities(&repost)
	if len(got) != 2 || got[0] != "comm1" || got[1] != "comm2" {
		t.Fatalf("kind-16 communities = %v, want [comm1 comm2] (p must NOT count)", got)
	}

	targeted := nostr.Event{
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "x"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
			{"h", "commB"},
			{"p", "commA"}, // duplicate must be deduped
		},
	}
	got = shareCommunities(&targeted)
	if len(got) != 2 || got[0] != "commA" || got[1] != "commB" {
		t.Fatalf("kind-30222 communities = %v, want [commA commB] (p counts, deduped)", got)
	}
}

func TestNostrToShare_Repost(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xab},
		Kind: 16,
		Tags: nostr.Tags{
			{"e", "EVENT_ID_REF"},
			{"a", "30142:auth:slug"},
			{"k", "30142"},
			{"p", "author"},
			{"h", "comm1"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	if doc.ID != evt.ID.Hex() {
		t.Errorf("kind-16 doc id = %q, want event id %q", doc.ID, evt.ID.Hex())
	}
	if len(doc.RefE) != 1 || doc.RefE[0] != "EVENT_ID_REF" {
		t.Errorf("RefE = %v", doc.RefE)
	}
	if len(doc.RefA) != 1 || doc.RefA[0] != "30142:auth:slug" {
		t.Errorf("RefA = %v", doc.RefA)
	}
	if doc.RefKind != "30142" {
		t.Errorf("RefKind = %q, want \"30142\"", doc.RefKind)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "comm1" {
		t.Errorf("Community = %v, want [comm1]", doc.Community)
	}
	if doc.EventKind != 16 {
		t.Errorf("EventKind = %d, want 16", doc.EventKind)
	}
}

func TestNostrToShare_TargetedPublicationDocID(t *testing.T) {
	evt := nostr.Event{
		ID:   nostr.ID{0xcd},
		Kind: 30222,
		Tags: nostr.Tags{
			{"d", "slug-1"},
			{"a", "30142:auth:slug"},
			{"p", "commA"},
		},
	}
	doc, err := nostrToShare(&evt)
	if err != nil {
		t.Fatalf("nostrToShare: %v", err)
	}
	want := nostrToShareDocID(t, evt.PubKey.Hex(), "slug-1")
	if doc.ID != want {
		t.Errorf("kind-30222 doc id = %q, want addressable id %q", doc.ID, want)
	}
	if len(doc.Community) != 1 || doc.Community[0] != "commA" {
		t.Errorf("Community = %v, want [commA] (from p)", doc.Community)
	}
}

func TestNostrToShare_TargetedPublicationMissingD(t *testing.T) {
	evt := nostr.Event{Kind: 30222, Tags: nostr.Tags{{"a", "30142:auth:slug"}, {"p", "commA"}}}
	if _, err := nostrToShare(&evt); err == nil {
		t.Fatal("expected error for kind-30222 missing 'd' tag")
	}
}

func nostrToShareDocID(t *testing.T, pubkey, d string) string {
	t.Helper()
	return typesense30142.GenerateDocumentID(pubkey, d)
}

func TestValidateShare(t *testing.T) {
	cases := []struct {
		name   string
		event  nostr.Event
		reject bool
	}{
		{"repost with h + e", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"e", "id"}}}, false},
		{"repost with h + a", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}, {"a", "30142:x:y"}}}, false},
		{"repost no community", nostr.Event{Kind: 16, Tags: nostr.Tags{{"e", "id"}, {"p", "author"}}}, true},
		{"repost no reference", nostr.Event{Kind: 16, Tags: nostr.Tags{{"h", "c"}}}, true},
		{"targeted via p with a + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"p", "c"}, {"a", "30142:x:y"}}}, false},
		{"targeted via h with e + d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"h", "c"}, {"e", "id"}}}, false},
		{"targeted missing d", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"p", "c"}, {"a", "30142:x:y"}}}, true},
		{"targeted no community", nostr.Event{Kind: 30222, Tags: nostr.Tags{{"d", "s"}, {"a", "30142:x:y"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reject, msg := validateShare(c.event)
			if reject != c.reject {
				t.Errorf("validateShare = %v (%q), want reject=%v", reject, msg, c.reject)
			}
		})
	}
}

func TestSharesSchema(t *testing.T) {
	schema := sharesSchema("community_shares")
	if schema.Name != "community_shares" {
		t.Fatalf("schema name = %q", schema.Name)
	}
	want := map[string]string{
		"id": "string",
		// `d` gives the collection a queryable addressable identity. Its
		// absence also broke a-tag deletion enforcement on kind 30222, since
		// handleDeleteRequest resolves its target through this collection.
		"d":    "string",
		"refE": "string[]",
		"refA": "string[]",
		// string, not int32: the shared query path filters every tag with
		// backtick-quoted exact match, which an int field will not accept.
		"refKind":   "string",
		"community": "string[]", // from structuredEnvelopeFields — drives #h / community:
		"eventID":   "string",   // delete-by-eventID needs this present
	}
	got := make(map[string]string)
	for _, f := range schema.Fields {
		got[f.Name] = f.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("field %q type = %q, want %q", name, got[name], typ)
		}
	}
}

// nostrlib#6: the community_shares collection was filterable by #h and nothing
// else. Every other tag resolved to a field the collection does not declare,
// and Typesense answers found:0 for a filter on an absent field — so the query
// was honoured-as-empty, indistinguishable from "no events match".
//
// These pin the two halves of the fix together: the field the schema declares
// and the field a filter on that tag actually resolves to. If they ever drift,
// the filters go silently back to returning 0.
func TestSharesTagFilters_ResolveToDeclaredFields(t *testing.T) {
	declared := make(map[string]bool)
	for _, f := range sharesSchema("community_shares").Fields {
		declared[f.Name] = true
	}

	for tag, field := range sharesTagFields() {
		if !declared[field] {
			t.Errorf("#%s maps to %q, which sharesSchema does not declare — the filter would return 0 for every input", tag, field)
		}
	}

	// `d` is not in the map: the query path handles it directly as `d:=`, so
	// the schema has to carry that exact field name.
	if !declared["d"] {
		t.Error("sharesSchema declares no `d` field, so #d and a-tag deletion enforcement both resolve to nothing")
	}
	// `h` was the one tag that already worked; it must keep working.
	if !declared["community"] {
		t.Error("sharesSchema lost the `community` field that backs #h")
	}
}

// The plugin-shaped queries that returned 0 for every input before this.
// Driven through the public CountEvents path against a stub Typesense so the
// assertion is on the filter_by the relay actually sends, not on an internal
// helper.
func sharesFilterBy(t *testing.T, filter nostr.Filter) (string, bool) {
	t.Helper()
	var gotFilterBy string
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotFilterBy = r.URL.Query().Get("filter_by")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"found":0}`))
	}))
	defer srv.Close()

	ts := &typesense30142.TSBackend{
		Host:           srv.URL,
		ApiKey:         "k",
		CollectionName: "community_shares",
		SearchFields:   "eventID",
		TagFields:      sharesTagFields(),
	}
	if _, err := ts.CountEvents(filter); err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	return gotFilterBy, called
}

func TestSharesBackendFilterExpressions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter nostr.Filter
		want   string
	}{
		{
			"which communities carry this resource",
			nostr.Filter{Kinds: []nostr.Kind{16}, Tags: nostr.TagMap{"a": []string{"30142:pk:https://example.org/r"}}},
			"refA:=`30142:pk:https://example.org/r`",
		},
		{
			"all community shares of AMB resources",
			nostr.Filter{Kinds: []nostr.Kind{16}, Tags: nostr.TagMap{"k": []string{"30142"}}},
			"refKind:=`30142`",
		},
		{
			"shares referencing this event",
			nostr.Filter{Tags: nostr.TagMap{"e": []string{"abc123"}}},
			"refE:=`abc123`",
		},
		{
			"a-tag deletion enforcement shape: kinds + #d",
			nostr.Filter{Kinds: []nostr.Kind{30222}, Tags: nostr.TagMap{"d": []string{"the-d"}}},
			"d:=`the-d`",
		},
		{
			"the one that already worked",
			nostr.Filter{Tags: nostr.TagMap{"h": []string{"comm1"}}},
			"community:=`comm1`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, called := sharesFilterBy(t, tc.filter)
			if !called {
				t.Fatal("no request reached Typesense — the filter was reported unsatisfiable and would yield nothing")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("filter_by = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// Fail-closed must survive: a tag the collection genuinely cannot express still
// yields nothing rather than dropping the clause and widening the query. The
// evidence is that no request is sent at all.
func TestSharesBackendUnmappedTagStillFailsClosed(t *testing.T) {
	_, called := sharesFilterBy(t, nostr.Filter{Tags: nostr.TagMap{"zzz": []string{"v"}}})
	if called {
		t.Error("an unmappable tag must fail closed, but a request was sent to Typesense")
	}
}
