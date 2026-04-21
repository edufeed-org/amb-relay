package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bbolt.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := bbolt.Open(filepath.Join(dir, "test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketFetchedContent)
		return err
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return db
}

func TestContentStore_PutGet(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	entry := ContentEntry{
		Text:      "hello world",
		FetchedAt: time.Now().Unix(),
		Status:    ContentStatusFetched,
		SourceURL: "https://example.org/resource",
	}
	if err := cs.Put("event-id-1", entry); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := cs.Get("event-id-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if got.Text != entry.Text || got.Status != entry.Status || got.SourceURL != entry.SourceURL {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, entry)
	}
}

func TestContentStore_GetMissing(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_, ok, err := cs.Get("no-such-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestContentStore_Delete(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_ = cs.Put("k", ContentEntry{Text: "x", Status: ContentStatusFetched})
	if err := cs.Delete("k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, _ := cs.Get("k")
	if ok {
		t.Fatal("expected entry to be gone")
	}
}

func TestContentStore_ForEach(t *testing.T) {
	db := openTestDB(t)
	cs := &ContentStore{DB: db}

	_ = cs.Put("a", ContentEntry{Text: "A"})
	_ = cs.Put("b", ContentEntry{Text: "B"})

	seen := map[string]string{}
	err := cs.ForEach(func(id string, entry ContentEntry) error {
		seen[id] = entry.Text
		return nil
	})
	if err != nil {
		t.Fatalf("foreach: %v", err)
	}
	if seen["a"] != "A" || seen["b"] != "B" || len(seen) != 2 {
		t.Fatalf("unexpected entries: %v", seen)
	}
}

var _ = os.Stdout
