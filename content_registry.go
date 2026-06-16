package main

import (
	"iter"

	"fiatjaf.com/nostr"
)

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
