// coord_tombstone_backfill.go — one-shot startup backfill of NIP-09 'a'-tag
// coordinate tombstones for kind-5 deletion requests stored before
// tombstone.go's deleted_coords bucket existed.
//
// Those older kind-5 events never ran through applyCoordDeletions (purge.go),
// so an older version of the coordinates they reference could still be
// re-published even though the author asked for all versions up to the
// deletion's created_at to be gone. This file closes that gap by replaying
// every stored kind-5 through the exact same live code path once. It is
// launched with `go` from main(), concurrently with the relay serving
// traffic, not before — see backfillCoordTombstones's doc comment.
package main

import (
	"context"
	"fmt"
	"math"
	"os"

	"fiatjaf.com/nostr"
)

// migrationCoordTombstonesV1 is the marker key recorded in ManagementStore's
// migrations bucket once a full scan of stored kind-5 events has been
// applied cleanly. Bump the suffix (v2, v3, …) if the backfill logic ever
// needs to run again for everyone regardless of the force override.
const migrationCoordTombstonesV1 = "migration:coord_tombstones_v1"

// backfillProgressEvery controls how often (in kind-5 events scanned) a
// progress line is logged, so a long run against a large corpus is
// observable rather than silent until the final summary.
const backfillProgressEvery = 100

// backfillEventStore is the subset of *boltdb.BoltBackend the backfill scan
// needs. QueryEvents drains the stored kind-5 events; CountEvents is an
// authoritative cross-check against that drain. nostrlib's boltdb backend
// swallows a bbolt read error inside QueryEvents (it logs the error and
// yields whatever it already had, returning no error at all), so a read
// failure partway through a large corpus would look identical to "there are
// no more kind-5 events" — and the marker would then be set on an incomplete
// scan. CountEvents propagates such an error as a real Go error instead, so
// comparing the two catches exactly that case.
type backfillEventStore interface {
	rawEventStore
	CountEvents(nostr.Filter) (uint32, error)
}

// backfillCoordTombstones scans every stored kind-5 deletion request and
// runs it through purger.applyCoordDeletions — the exact path a live
// deletion takes: each same-author 'a' coordinate is tombstoned up to the
// request's created_at (MarkCoordDeleted; monotonic, safe to repeat) and
// every stored version at or before that time is purged from Bolt and the
// search collections. Foreign-author coordinates and 'e' tags are not part
// of this (sameAuthorCoords already excludes the former; 'e'-tag deletions
// need no backfill since khatru's own NIP-09 pass has always handled them).
//
// Deliberately reuses the live path rather than a tombstone-only subset of
// it: NIP-09 says relays SHOULD delete every version up to the deletion's
// created_at, and running the exact same code a fresh deletion would run
// keeps a backfilled coordinate behaviorally identical to one deleted after
// the tombstone feature shipped.
//
// Must be launched with `go` from main(), concurrently with (not before) the
// relay accepting connections. A production corpus can hold thousands of
// kind-5 events, each costing a Bolt write plus Typesense lookups/deletes, so
// running this to completion before the listener opens would delay startup
// by an unbounded amount. Running it concurrently is safe: the read/write
// gate (tombstone.go) protects a coordinate the instant ITS tombstone is
// written, exactly as it does for a live deletion — nothing here requires
// the whole scan to finish before the relay can serve correctly.
//
// Runs once per database: a full scan that completes with zero tombstone or
// purge errors writes a marker to mgmt, and every later startup skips the
// scan entirely. Any failure — the Count/Query mismatch above, a tombstone
// or purge error, or ctx cancellation (the caller cancels ctx on SIGINT/
// SIGTERM so a run in flight at shutdown aborts cleanly) — returns an error
// and never reaches the marker write, so the scan is retried from scratch on
// the next startup. That retry is safe even for a partially-completed
// previous attempt: MarkCoordDeleted is monotonic and authorDelete's purge
// is idempotent. COORD_TOMBSTONE_BACKFILL_FORCE=true (env) re-runs the scan
// regardless of the marker.
func backfillCoordTombstones(ctx context.Context, bolt backfillEventStore, mgmt *ManagementStore, purger *eventPurger) error {
	if os.Getenv("COORD_TOMBSTONE_BACKFILL_FORCE") != "true" {
		done, err := mgmt.MigrationDone(migrationCoordTombstonesV1)
		if err != nil {
			return fmt.Errorf("coord tombstone backfill: check marker: %w", err)
		}
		if done {
			return nil
		}
	}

	kind5 := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}}

	authoritative, err := bolt.CountEvents(kind5)
	if err != nil {
		return fmt.Errorf("coord tombstone backfill: count kind-5 events: %w", err)
	}

	// Drain fully before any write: boltdb's QueryEvents runs its yield
	// callback from inside one bbolt.View transaction for the whole
	// iteration, and applyCoordDeletions writes (MarkCoordDeleted and
	// authorDelete's purge both call DB.Update) — a nested managed
	// transaction on the same goroutine, which bbolt deadlocks on rather
	// than rejects. Collecting first closes the View before any write
	// happens.
	var dels []nostr.Event
	for ev := range bolt.QueryEvents(kind5, math.MaxInt) {
		dels = append(dels, ev)
	}
	if uint32(len(dels)) != authoritative {
		return fmt.Errorf("coord tombstone backfill: drained %d kind-5 event(s) but CountEvents reports %d — refusing to mark complete (a QueryEvents read error looks identical to \"no more events\")",
			len(dels), authoritative)
	}

	scanned, tombstoned, purged, errs := 0, 0, 0, 0
	for _, ev := range dels {
		select {
		case <-ctx.Done():
			return fmt.Errorf("coord tombstone backfill: cancelled after %d/%d kind-5 event(s) scanned: %w", scanned, len(dels), ctx.Err())
		default:
		}

		scanned++
		t, p, e := purger.applyCoordDeletions(ctx, ev)
		tombstoned += t
		purged += p
		errs += e

		if scanned%backfillProgressEvery == 0 {
			fmt.Printf("coord tombstone backfill: progress %d/%d kind-5 scanned, %d tombstone(s), %d version(s) purged\n",
				scanned, len(dels), tombstoned, purged)
		}
	}

	if errs > 0 {
		return fmt.Errorf("coord tombstone backfill: %d tombstone/purge error(s) across %d kind-5 event(s) scanned — not marking complete, will retry next startup", errs, scanned)
	}

	// applyCoordDeletions also checks ctx between coordinates (purge.go), so
	// a cancellation landing mid-way through the LAST event's coordinates
	// would otherwise return here with errs==0 and no further per-event
	// check left to catch it — the loop above only checks ctx before each
	// event, not after the last one. This closes that gap: no cancellation
	// anywhere in the run, by either check, reaches the marker write.
	select {
	case <-ctx.Done():
		return fmt.Errorf("coord tombstone backfill: cancelled after %d/%d kind-5 event(s) scanned: %w", scanned, len(dels), ctx.Err())
	default:
	}

	if err := mgmt.MarkMigrationDone(migrationCoordTombstonesV1); err != nil {
		return fmt.Errorf("coord tombstone backfill: record marker: %w", err)
	}
	fmt.Printf("coord tombstone backfill: done — scanned %d kind-5 event(s), applied %d tombstone(s), purged %d version(s)\n",
		scanned, tombstoned, purged)
	return nil
}
