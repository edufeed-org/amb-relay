package main

import (
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"go.etcd.io/bbolt"
)

func TestEventCoord(t *testing.T) {
	pk := nostr.GetPublicKey(nostr.Generate())
	cases := []struct {
		name string
		ev   nostr.Event
		want string
		ok   bool
	}{
		{"addressable", nostr.Event{Kind: 30142, PubKey: pk, Tags: nostr.Tags{{"d", "https://x/y"}}}, "30142:" + pk.Hex() + ":https://x/y", true},
		{"addressable missing d is empty d", nostr.Event{Kind: 30023, PubKey: pk}, "30023:" + pk.Hex() + ":", true},
		{"replaceable", nostr.Event{Kind: 0, PubKey: pk, Tags: nostr.Tags{{"d", "ignored"}}}, "0:" + pk.Hex() + ":", true},
		{"regular", nostr.Event{Kind: 16, PubKey: pk}, "", false},
		{"deletion", nostr.Event{Kind: 5, PubKey: pk}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := eventCoord(c.ev)
			if got != c.want || ok != c.ok {
				t.Fatalf("eventCoord = (%q,%v), want (%q,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestCoordTombstone_KeepsMaxAndBlocksOlderOrEqual(t *testing.T) {
	m := openTestMgmt(t)
	sk := nostr.Generate()
	pk := nostr.GetPublicKey(sk)
	coord := "30142:" + pk.Hex() + ":res"

	if _, ok := m.CoordDeletedUntil(coord); ok {
		t.Fatal("fresh store must have no tombstone")
	}
	if err := m.MarkCoordDeleted(coord, 2000); err != nil {
		t.Fatal(err)
	}
	// An older deletion must not lower the tombstone.
	if err := m.MarkCoordDeleted(coord, 1000); err != nil {
		t.Fatal(err)
	}
	if ts, ok := m.CoordDeletedUntil(coord); !ok || ts != 2000 {
		t.Fatalf("tombstone = %d %v, want 2000 true", ts, ok)
	}

	older := signedEvent(t, sk, 30142, 1999, nostr.Tags{{"d", "res"}})
	equal := signedEvent(t, sk, 30142, 2000, nostr.Tags{{"d", "res"}})
	newer := signedEvent(t, sk, 30142, 2001, nostr.Tags{{"d", "res"}})
	otherD := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "other"}})
	for _, c := range []struct {
		ev      nostr.Event
		blocked bool
	}{{older, true}, {equal, true}, {newer, false}, {otherD, false}} {
		if got := m.BlockReason(c.ev) != ""; got != c.blocked {
			t.Errorf("created_at=%d d=%s blocked=%v, want %v", c.ev.CreatedAt, c.ev.Tags.GetD(), got, c.blocked)
		}
	}
}

func TestBlockReason_BansAndDeletions(t *testing.T) {
	m := openTestMgmt(t)
	sk := nostr.Generate()
	ev := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "a"}})
	if r := m.BlockReason(ev); r != "" {
		t.Fatalf("fresh event blocked: %q", r)
	}

	if err := m.MarkEventDeleted(ev.ID.Hex()); err != nil {
		t.Fatal(err)
	}
	if r := m.BlockReason(ev); r != "blocked: event was deleted by its author" {
		t.Fatalf("deleted reason = %q", r)
	}

	ev2 := signedEvent(t, sk, 30142, 1001, nostr.Tags{{"d", "b"}})
	if err := m.BanEvent(ev2.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if r := m.BlockReason(ev2); r != "event is banned" {
		t.Fatalf("banned-event reason = %q", r)
	}

	ev3 := signedEvent(t, sk, 30142, 1002, nostr.Tags{{"d", "c"}})
	if err := m.BanPubKey(ev3.PubKey, "x"); err != nil {
		t.Fatal(err)
	}
	if r := m.BlockReason(ev3); r != "pubkey is banned" {
		t.Fatalf("banned-pubkey reason = %q", r)
	}
}

// BenchmarkBlockReason measures the hot write/read-path gate check. It was
// folded from 4 separate bbolt.View transactions (IsPubKeyBanned,
// IsEventBanned, IsEventDeleted, CoordDeletedUntil) into 1; run with
// -benchmem to see allocs/op alongside ns/op.
func BenchmarkBlockReason(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench-mgmt.db")
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		b.Fatalf("open bbolt: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	mgmt := &ManagementStore{}
	if err := mgmt.Init(db); err != nil {
		b.Fatalf("mgmt init: %v", err)
	}

	sk := nostr.Generate()
	ev := nostr.Event{
		PubKey:    nostr.GetPublicKey(sk),
		CreatedAt: 1_700_000_000,
		Kind:      30142,
		Tags:      nostr.Tags{{"d", "bench"}},
	}
	if err := ev.Sign(sk); err != nil {
		b.Fatalf("sign: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		mgmt.BlockReason(ev)
	}
}
