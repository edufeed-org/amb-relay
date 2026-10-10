package main

import (
	"context"
	"reflect"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip19"
	"github.com/edufeed-org/amb-relay/vocabmap"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

func TestConceptTagFields(t *testing.T) {
	m := conceptTagFields()
	if len(m) != maxConceptFilters {
		t.Fatalf("len = %d, want %d", len(m), maxConceptFilters)
	}
	if m["concepts_0"] != "concepts" || m["concepts_7"] != "concepts" {
		t.Fatalf("aliases = %v", m)
	}
}

func TestHasConceptTags(t *testing.T) {
	if hasConceptTags(nostr.Filter{Tags: nostr.TagMap{"about:id": {"x"}}}) {
		t.Fatal("about:id is not a synthetic concept tag")
	}
	if !hasConceptTags(nostr.Filter{Tags: nostr.TagMap{"concepts_3": {"x"}}}) {
		t.Fatal("concepts_3 is a synthetic concept tag")
	}
}

func TestEnsureConceptsField(t *testing.T) {
	s := typesense30142.CollectionSchema{Fields: []typesense30142.Field{{Name: "d", Type: "string"}}}
	ensureConceptsField(&s)
	ensureConceptsField(&s)
	n := 0
	for _, f := range s.Fields {
		if f.Name == "concepts" {
			n++
			if f.Type != "string[]" || !f.Optional {
				t.Fatalf("concepts field = %+v", f)
			}
		}
	}
	if n != 1 {
		t.Fatalf("concepts fields = %d, want 1", n)
	}
}

type fakeGraphs struct {
	g        *vocabmap.Graph
	ok       bool
	gotTrust Trust
}

func (f *fakeGraphs) Defaults() Trust { return Trust{Concordances: []string{"39741:def:d"}} }
func (f *fakeGraphs) Graph(_ context.Context, t Trust) (*vocabmap.Graph, bool) {
	f.gotTrust = t
	return f.g, f.ok
}

func testGraphs() *fakeGraphs {
	m := func(from, rel, to string) vocabmap.Mapping {
		return vocabmap.Mapping{From: from, Relation: rel, To: []string{to}}
	}
	return &fakeGraphs{ok: true, g: vocabmap.NewGraph(nil, []vocabmap.Mapping{
		m("G56", "broadMatch", "L2"), m("SA", "exactMatch", "SB"), m("GYM", "relatedMatch", "L2"),
	})}
}

var amb = []nostr.Kind{30142}

func TestExpandTagFilter(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}, "t": {"x"}}}, testGraphs(), false)
	want := nostr.TagMap{"concepts_0": {"L2", "G56"}, "t": {"x"}}
	if !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("tags = %v, want %v", got.Tags, want)
	}
}

func TestExpandSearchFieldAndPhrase(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Search: `"heilige schrift" educationalLevel.id:L2 sort:datePublished`}, testGraphs(), false)
	if got.Search != `"heilige schrift" sort:datePublished` {
		t.Errorf("search = %q", got.Search)
	}
	if !reflect.DeepEqual(got.Tags["concepts_0"], []string{"L2", "G56"}) {
		t.Errorf("tags = %v", got.Tags)
	}
}

func TestExpandTwoFiltersStayAnded(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}, Search: "about.id:SA"}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"L2", "G56"}, "concepts_1": {"SA", "SB"}}) || got.Search != "" {
		t.Fatalf("tags = %v search = %q", got.Tags, got.Search)
	}
}

func TestExpandExtTag(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"ext:ekw:subject:id": {"SB"}}}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"SB", "SA"}}) {
		t.Fatalf("tags = %v", got.Tags)
	}
}

func TestExpandLeavesUnmappedAndForeign(t *testing.T) {
	g := testGraphs()
	cases := []nostr.Filter{
		{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"NOMAP"}}},                     // nothing mapped
		{Kinds: amb, Search: "about.id:SA about.name:Foo"},                                     // same-base OR group
		{Kinds: []nostr.Kind{30142, 31923}, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}}, // not AMB-only
		{Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}},                                    // no kinds
		{Kinds: amb, Tags: nostr.TagMap{"keywords:id": {"L2"}}},                                // not a concept field
	}
	for _, f := range cases {
		if got := expandConceptFilters(context.Background(), f, g, false); !reflect.DeepEqual(got, f) {
			t.Errorf("%+v rewritten to %+v", f, got)
		}
	}
}

func TestExpandTokens(t *testing.T) {
	naddr := nip19.EncodeNaddr(vt.Owner, 39741, "k", nil)
	npub := nip19.EncodeNpub(vt.Other)
	g := testGraphs()

	off := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"educationalLevel:id": {"L2"}}, Search: "mathe expand:off"}, g, false)
	if off.Search != "mathe" || !reflect.DeepEqual(off.Tags, nostr.TagMap{"educationalLevel:id": {"L2"}}) {
		t.Errorf("expand:off -> %+v", off)
	}

	rel := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Search: "educationalLevel.id:L2 expand:related expand:" + naddr + " expand:" + npub + " expand:bogus"}, g, false)
	if rel.Search != "" || !reflect.DeepEqual(rel.Tags["concepts_0"], []string{"L2", "G56", "GYM"}) {
		t.Errorf("expand:related -> %+v", rel)
	}
	wantTrust := Trust{Concordances: []string{vt.ConcordanceAddr(vt.Owner, "k")}, Authors: []string{vt.Other.Hex()}}
	if !reflect.DeepEqual(g.gotTrust, wantTrust) {
		t.Errorf("trust = %+v, want %+v", g.gotTrust, wantTrust)
	}

	nilReg := expandConceptFilters(context.Background(), nostr.Filter{Kinds: amb, Search: "x expand:off"}, nil, false)
	if nilReg.Search != "x" {
		t.Errorf("tokens must be stripped without a registry: %q", nilReg.Search)
	}
}

func TestExpandDefaultTrustAndUnavailableGraph(t *testing.T) {
	g := testGraphs()
	expandConceptFilters(context.Background(), nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"about:id": {"SA"}}}, g, false)
	if !reflect.DeepEqual(g.gotTrust, g.Defaults()) {
		t.Errorf("default trust = %+v", g.gotTrust)
	}
	g.ok = false
	f := nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"about:id": {"SA"}}}
	if got := expandConceptFilters(context.Background(), f, g, false); !reflect.DeepEqual(got, f) {
		t.Errorf("unavailable graph must leave the filter: %+v", got)
	}
}

func TestExpandSkipsClientConceptSlots(t *testing.T) {
	got := expandConceptFilters(context.Background(),
		nostr.Filter{Kinds: amb, Tags: nostr.TagMap{"concepts_0": {"Q"}, "about:id": {"SA"}}}, testGraphs(), false)
	if !reflect.DeepEqual(got.Tags, nostr.TagMap{"concepts_0": {"Q"}, "concepts_1": {"SA", "SB"}}) {
		t.Fatalf("tags = %v", got.Tags)
	}
}
