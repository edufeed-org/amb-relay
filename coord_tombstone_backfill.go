// coord_tombstone_backfill.go — one-shot startup backfill of NIP-09 'a'-tag
// coordinate tombstones for kind-5 deletion requests stored before
// tombstone.go's deleted_coords bucket existed.
//
// Those older kind-5 events never ran through applyCoordDeletions (purge.go),
// so an older version of the coordinates they reference could still be
// re-published even though the author asked for all versions up to the
// deletion's created_at to be gone. This file closes that gap by replaying
// every stored kind-5 through the exact same live code path once, at
// startup, after the stores are open and before the listener starts.
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
// applied. Bump the suffix (v2, v3, …) if the backfill logic ever needs to
// run again for everyone regardless of the force override.
const migrationCoordTombstonesV1 = "migration:coord_tombstones_v1"

// backfillCoordTombstones scans every stored kind-5 deletion request and
// runs it through purger.applyCoordDeletions — the exact path a live
// deletion takes: each same-author 'a' coordinate is tombstoned up to the
// request's created_at (MarkCoordDeleted; monotonic, safe to repeat) and
// every stored version at or before that time is purged from Bolt and the
// search collections. Foreign-author coordinates and 'e' tags are not part
// of this (sameAuthorCoords already excludes them; 'e'-tag deletions need no
// backfill since khatru's own NIP-09 pass has always handled them).
//
// Deliberately reuses the live path rather than a tombstone-only subset of
// it: NIP-09 says relays SHOULD delete every version up to the deletion's
// created_at, and running the exact same code a fresh deletion would run
// keeps a backfilled coordinate behaviorally identical to one deleted after
// the tombstone feature shipped. Nothing here purges anything that a live
// re-delivery of the same kind-5 wouldn't also purge.
//
// Runs once per database: a full scan that completes without error writes a
// marker to mgmt, and every later startup skips the scan entirely. A scan
// that errors out never reaches the marker write, so it is retried on the
// next startup. COORD_TOMBSTONE_BACKFILL_FORCE=true (env) re-runs the scan
// regardless of the marker — safe, since MarkCoordDeleted and authorDelete
// are both idempotent.
func backfillCoordTombstones(ctx context.Context, bolt rawEventStore, mgmt *ManagementStore, purger *eventPurger) error {
	if os.Getenv("COORD_TOMBSTONE_BACKFILL_FORCE") != "true" {
		done, err := mgmt.MigrationDone(migrationCoordTombstonesV1)
		if err != nil {
			return fmt.Errorf("coord tombstone backfill: check marker: %w", err)
		}
		if done {
			return nil
		}
	}

	// Drain the query fully before calling applyCoordDeletions: boltdb's
	// QueryEvents runs its yield callback from inside one bbolt.View
	// transaction for the whole iteration, and applyCoordDeletions writes
	// (MarkCoordDeleted/authorDelete both call DB.Update) — a nested managed
	// transaction on the same goroutine, which bbolt deadlocks on rather than
	// rejects. Collecting first closes the View before any write happens.
	var dels []nostr.Event
	for ev := range bolt.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}}, math.MaxInt) {
		dels = append(dels, ev)
	}

	scanned, tombstones := 0, 0
	for _, ev := range dels {
		scanned++
		tombstones += len(sameAuthorCoords(ev))
		purger.applyCoordDeletions(ctx, ev)
	}

	if err := mgmt.MarkMigrationDone(migrationCoordTombstonesV1); err != nil {
		return fmt.Errorf("coord tombstone backfill: record marker: %w", err)
	}
	fmt.Printf("coord tombstone backfill: scanned %d kind-5 event(s), applied %d tombstone(s)\n", scanned, tombstones)
	return nil
}
