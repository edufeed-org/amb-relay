package main

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"go.etcd.io/bbolt"
)

// openTestMgmt opens a fresh bbolt database in t.TempDir() and returns an
// initialized ManagementStore. The DB is closed automatically via t.Cleanup.
func openTestMgmt(t *testing.T) *ManagementStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mgmt.db")
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mgmt := &ManagementStore{}
	if err := mgmt.Init(db); err != nil {
		t.Fatalf("mgmt init: %v", err)
	}
	return mgmt
}

func TestNeedsRefetch_MarkListRemove(t *testing.T) {
	mgmt := openTestMgmt(t)

	// Empty initially.
	ids, err := mgmt.ListNeedsRefetch()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("want empty list, got %v", ids)
	}

	// Mark two ids.
	for _, id := range []string{"aaaa", "bbbb"} {
		if err := mgmt.MarkNeedsRefetch(id); err != nil {
			t.Fatalf("Mark %q: %v", id, err)
		}
	}

	ids, err = mgmt.ListNeedsRefetch()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(ids)
	if got, want := ids, []string{"aaaa", "bbbb"}; !equalStringSlices(got, want) {
		t.Fatalf("List after marks = %v, want %v", got, want)
	}

	// Marking the same id again is idempotent.
	if err := mgmt.MarkNeedsRefetch("aaaa"); err != nil {
		t.Fatalf("Mark idempotent: %v", err)
	}
	ids, _ = mgmt.ListNeedsRefetch()
	if len(ids) != 2 {
		t.Fatalf("idempotent mark changed length: %v", ids)
	}

	// Remove one.
	if err := mgmt.RemoveNeedsRefetch("aaaa"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ids, _ = mgmt.ListNeedsRefetch()
	sort.Strings(ids)
	if got, want := ids, []string{"bbbb"}; !equalStringSlices(got, want) {
		t.Fatalf("List after remove = %v, want %v", got, want)
	}

	// Removing a non-existent id is a no-op.
	if err := mgmt.RemoveNeedsRefetch("cccc"); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}

	// Remove the last one; list empty.
	if err := mgmt.RemoveNeedsRefetch("bbbb"); err != nil {
		t.Fatalf("Remove last: %v", err)
	}
	ids, _ = mgmt.ListNeedsRefetch()
	if len(ids) != 0 {
		t.Fatalf("want empty after all removes, got %v", ids)
	}
}

func TestBanEvent_IsEventBanned(t *testing.T) {
	mgmt := openTestMgmt(t)

	id, err := nostr.IDFromHex("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("IDFromHex: %v", err)
	}

	if mgmt.IsEventBanned(id) {
		t.Fatal("fresh store: id should not be banned")
	}

	if err := mgmt.BanEvent(id, "spam"); err != nil {
		t.Fatalf("BanEvent: %v", err)
	}
	if !mgmt.IsEventBanned(id) {
		t.Fatal("after BanEvent: id should be banned")
	}

	if err := mgmt.AllowEvent(id); err != nil {
		t.Fatalf("AllowEvent: %v", err)
	}
	if mgmt.IsEventBanned(id) {
		t.Fatal("after AllowEvent: id should not be banned")
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDeletedEvents_MarkAndCheck(t *testing.T) {
	mgmt := openTestMgmt(t)
	id := "5f2ad0c3aa0000000000000000000000000000000000000000000000000000ab"

	if mgmt.IsEventDeleted(id) {
		t.Fatal("unmarked id reported deleted")
	}
	if err := mgmt.MarkEventDeleted(id); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !mgmt.IsEventDeleted(id) {
		t.Fatal("marked id not reported deleted")
	}
	// Idempotent: re-marking is a no-op, not an error.
	if err := mgmt.MarkEventDeleted(id); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if mgmt.IsEventDeleted("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("different id reported deleted")
	}
}
