package main

import (
	"context"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

// backfillHarness wires a bare bolt + mgmt + eventPurger (no Typesense, no
// khatru relay) so a kind-5 can be saved directly into Bolt the way it would
// sit there from before tombstone.go existed — i.e. without ever having run
// through applyCoordDeletions.
type backfillHarness struct {
	bolt   *boltdb.BoltBackend
	mgmt   *ManagementStore
	purger *eventPurger
}

func newBackfillHarness(t *testing.T) *backfillHarness {
	t.Helper()
	db := openTestBolt(t)
	mgmt := &ManagementStore{}
	if err := mgmt.Init(db.DB); err != nil {
		t.Fatal(err)
	}
	reg := newRegistry(deletionContentType(db, map[nostr.Kind]bool{30142: true}))
	purger := &eventPurger{bolt: db, mgmt: mgmt, content: &ContentStore{DB: db.DB}, reg: reg, logf: t.Logf}
	return &backfillHarness{bolt: db, mgmt: mgmt, purger: purger}
}

func (h *backfillHarness) inBolt(id nostr.ID) bool {
	for range h.bolt.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
		return true
	}
	return false
}

func TestBackfillCoordTombstones_TombstonesAndPurgesPreExistingDeletion(t *testing.T) {
	h := newBackfillHarness(t)
	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})

	// Simulate pre-deploy state: both saved straight to Bolt, bypassing
	// StoreEvent/applyCoordDeletions entirely.
	for _, ev := range []nostr.Event{v1, del} {
		if err := h.bolt.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, ok := h.mgmt.CoordDeletedUntil(coordOf(v1)); ok {
		t.Fatal("setup: no tombstone should exist yet")
	}

	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	until, ok := h.mgmt.CoordDeletedUntil(coordOf(v1))
	if !ok || until != 2000 {
		t.Fatalf("tombstone = (%v, %v), want (2000, true)", until, ok)
	}
	if h.inBolt(v1.ID) {
		t.Fatal("backfill must purge the covered version, same as a live deletion would")
	}

	// Older-or-equal version now rejected via the gate.
	if r := h.mgmt.BlockReason(signedEvent(t, sk, 30142, 1500, tags)); r == "" {
		t.Fatal("version older than the deletion must now be rejected")
	}
	if r := h.mgmt.BlockReason(signedEvent(t, sk, 30142, 2000, tags)); r == "" {
		t.Fatal("version at the deletion's created_at must now be rejected")
	}
	// Newer version accepted.
	if r := h.mgmt.BlockReason(signedEvent(t, sk, 30142, 2001, tags)); r != "" {
		t.Fatalf("newer version must be accepted, got reason %q", r)
	}
}

func TestBackfillCoordTombstones_ForeignAuthorIgnored(t *testing.T) {
	h := newBackfillHarness(t)
	victim, attacker := nostr.Generate(), nostr.Generate()
	v := signedEvent(t, victim, 30142, 1000, nostr.Tags{{"d", "res"}})
	del := signedEvent(t, attacker, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v)}})
	for _, ev := range []nostr.Event{v, del} {
		if err := h.bolt.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if !h.inBolt(v.ID) {
		t.Fatal("cross-author a-tag must not purge the victim's event")
	}
	if _, ok := h.mgmt.CoordDeletedUntil(coordOf(v)); ok {
		t.Fatal("cross-author a-tag must not record a tombstone")
	}
}

func TestBackfillCoordTombstones_MarkerPreventsSecondRun(t *testing.T) {
	h := newBackfillHarness(t)
	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})
	for _, ev := range []nostr.Event{v1, del} {
		if err := h.bolt.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("first backfill: %v", err)
	}

	// Re-insert v1 directly into Bolt (bypassing the gate, as a second scan
	// would see it) to prove a second, unforced run does nothing: if it ran
	// applyCoordDeletions again it would purge v1 right back out.
	if err := h.bolt.SaveEvent(v1); err != nil {
		t.Fatalf("re-save v1: %v", err)
	}
	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if !h.inBolt(v1.ID) {
		t.Fatal("marker should have skipped the second scan, but v1 was purged again")
	}
}

func TestBackfillCoordTombstones_ForceEnvReRuns(t *testing.T) {
	h := newBackfillHarness(t)
	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})
	for _, ev := range []nostr.Event{v1, del} {
		if err := h.bolt.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if err := h.bolt.SaveEvent(v1); err != nil {
		t.Fatalf("re-save v1: %v", err)
	}

	t.Setenv("COORD_TOMBSTONE_BACKFILL_FORCE", "true")
	if err := backfillCoordTombstones(context.Background(), h.bolt, h.mgmt, h.purger); err != nil {
		t.Fatalf("forced backfill: %v", err)
	}
	if h.inBolt(v1.ID) {
		t.Fatal("forced re-run must re-apply applyCoordDeletions and purge v1 again")
	}
}

func TestBackfillCoordTombstones_FailedRunDoesNotSetMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	db := &boltdb.BoltBackend{Path: path}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	mgmt := &ManagementStore{}
	if err := mgmt.Init(db.DB); err != nil {
		t.Fatal(err)
	}
	reg := newRegistry(deletionContentType(db, map[nostr.Kind]bool{30142: true}))
	purger := &eventPurger{bolt: db, mgmt: mgmt, content: &ContentStore{DB: db.DB}, reg: reg, logf: t.Logf}

	db.Close()
	if err := backfillCoordTombstones(context.Background(), db, mgmt, purger); err == nil {
		t.Fatal("backfill against a closed DB must return an error")
	}

	// Reopen the same file: the marker must be absent, proving the failed
	// attempt never recorded success.
	db2 := &boltdb.BoltBackend{Path: path}
	if err := db2.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db2.Close)
	mgmt2 := &ManagementStore{}
	if err := mgmt2.Init(db2.DB); err != nil {
		t.Fatal(err)
	}
	done, err := mgmt2.MigrationDone(migrationCoordTombstonesV1)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("marker must not be set after a failed backfill")
	}
}
