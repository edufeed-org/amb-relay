package main

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

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

// alwaysFailingDelete is a contentType registered for kind 30142 whose
// deleteID always errors, standing in for a Typesense collection that is
// unreachable during the backfill's purge step. Its validate/store/fetch are
// no-ops; deleteID is the only thing applyCoordDeletions's purge path calls
// on it (via registry.deleteEverywhere).
func alwaysFailingDelete(kind nostr.Kind) contentType {
	return contentType{
		kinds:    []nostr.Kind{kind},
		validate: func(nostr.Event) (bool, string) { return false, "" },
		store:    func(nostr.Event) {},
		fetch:    func(nostr.Filter, int) iter.Seq[nostr.Event] { return func(func(nostr.Event) bool) {} },
		count:    func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(nostr.ID) error { return errors.New("typesense unreachable") },
	}
}

// TestBackfillCoordTombstones_PurgeErrorDoesNotSetMarker exercises a REAL
// failure in the scan (not merely a pre-check failure, which the earlier,
// misleading version of this test relied on by closing the DB before the
// first call): the coordinate's covered version lives in Bolt and is found
// and tombstoned, but purging it from the (fake, always-failing) search
// collection errors, exactly as a Typesense outage would during a live
// deletion. The marker must not be set, so the scan is retried next startup.
func TestBackfillCoordTombstones_PurgeErrorDoesNotSetMarker(t *testing.T) {
	db := openTestBolt(t)
	mgmt := &ManagementStore{}
	if err := mgmt.Init(db.DB); err != nil {
		t.Fatal(err)
	}
	reg := newRegistry(alwaysFailingDelete(30142), deletionContentType(db, map[nostr.Kind]bool{30142: true}))
	purger := &eventPurger{bolt: db, mgmt: mgmt, content: &ContentStore{DB: db.DB}, reg: reg, logf: t.Logf}

	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})
	for _, ev := range []nostr.Event{v1, del} {
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	err := backfillCoordTombstones(context.Background(), db, mgmt, purger)
	if err == nil {
		t.Fatal("a purge error during the scan must be surfaced as an error")
	}

	done, derr := mgmt.MigrationDone(migrationCoordTombstonesV1)
	if derr != nil {
		t.Fatal(derr)
	}
	if done {
		t.Fatal("marker must not be set when a purge failed")
	}
}

// TestBackfillCoordTombstones_CountMismatchDoesNotSetMarker covers the other
// real failure mode item 2 asks for: QueryEvents silently swallowing a bbolt
// read error and yielding fewer events than are actually stored, which would
// otherwise look identical to "there were only this many kind-5 events" and
// let the marker get set on an incomplete scan. A fake store whose
// QueryEvents under-reports relative to its own CountEvents reproduces that
// exact shape without needing to corrupt a real bbolt file.
type mismatchedCountStore struct {
	*boltdb.BoltBackend
}

func (m mismatchedCountStore) CountEvents(f nostr.Filter) (uint32, error) {
	n, err := m.BoltBackend.CountEvents(f)
	if err != nil {
		return n, err
	}
	return n + 1, nil // claim one more than QueryEvents will actually yield
}

func TestBackfillCoordTombstones_CountMismatchDoesNotSetMarker(t *testing.T) {
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

	mismatched := mismatchedCountStore{BoltBackend: h.bolt}
	err := backfillCoordTombstones(context.Background(), mismatched, h.mgmt, h.purger)
	if err == nil {
		t.Fatal("a Count/Query mismatch must be surfaced as an error")
	}

	done, derr := h.mgmt.MigrationDone(migrationCoordTombstonesV1)
	if derr != nil {
		t.Fatal(derr)
	}
	if done {
		t.Fatal("marker must not be set when the drained count disagreed with CountEvents")
	}
}

// TestBackfillCoordTombstones_CancelledContextDoesNotSetMarker: main.go
// cancels the backfill's context on the same SIGINT/SIGTERM that triggers
// graceful shutdown. A run that observes cancellation must stop, return an
// error, and never record the completion marker.
func TestBackfillCoordTombstones_CancelledContextDoesNotSetMarker(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the scan's per-event loop runs

	err := backfillCoordTombstones(ctx, h.bolt, h.mgmt, h.purger)
	if err == nil {
		t.Fatal("a cancelled context must abort the backfill with an error")
	}

	done, derr := h.mgmt.MigrationDone(migrationCoordTombstonesV1)
	if derr != nil {
		t.Fatal(derr)
	}
	if done {
		t.Fatal("marker must not be set when the run was cancelled")
	}
	if !h.inBolt(v1.ID) {
		t.Fatal("a run cancelled before processing any event must not have purged v1")
	}
}

// TestApplyCoordDeletions_CtxCancelledBetweenCoordinatesStopsEarly exercises
// the per-coordinate ctx check added to applyCoordDeletions (purge.go): a
// single kind-5 event with two same-author 'a' tags, where cancellation is
// triggered, deterministically and without any sleep/timing, as a side
// effect of finishing the FIRST coordinate's purge. The second coordinate
// must never be reached.
func TestApplyCoordDeletions_CtxCancelledBetweenCoordinatesStopsEarly(t *testing.T) {
	db := openTestBolt(t)
	mgmt := &ManagementStore{}
	if err := mgmt.Init(db.DB); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelOnDelete := contentType{
		kinds:    []nostr.Kind{30142},
		validate: func(nostr.Event) (bool, string) { return false, "" },
		store:    func(nostr.Event) {},
		fetch:    func(nostr.Filter, int) iter.Seq[nostr.Event] { return func(func(nostr.Event) bool) {} },
		count:    func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(nostr.ID) error { cancel(); return nil },
	}
	reg := newRegistry(cancelOnDelete, deletionContentType(db, map[nostr.Kind]bool{30142: true}))
	purger := &eventPurger{bolt: db, mgmt: mgmt, content: &ContentStore{DB: db.DB}, reg: reg, logf: t.Logf}

	sk := nostr.Generate()
	v0 := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "res-0"}})
	v1 := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "res-1"}})
	for _, ev := range []nostr.Event{v0, v1} {
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{
		{"a", coordOf(v0)}, {"a", coordOf(v1)}, {"k", "30142"},
	})

	tombstoned, purged, errs := purger.applyCoordDeletions(ctx, del)
	if tombstoned != 1 || purged != 1 || errs != 0 {
		t.Fatalf("got (tombstoned=%d, purged=%d, errs=%d), want exactly the first coordinate processed (1, 1, 0) before cancellation stopped the loop",
			tombstoned, purged, errs)
	}
	if _, ok := mgmt.CoordDeletedUntil(coordOf(v0)); !ok {
		t.Fatal("the first coordinate should have been tombstoned before cancellation took effect")
	}
	if _, ok := mgmt.CoordDeletedUntil(coordOf(v1)); ok {
		t.Fatal("the second coordinate must not have been reached after cancellation")
	}
}

// TestBackfillCoordTombstones_CancelMidRunJoinsPromptlyAndSkipsMarker mirrors
// main.go's goroutine + WaitGroup join pattern: the backfill runs in its own
// goroutine, the test cancels its context while that goroutine is
// synchronously blocked mid-purge (via a channel handshake, not a sleep —
// deterministic), and then joins the goroutine the same bounded way main.go
// does. The goroutine must exit, the join must complete well within the
// bound, and the marker must not be set.
func TestBackfillCoordTombstones_CancelMidRunJoinsPromptlyAndSkipsMarker(t *testing.T) {
	db := openTestBolt(t)
	mgmt := &ManagementStore{}
	if err := mgmt.Init(db.DB); err != nil {
		t.Fatal(err)
	}

	midPurge := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	blockOnFirstDelete := contentType{
		kinds:    []nostr.Kind{30142},
		validate: func(nostr.Event) (bool, string) { return false, "" },
		store:    func(nostr.Event) {},
		fetch:    func(nostr.Filter, int) iter.Seq[nostr.Event] { return func(func(nostr.Event) bool) {} },
		count:    func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(nostr.ID) error {
			once.Do(func() { close(midPurge) })
			<-proceed
			return nil
		},
	}
	reg := newRegistry(blockOnFirstDelete, deletionContentType(db, map[nostr.Kind]bool{30142: true}))
	purger := &eventPurger{bolt: db, mgmt: mgmt, content: &ContentStore{DB: db.DB}, reg: reg, logf: t.Logf}

	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	del := signedEvent(t, sk, nostr.KindDeletion, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})
	for _, ev := range []nostr.Event{v1, del} {
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = backfillCoordTombstones(ctx, db, mgmt, purger)
	}()

	select {
	case <-midPurge:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill never reached the purge step")
	}
	cancel()       // cancel while the goroutine is synchronously blocked mid-purge
	close(proceed) // let it continue; it must observe the cancellation and stop

	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill goroutine did not exit/join within 5s of cancellation")
	}

	if runErr == nil {
		t.Fatal("a run cancelled mid-flight must return an error")
	}
	done, derr := mgmt.MigrationDone(migrationCoordTombstonesV1)
	if derr != nil {
		t.Fatal(derr)
	}
	if done {
		t.Fatal("marker must not be set when the run was cancelled mid-flight")
	}
}
