package main

import (
	"iter"

	"fiatjaf.com/nostr"
)

// combinedFetch returns a fetchFunc that routes a filter to the AMB backend,
// the long-form backend, or both, based on filter.Kinds — then concatenates
// results. A filter with no Kinds (e.g. the IDs-only fetch chunkRerankQuery
// issues for parent events) queries both, since the parent may live in either
// collection. De-dups by event ID.
func combinedFetch(ambFetch, lfFetch fetchFunc) fetchFunc {
	return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
		wantAMB, wantLF := false, false
		if len(filter.Kinds) == 0 {
			wantAMB, wantLF = true, true
		} else {
			for _, k := range filter.Kinds {
				switch k {
				case 30142:
					wantAMB = true
				case 30023:
					wantLF = true
				}
			}
		}
		return func(yield func(nostr.Event) bool) {
			seen := make(map[nostr.ID]bool)
			emit := func(src fetchFunc) bool {
				for e := range src(filter, maxLimit) {
					if seen[e.ID] {
						continue
					}
					seen[e.ID] = true
					if !yield(e) {
						return false
					}
				}
				return true
			}
			if wantAMB {
				if !emit(ambFetch) {
					return
				}
			}
			if wantLF {
				if !emit(lfFetch) {
					return
				}
			}
		}
	}
}
