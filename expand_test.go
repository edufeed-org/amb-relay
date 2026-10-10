package main

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
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
