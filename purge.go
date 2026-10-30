// purge.go — removing an event from every store: NIP-09 deletions (author)
// and NIP-86 banevent (admin).
//
// Every search collection keys addressable/replaceable docs by coordinate
// ({pubkey}:{d}, or {kind}:{pubkey}:{d} for multi-kind collections), not by
// event id, but each doc carries the id in its `eventID` field and
// TSBackend.DeleteEvent deletes by `filter_by=eventID:=<id>` — so a delete by
// id reaches the doc whatever its key, and works when the event is already
// gone from Bolt.
//
// What the id alone cannot guard against is a write that is already queued:
// the AMB projection is asynchronous (TSWriteBuffer), so a delete issued
// directly against Typesense can be overtaken by a flush of the same event
// that was queued before the ban/deletion, silently re-creating the doc.
// Hence the order used here: record the ban/tombstone FIRST (both write
// buffers re-check it right before writing), then delete synchronously
// (errors reach the caller), then queue a serialized delete behind every
// projection already in the buffer.
package main

import (
	"context"
	"fmt"
	"iter"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
)

// rawEventStore is the subset of *boltdb.BoltBackend the purge path needs.
type rawEventStore interface {
	QueryEvents(nostr.Filter, int) iter.Seq[nostr.Event]
	DeleteEvent(nostr.ID) error
}

type eventPurger struct {
	bolt    rawEventStore
	mgmt    *ManagementStore
	content interface{ Delete(string) error }
	reg     *registry
	// queueDelete serializes a backstop delete behind queued projections
	// (tsBuf.QueueDelete). Optional.
	queueDelete func(nostr.ID)
	// beforeDelete runs before an author deletion removes the event and may
	// return a func to run afterwards (main.go: community-share restamp).
	// Optional.
	beforeDelete func(nostr.ID) (after func())
	logf         func(format string, args ...any)
}

func (p *eventPurger) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// purge removes id from Bolt, the content store and every search collection.
// Idempotent; safe when the event is already gone from any or all of them.
func (p *eventPurger) purge(id nostr.ID) error {
	if err := p.bolt.DeleteEvent(id); err != nil {
		p.log("delete %s from bolt: %v\n", id.Hex(), err)
	}
	if p.content != nil {
		_ = p.content.Delete(id.Hex()) // idempotent; no row is fine
	}
	// The id carries no kind, so try every collection (each delete is a
	// filter-by-eventID no-op when the doc lives elsewhere).
	err := p.reg.deleteEverywhere(id, func(e error) {
		p.log("delete %s (secondary collection): %v\n", id.Hex(), e)
	})
	if p.queueDelete != nil {
		p.queueDelete(id)
	}
	return err
}

// authorDelete is relay.DeleteEvent: khatru calls it for each event a NIP-09
// request targets (after AllowDeleting), and applyCoordDeletions calls it for
// 'a'-tag versions. The id is tombstoned before anything is removed.
func (p *eventPurger) authorDelete(_ context.Context, id nostr.ID) error {
	var after func()
	if p.beforeDelete != nil {
		after = p.beforeDelete(id)
	}
	if err := p.mgmt.MarkEventDeleted(id.Hex()); err != nil {
		p.log("mark deleted %s: %v\n", id.Hex(), err)
	}
	err := p.purge(id)
	if after != nil {
		after()
	}
	return err
}

// ban is NIP-86 banevent: record the ban first (so neither write buffer nor
// the read path lets the event back), then purge it everywhere. A purge
// error is returned, but the ban stands and the event is no longer served;
// re-calling banevent retries the purge.
func (p *eventPurger) ban(_ context.Context, id nostr.ID, reason string) error {
	if err := p.mgmt.BanEvent(id, reason); err != nil {
		return err
	}
	return p.purge(id)
}

// coordRef is one NIP-09 'a' target honoured by applyCoordDeletions.
type coordRef struct {
	raw    string
	kind   nostr.Kind
	pubkey nostr.PubKey
	d      string
}

// sameAuthorCoords returns the 'a' targets of a deletion request that
// reference replaceable/addressable events of the request's own author.
// NIP-09: a deletion can only remove its author's events; foreign
// coordinates are ignored.
func sameAuthorCoords(del nostr.Event) []coordRef {
	var out []coordRef
	for _, tag := range del.Tags {
		if len(tag) < 2 || tag[0] != "a" {
			continue
		}
		spl := strings.SplitN(tag[1], ":", 3)
		if len(spl) != 3 {
			continue
		}
		n, err := strconv.Atoi(spl[0])
		if err != nil || n < 0 {
			continue
		}
		kind := nostr.Kind(n)
		if !kind.IsAddressable() && !kind.IsReplaceable() {
			continue
		}
		pk, err := nostr.PubKeyFromHex(spl[1])
		if err != nil || pk != del.PubKey {
			continue
		}
		d := spl[2]
		if kind.IsReplaceable() {
			d = ""
		}
		out = append(out, coordRef{raw: fmt.Sprintf("%d:%s:%s", kind, pk.Hex(), d), kind: kind, pubkey: pk, d: d})
	}
	return out
}

// applyCoordDeletions handles a stored kind-5's 'a' tags: it tombstones each
// same-author coordinate up to the request's created_at (so an older or
// equal version can never be re-published, while a newer one is accepted)
// and removes every stored version at or before that time from Bolt and
// every search collection. Versions are looked up in Bolt AND in the search
// index, so a doc that outlived its Bolt row is still found.
//
// khatru's own NIP-09 pass runs after StoreEvent and finds nothing left for
// these coordinates; it still handles the 'e' tags.
//
// Returns how many coordinates were tombstoned, how many stored versions
// were purged, and how many tombstone/purge attempts failed (errs) — used by
// coord_tombstone_backfill.go to log progress and to decide whether a backfill
// run is clean enough to mark complete. Live callers (main.go's StoreEvent)
// ignore the return values; errors still reach the operator via p.log either
// way.
func (p *eventPurger) applyCoordDeletions(ctx context.Context, del nostr.Event) (tombstoned, purged, errs int) {
	if del.Kind != nostr.KindDeletion {
		return 0, 0, 0
	}
	for _, c := range sameAuthorCoords(del) {
		if err := p.mgmt.MarkCoordDeleted(c.raw, del.CreatedAt); err != nil {
			p.log("tombstone %s: %v\n", c.raw, err)
			errs++
		} else {
			tombstoned++
		}
		f := nostr.Filter{
			Kinds:   []nostr.Kind{c.kind},
			Authors: []nostr.PubKey{c.pubkey},
			Until:   del.CreatedAt,
		}
		if c.kind.IsAddressable() {
			f.Tags = nostr.TagMap{"d": []string{c.d}}
		}
		seen := map[nostr.ID]bool{}
		var targets []nostr.ID
		collect := func(seq iter.Seq[nostr.Event]) {
			for ev := range seq {
				if seen[ev.ID] || ev.PubKey != del.PubKey || ev.CreatedAt > del.CreatedAt {
					continue
				}
				if c.kind.IsAddressable() && ev.Tags.GetD() != c.d {
					continue
				}
				seen[ev.ID] = true
				targets = append(targets, ev.ID)
			}
		}
		collect(p.bolt.QueryEvents(f, 1000))
		collect(p.reg.fetch(f, 250))
		for _, id := range targets {
			if err := p.authorDelete(ctx, id); err != nil {
				p.log("a-tag delete %s (%s): %v\n", id.Hex(), c.raw, err)
				errs++
				continue
			}
			purged++
		}
	}
	return tombstoned, purged, errs
}

// gateSeq drops events the gate blocks from a query result: the read-side
// half of the ban/deletion guarantee. It keeps a doc that outlived its ban or
// deletion in a search collection (as after the 2026-10-29 incident) from
// being served while it awaits cleanup.
func gateSeq(seq iter.Seq[nostr.Event], blocked func(nostr.Event) bool) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for ev := range seq {
			if blocked(ev) {
				continue
			}
			if !yield(ev) {
				return
			}
		}
	}
}

// gatedRawStore wraps the raw store for startup hydration (Typesense →
// Bolt): a banned/deleted event whose search doc outlived its purge must not
// be copied back into Bolt. Blocked saves are skipped as success.
type gatedRawStore struct {
	hydrateStore
	blocked func(nostr.Event) bool
}

type hydrateStore interface {
	SaveEvent(nostr.Event) error
	QueryEvents(nostr.Filter, int) iter.Seq[nostr.Event]
}

func (g gatedRawStore) SaveEvent(ev nostr.Event) error {
	if g.blocked(ev) {
		return nil
	}
	return g.hydrateStore.SaveEvent(ev)
}
