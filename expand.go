package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/nip19"
	"github.com/edufeed-org/amb-relay/vocabmap"
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

// rateLimitedWarning lets a warning through at most once per
// noGraphWarnEvery, lock-free across concurrent queries.
type rateLimitedWarning struct{ last atomic.Int64 }

const noGraphWarnEvery = time.Minute

func (w *rateLimitedWarning) allow(now time.Time) bool {
	prev := w.last.Load()
	if prev != 0 && now.Sub(time.Unix(0, prev)) < noGraphWarnEvery {
		return false
	}
	return w.last.CompareAndSwap(prev, now.UnixNano())
}

// noGraphWarning rate-limits the spec §2 warning for queries that name
// trusted sources while none of them is loaded (relay unreachable, cold start).
var noGraphWarning rateLimitedWarning

type graphProvider interface {
	Defaults() Trust
	Graph(ctx context.Context, trust Trust) (*vocabmap.Graph, bool)
}

type expandOptions struct {
	off, related          bool
	concordances, authors []string
}

func (o *expandOptions) parse(value string) {
	switch value {
	case "off":
		o.off = true
		return
	case "related":
		o.related = true
		return
	}
	prefix, data, err := nip19.Decode(value)
	if err != nil {
		log.Printf("expand: ignoring %q: %v", value, err)
		return
	}
	switch prefix {
	case "naddr":
		if p, ok := data.(nostr.EntityPointer); ok && p.Kind == vocabmap.KindConcordance {
			o.concordances = append(o.concordances, fmt.Sprintf("%d:%s:%s", p.Kind, p.PublicKey.Hex(), p.Identifier))
		}
	case "npub":
		if pk, ok := data.(nostr.PubKey); ok {
			o.authors = append(o.authors, pk.Hex())
		}
	}
}

func (o expandOptions) trust(defaults Trust) Trust {
	t := Trust{Concordances: defaults.Concordances, Authors: o.authors}
	if len(o.concordances) > 0 {
		t.Concordances = o.concordances
	}
	return t
}

var conceptBases = map[string]bool{"educationalLevel": true, "about": true, "learningResourceType": true, "audience": true}

// isConceptKey: "<base><sep>id" for a core vocab field, or
// "ext<sep><ns><sep><facet><sep>id" (sep ":" for tags, "." for NIP-50 fields).
func isConceptKey(key, sep string) bool {
	parts := strings.Split(key, sep)
	if len(parts) == 2 {
		return parts[1] == "id" && conceptBases[parts[0]]
	}
	return len(parts) == 4 && parts[0] == "ext" && parts[1] != "" && parts[2] != "" && parts[3] == "id"
}

// searchTokens splits like typesense30142's NIP-50 tokenizer, but keeps each
// token verbatim (quotes included) so kept tokens re-join unchanged.
func searchTokens(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		if s[i] == '"' {
			if end := strings.Index(s[i+1:], `"`); end >= 0 {
				i += end + 2
				tokens = append(tokens, s[start:i])
				continue
			}
		}
		for i < len(s) && s[i] != ' ' {
			if s[i] == '"' {
				if end := strings.Index(s[i+1:], `"`); end >= 0 {
					i += end + 2
					continue
				}
			}
			i++
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens
}

// fieldToken mirrors typesense30142.parseFieldFilter; phrases are never fields.
func fieldToken(tok string) (field, value string, ok bool) {
	if strings.HasPrefix(tok, `"`) {
		return "", "", false
	}
	i := strings.Index(tok, ":")
	if i <= 0 {
		return "", "", false
	}
	value = strings.Trim(tok[i+1:], `"`)
	return tok[:i], value, value != ""
}

func onlyAMB(kinds []nostr.Kind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if k != 30142 {
			return false
		}
	}
	return true
}

// expandConceptFilters rewrites the concept filters of an AMB-only query into
// synthetic concepts_<n> tag filters holding the values expanded through the
// trusted mappings (spec §1.4/§1.5). `expand:` tokens are always stripped from
// Search. Anything that cannot be expanded is left as it was — expansion never
// narrows a query and never fails it.
func expandConceptFilters(ctx context.Context, filter nostr.Filter, reg graphProvider, debug bool) nostr.Filter {
	out := filter
	var opts expandOptions
	type token struct{ raw, field, value string }
	var tokens []token
	fieldsByBase := map[string]map[string]bool{}
	stripped := false
	for _, raw := range searchTokens(filter.Search) {
		t := token{raw: raw}
		if field, value, ok := fieldToken(raw); ok {
			if field == "expand" {
				opts.parse(value)
				stripped = true
				continue
			}
			t.field, t.value = field, value
			base, _, _ := strings.Cut(field, ".")
			if fieldsByBase[base] == nil {
				fieldsByBase[base] = map[string]bool{}
			}
			fieldsByBase[base][field] = true
		}
		tokens = append(tokens, t)
	}
	join := func(drop map[string]bool) string {
		raws := make([]string, 0, len(tokens))
		for _, t := range tokens {
			if !drop[t.field] || t.field == "" {
				raws = append(raws, t.raw)
			}
		}
		return strings.Join(raws, " ")
	}
	if stripped {
		out.Search = join(nil)
	}
	if reg == nil || opts.off || !onlyAMB(filter.Kinds) {
		return out
	}

	type group struct {
		tag, field string
		values     []string
	}
	var groups []group
	var tagNames []string
	for name := range filter.Tags {
		if isConceptKey(name, ":") {
			tagNames = append(tagNames, name)
		}
	}
	sort.Strings(tagNames)
	for _, name := range tagNames {
		groups = append(groups, group{tag: name, values: filter.Tags[name]})
	}
	seenField := map[string]int{}
	for _, t := range tokens {
		base, _, _ := strings.Cut(t.field, ".")
		if t.field == "" || !isConceptKey(t.field, ".") || len(fieldsByBase[base]) != 1 {
			continue
		}
		if i, ok := seenField[t.field]; ok {
			groups[i].values = append(groups[i].values, t.value)
			continue
		}
		seenField[t.field] = len(groups)
		groups = append(groups, group{field: t.field, values: []string{t.value}})
	}
	if len(groups) == 0 {
		return out
	}
	trust := opts.trust(reg.Defaults())
	g, ok := reg.Graph(ctx, trust)
	if !ok {
		if len(trust.sourceKeys()) > 0 && noGraphWarning.allow(time.Now()) {
			log.Printf("expand: no mapping graph available; running unexpanded")
		}
		return out
	}

	tags := make(nostr.TagMap, len(filter.Tags)+len(groups))
	for k, v := range filter.Tags {
		tags[k] = v
	}
	dropFields := map[string]bool{}
	slot, written := 0, false
	for _, gr := range groups {
		expanded, changed := expandValues(g, gr.values, opts.related)
		if !changed {
			continue
		}
		for slot < maxConceptFilters {
			if _, taken := tags[fmt.Sprintf("%s%d", conceptTagPrefix, slot)]; !taken {
				break
			}
			slot++
		}
		if slot >= maxConceptFilters {
			break
		}
		if debug {
			log.Printf("expand: %v -> %v", gr.values, expanded)
		}
		tags[fmt.Sprintf("%s%d", conceptTagPrefix, slot)] = expanded
		slot++
		written = true
		if gr.tag != "" {
			delete(tags, gr.tag)
		} else {
			dropFields[gr.field] = true
		}
	}
	if !written {
		return out
	}
	out.Tags = tags
	if len(dropFields) > 0 {
		out.Search = join(dropFields)
	}
	return out
}

// expandValues ORs the expansions of values: the values first, then each
// expansion's additions. changed is false when nothing was added.
func expandValues(g *vocabmap.Graph, values []string, related bool) ([]string, bool) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	changed := false
	for _, v := range values {
		ids, truncated := g.Expand(v, related)
		if truncated {
			log.Printf("expand: %s capped at %d identifiers", v, vocabmap.MaxResults)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
				changed = true
			}
		}
	}
	return out, changed
}
