// deletion.go — NIP-09 kind-5 deletion requests: write policy + read path.
package main

import (
	"iter"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

// validateDeletion is the write policy for kind-5 deletion requests. The
// relay stores deletions so clients and mirrors can learn that content was
// deleted, but stays scoped to kinds it serves: a deletion whose only
// targets are 'a' coords of foreign kinds is rejected. 'e' targets carry no
// kind, so any deletion with an 'e' tag is accepted.
func validateDeletion(served map[nostr.Kind]bool) func(nostr.Event) (reject bool, msg string) {
	return func(event nostr.Event) (bool, string) {
		hasE, hasA, servesA := false, false, false
		for _, tag := range event.Tags {
			if len(tag) < 2 || tag[1] == "" {
				continue
			}
			switch tag[0] {
			case "e":
				if _, err := nostr.IDFromHex(tag[1]); err == nil {
					hasE = true
				}
			case "a":
				hasA = true
				if spl := strings.SplitN(tag[1], ":", 2); len(spl) == 2 {
					if n, err := strconv.Atoi(spl[0]); err == nil && served[nostr.Kind(n)] {
						servesA = true
					}
				}
			}
		}
		switch {
		case hasE:
			return false, ""
		case !hasA:
			return true, "missing 'e' or 'a' tag referencing the event to delete"
		case !servesA:
			return true, "deletion references no kind served by this relay"
		}
		return false, ""
	}
}

// deletionContentType serves stored kind-5 deletion requests from the raw
// BoltDB store. Deletions are persisted by the normal StoreEvent path
// (boltBuf); this type only adds the read path, so REQs over
// kinds/authors/ids/#e/#a/#k return the deletion trail (NIP-09: relays
// SHOULD continue to share deletion requests).
//
// BoltDB holds every kind, so fetch/count clamp the filter to kind 5 —
// otherwise a kind-agnostic fan-out (Negentropy, chunk parent fetch) would
// re-yield all content events from the raw store. Bolt id-lookups ignore
// filter.Kinds entirely (queryByIds), hence the additional post-filter on
// the yielded events.
func deletionContentType(db *boltdb.BoltBackend, served map[nostr.Kind]bool) contentType {
	clamp := func(f nostr.Filter) nostr.Filter {
		f.Kinds = []nostr.Kind{nostr.KindDeletion}
		return f
	}
	return contentType{
		kinds:    []nostr.Kind{nostr.KindDeletion},
		validate: validateDeletion(served),
		store:    func(nostr.Event) {}, // persisted via boltBuf in relay.StoreEvent
		fetch: func(f nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
			return func(yield func(nostr.Event) bool) {
				for ev := range db.QueryEvents(clamp(f), maxLimit) {
					if ev.Kind != nostr.KindDeletion {
						continue
					}
					if !yield(ev) {
						return
					}
				}
			}
		},
		count:    func(f nostr.Filter) (uint32, error) { return db.CountEvents(clamp(f)) },
		deleteID: func(nostr.ID) error { return nil }, // no search collection to clean
		chunked:  false,
	}
}
