package main

import (
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// fetchFunc abstracts a content type's event query (e.g. tsDB.QueryEvents) so
// the registry and the semantic rerank layer can consume it without Typesense.
type fetchFunc func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]

// contentType is one content shape the relay serves. Its fields are exactly the
// per-kind variation the event path switched on before the registry existed —
// discovered from the AMB (30142) and long-form (30023) duplication. Projection
// and schema are deliberately absent: projection lives inside each store closure
// (AMB: the write buffer's PrepareBatch; long-form: storeLongform), schema is set
// once at backend Init, and neither was ever branched on per-event in main.go.
type contentType struct {
	kinds    []nostr.Kind
	validate func(nostr.Event) (reject bool, msg string)
	store    func(nostr.Event)
	fetch    fetchFunc
	count    func(nostr.Filter) (uint32, error)
	deleteID func(nostr.ID) error

	// chunked is true when amb-indexer chunks this type's content into the
	// shared chunk collection (AMB, long-form, wiki). Calendar events are not
	// chunked, so chunk-rerank must not own their search queries — see
	// registry.targetsChunked.
	chunked bool
}

// registry dispatches the event path across registered content types by kind.
// types is kept in registration order so fan-out (fetch/count/delete) is
// deterministic; byKind indexes into it for O(1) single-event dispatch.
type registry struct {
	types  []contentType
	byKind map[nostr.Kind]int
}

func newRegistry(types ...contentType) *registry {
	r := &registry{types: types, byKind: make(map[nostr.Kind]int, len(types))}
	for i, ct := range types {
		for _, k := range ct.kinds {
			r.byKind[k] = i
		}
	}
	return r
}

// kinds returns every registered kind, for NIP-11 retention advertisement.
func (r *registry) kinds() []nostr.Kind {
	var out []nostr.Kind
	for _, ct := range r.types {
		out = append(out, ct.kinds...)
	}
	return out
}

// validate dispatches to the owning content type. Unregistered kinds are the
// single source of "kind not accepted".
func (r *registry) validate(event nostr.Event) (reject bool, msg string) {
	i, ok := r.byKind[event.Kind]
	if !ok {
		return true, "kind not accepted"
	}
	return r.types[i].validate(event)
}

// store routes a validated event to the owning content type's write path.
func (r *registry) store(event nostr.Event) {
	if i, ok := r.byKind[event.Kind]; ok {
		r.types[i].store(event)
	}
}

// selected returns indices of the content types a filter targets: those owning
// any kind in filter.Kinds, or all types when filter.Kinds is empty (a
// kind-agnostic query, e.g. the chunk-rerank parent fetch or Negentropy).
func (r *registry) selected(filter nostr.Filter) []int {
	if len(filter.Kinds) == 0 {
		idx := make([]int, len(r.types))
		for i := range r.types {
			idx[i] = i
		}
		return idx
	}
	seen := make(map[int]bool)
	var idx []int
	for _, k := range filter.Kinds {
		if i, ok := r.byKind[k]; ok && !seen[i] {
			seen[i] = true
			idx = append(idx, i)
		}
	}
	return idx
}

// fetch is a fetchFunc fanning a filter out to every selected content type,
// merging events in registration order and de-duplicating by id. Caller
// early-exit is preserved: once yield returns false it stops pulling. With a
// single registered type this is an exact passthrough of that type's fetch.
func (r *registry) fetch(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		seen := make(map[nostr.ID]bool)
		for _, i := range r.selected(filter) {
			for ev := range r.types[i].fetch(filter, maxLimit) {
				if seen[ev.ID] {
					continue
				}
				seen[ev.ID] = true
				if !yield(ev) {
					return
				}
			}
		}
	}
}

// targetsChunked reports whether a search over this filter could be served by
// the chunk index. Kind-agnostic filters (empty Kinds) include chunked content,
// so they qualify. Otherwise at least one targeted kind must belong to a chunked
// content type. Calendar is registered chunked:true, so calendar searches route
// into calendarRerankQuery (which strips synthetic range params and re-windows
// the reranked pool relay-side).
func (r *registry) targetsChunked(filter nostr.Filter) bool {
	if len(filter.Kinds) == 0 {
		for _, ct := range r.types {
			if ct.chunked {
				return true
			}
		}
		return false
	}
	for _, k := range filter.Kinds {
		if i, ok := r.byKind[k]; ok && r.types[i].chunked {
			return true
		}
	}
	return false
}

// searchHasFreeText reports whether a NIP-50 search carries at least one
// free-text term. Pure field:value searches (e.g. "community:<pubkey>") or
// "sort:" directives have nothing to rank semantically, so chunk-rerank must
// not own them — otherwise the search string is sent to the chunk index
// literally, dropping the field filter and returning coincidental matches.
func searchHasFreeText(search string) bool {
	return len(typesense30142.ParseSearchQuery(search).RawTerms) > 0
}

// count sums event counts across the content types a filter targets.
func (r *registry) count(filter nostr.Filter) (uint32, error) {
	var total uint32
	for _, i := range r.selected(filter) {
		n, err := r.types[i].count(filter)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// deleteEverywhere removes an id from every content type's search collection. A
// NIP-09/ban delete carries only the id, not the kind, so all collections are
// tried; per-collection delete-by-filter is idempotent (a miss is a 200/no-op).
// Returns the first error (types are tried in registration order, AMB first);
// any later errors go to logErr.
func (r *registry) deleteEverywhere(id nostr.ID, logErr func(error)) error {
	var firstErr error
	for i := range r.types {
		if err := r.types[i].deleteID(id); err != nil {
			if firstErr == nil {
				firstErr = err
			} else if logErr != nil {
				logErr(err)
			}
		}
	}
	return firstErr
}
