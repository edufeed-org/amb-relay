package main

import (
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Concept filter expansion (spec: docs/superpowers/specs/2026-10-10-mapping-expansion-design.md).
// The expander rewrites each concept filter into a synthetic tag filter
// concepts_<n>; TagFields aliases every such key onto the one `concepts`
// field, so separate filters stay separate && clauses.
const (
	conceptTagPrefix  = "concepts_"
	maxConceptFilters = 8
)

func conceptTagFields() map[string]string {
	m := make(map[string]string, maxConceptFilters)
	for i := 0; i < maxConceptFilters; i++ {
		m[fmt.Sprintf("%s%d", conceptTagPrefix, i)] = "concepts"
	}
	return m
}

// hasConceptTags reports whether a filter carries synthetic concept tags.
// Events never carry them, so filter.Matches would reject every hit.
func hasConceptTags(f nostr.Filter) bool {
	for name := range f.Tags {
		if strings.HasPrefix(name, conceptTagPrefix) {
			return true
		}
	}
	return false
}

// ensureConceptsField adds `concepts` to a custom schema stored before the
// field existed (DefaultSchema already has it).
func ensureConceptsField(schema *typesense30142.CollectionSchema) {
	for _, f := range schema.Fields {
		if f.Name == "concepts" {
			return
		}
	}
	schema.Fields = append(schema.Fields, typesense30142.Field{Name: "concepts", Type: "string[]", Optional: true})
}
