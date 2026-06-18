package main

import (
	"os"
	"sort"
	"testing"

	"go.etcd.io/bbolt"
)

func newTestMgmt(t *testing.T) *ManagementStore {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "mgmt-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := bbolt.Open(f.Name(), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var m ManagementStore
	if err := m.Init(db); err != nil {
		t.Fatalf("mgmt init: %v", err)
	}
	return &m
}

func TestProfileQueueEnqueueDedupRemove(t *testing.T) {
	m := newTestMgmt(t)
	if err := m.EnqueueProfileCandidate("aa"); err != nil {
		t.Fatal(err)
	}
	if err := m.EnqueueProfileCandidate("aa"); err != nil { // dedup: idempotent Put
		t.Fatal(err)
	}
	if err := m.EnqueueProfileCandidate("bb"); err != nil {
		t.Fatal(err)
	}
	got, err := m.ListProfileQueue()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "aa" || got[1] != "bb" {
		t.Fatalf("queue = %v, want [aa bb]", got)
	}
	if err := m.RemoveProfileCandidate("aa"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.ListProfileQueue()
	if len(got) != 1 || got[0] != "bb" {
		t.Fatalf("after remove, queue = %v, want [bb]", got)
	}
}
