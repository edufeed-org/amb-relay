package main

import (
	"context"
	"iter"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
)

// fakeCollection models a Typesense collection the way the real ones behave
// for deletion: docs are keyed by COORDINATE (docIDFor), upserted in place,
// and deleted by matching the eventID field — never by doc id.
type fakeCollection struct {
	mu   sync.Mutex
	docs map[string]nostr.Event
}

func newFakeCollection() *fakeCollection { return &fakeCollection{docs: map[string]nostr.Event{}} }

func (c *fakeCollection) put(ev nostr.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.docs[docIDFor(ev.Kind, ev.PubKey.Hex(), ev.Tags.GetD())] = ev
}

func (c *fakeCollection) has(id nostr.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.docs {
		if ev.ID == id {
			return true
		}
	}
	return false
}

func (c *fakeCollection) contentType(kinds ...nostr.Kind) contentType {
	return contentType{
		kinds:    kinds,
		validate: func(nostr.Event) (bool, string) { return false, "" },
		store:    c.put,
		fetch: func(f nostr.Filter, max int) iter.Seq[nostr.Event] {
			c.mu.Lock()
			var out []nostr.Event
			for _, ev := range c.docs {
				if f.Matches(ev) {
					out = append(out, ev)
				}
			}
			c.mu.Unlock()
			return func(yield func(nostr.Event) bool) {
				for _, ev := range out {
					if !yield(ev) {
						return
					}
				}
			}
		},
		count: func(nostr.Filter) (uint32, error) { return 0, nil },
		deleteID: func(id nostr.ID) error { // filter_by=eventID:=<id>
			c.mu.Lock()
			defer c.mu.Unlock()
			for k, ev := range c.docs {
				if ev.ID == id {
					delete(c.docs, k)
				}
			}
			return nil
		},
	}
}

// deletionHarness wires a real khatru relay exactly like main.go's deletion
// path: OnEvent gate, StoreEvent → applyCoordDeletions for kind 5,
// DeleteEvent → authorDelete, AllowDeleting, gated QueryStored, BanEvent →
// ban. Bolt is real; the search collections are fakeCollections.
type deletionHarness struct {
	bolt   *boltdb.BoltBackend
	mgmt   *ManagementStore
	amb    *fakeCollection
	other  *fakeCollection // a second coordinate-keyed collection (long-form)
	purger *eventPurger
	client *nostr.Relay
	ctx    context.Context
}

func newDeletionHarness(t *testing.T) *deletionHarness {
	t.Helper()
	h := &deletionHarness{bolt: openTestBolt(t), amb: newFakeCollection(), other: newFakeCollection()}
	h.mgmt = &ManagementStore{}
	if err := h.mgmt.Init(h.bolt.DB); err != nil {
		t.Fatal(err)
	}
	served := map[nostr.Kind]bool{30142: true, 30023: true}
	reg := newRegistry(h.amb.contentType(30142), h.other.contentType(30023), deletionContentType(h.bolt, served))
	h.purger = &eventPurger{bolt: h.bolt, mgmt: h.mgmt, content: &ContentStore{DB: h.bolt.DB}, reg: reg, logf: t.Logf}

	relay := khatru.NewRelay()
	relay.OnEvent = func(ctx context.Context, ev nostr.Event) (bool, string) {
		if r := h.mgmt.BlockReason(ev); r != "" {
			return true, r
		}
		return reg.validate(ev)
	}
	relay.StoreEvent = func(ctx context.Context, ev nostr.Event) error {
		if err := h.bolt.SaveEvent(ev); err != nil {
			return err
		}
		reg.store(ev)
		h.purger.applyCoordDeletions(ctx, ev)
		return nil
	}
	relay.ReplaceEvent = func(ctx context.Context, ev nostr.Event) error {
		if _, err := h.bolt.ReplaceEvent(ev); err != nil {
			return err
		}
		reg.store(ev)
		return nil
	}
	relay.QueryStored = func(ctx context.Context, f nostr.Filter) iter.Seq[nostr.Event] {
		return gateSeq(reg.fetch(f, 250), h.mgmt.isBlocked)
	}
	relay.DeleteEvent = h.purger.authorDelete
	relay.AllowDeleting = func(ctx context.Context, target, del nostr.Event) bool {
		return target.Kind != nostr.KindDeletion && target.PubKey == del.PubKey
	}

	srv := httptest.NewServer(relay)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h.ctx = ctx
	c, err := nostr.RelayConnect(ctx, "ws"+srv.URL[4:], nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	h.client = c
	return h
}

func (h *deletionHarness) publish(t *testing.T, ev nostr.Event) error {
	t.Helper()
	return h.client.Publish(h.ctx, ev)
}

func (h *deletionHarness) mustPublish(t *testing.T, ev nostr.Event) {
	t.Helper()
	if err := h.publish(t, ev); err != nil {
		t.Fatalf("publish kind %d: %v", ev.Kind, err)
	}
}

func (h *deletionHarness) inBolt(id nostr.ID) bool {
	for range h.bolt.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
		return true
	}
	return false
}

func (h *deletionHarness) served(t *testing.T, f nostr.Filter) []nostr.Event {
	t.Helper()
	var out []nostr.Event
	for ev := range h.client.QueryEvents(f) {
		out = append(out, ev)
	}
	return out
}

func coordOf(ev nostr.Event) string { c, _ := eventCoord(ev); return c }

func TestDeletion_ETagOnAddressableRemovesEverywhere(t *testing.T) {
	h := newDeletionHarness(t)
	sk := nostr.Generate()
	res := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://material.example/x/"}})
	h.mustPublish(t, res)
	if !h.inBolt(res.ID) || !h.amb.has(res.ID) {
		t.Fatal("setup: resource must be in bolt and the collection")
	}

	h.mustPublish(t, signedEvent(t, sk, 5, 2000, nostr.Tags{{"e", res.ID.Hex()}, {"k", "30142"}}))

	if h.inBolt(res.ID) || h.amb.has(res.ID) {
		t.Fatalf("e-tag deletion left the event behind (bolt=%v ts=%v)", h.inBolt(res.ID), h.amb.has(res.ID))
	}
	if got := h.served(t, nostr.Filter{Kinds: []nostr.Kind{30142}}); len(got) != 0 {
		t.Fatalf("deleted resource still served: %d", len(got))
	}
	if err := h.publish(t, res); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("re-publishing the deleted event must be rejected, got %v", err)
	}
}

func TestDeletion_ATagRemovesCurrentVersionAndTombstones(t *testing.T) {
	h := newDeletionHarness(t)
	sk := nostr.Generate()
	tags := nostr.Tags{{"d", "res"}}
	v1 := signedEvent(t, sk, 30142, 1000, tags)
	h.mustPublish(t, v1)
	del := signedEvent(t, sk, 5, 2000, nostr.Tags{{"a", coordOf(v1)}, {"k", "30142"}})
	h.mustPublish(t, del)

	if h.inBolt(v1.ID) || h.amb.has(v1.ID) {
		t.Fatalf("a-tag deletion left v1 (bolt=%v ts=%v)", h.inBolt(v1.ID), h.amb.has(v1.ID))
	}
	// NIP-09: the relay keeps serving the deletion request.
	if got := h.served(t, nostr.Filter{Kinds: []nostr.Kind{5}, Authors: []nostr.PubKey{del.PubKey}}); len(got) != 1 || got[0].ID != del.ID {
		t.Fatalf("deletion request not served: %v", got)
	}

	for _, ts := range []nostr.Timestamp{1000, 1500, 2000} {
		old := signedEvent(t, sk, 30142, ts, tags)
		if err := h.publish(t, old); err == nil || !strings.Contains(err.Error(), "deleted") {
			t.Fatalf("version at %d (<= deletion) must be rejected, got %v", ts, err)
		}
	}

	newer := signedEvent(t, sk, 30142, 2001, tags)
	h.mustPublish(t, newer)
	if got := h.served(t, nostr.Filter{Kinds: []nostr.Kind{30142}}); len(got) != 1 || got[0].ID != newer.ID {
		t.Fatalf("newer version must be accepted and served, got %v", got)
	}
}

func TestDeletion_ATagKeepsVersionNewerThanRequest(t *testing.T) {
	h := newDeletionHarness(t)
	sk := nostr.Generate()
	v := signedEvent(t, sk, 30142, 3000, nostr.Tags{{"d", "res"}})
	h.mustPublish(t, v)
	h.mustPublish(t, signedEvent(t, sk, 5, 2000, nostr.Tags{{"a", coordOf(v)}}))
	if !h.inBolt(v.ID) || !h.amb.has(v.ID) {
		t.Fatal("a version newer than the deletion request must survive it")
	}
}

func TestDeletion_ATagOfAnotherAuthorIsIgnored(t *testing.T) {
	h := newDeletionHarness(t)
	victim, attacker := nostr.Generate(), nostr.Generate()
	v := signedEvent(t, victim, 30142, 1000, nostr.Tags{{"d", "res"}})
	h.mustPublish(t, v)
	// Accepted or not by khatru, it must not delete or tombstone anything.
	_ = h.publish(t, signedEvent(t, attacker, 5, 2000, nostr.Tags{{"a", coordOf(v)}}))

	if !h.inBolt(v.ID) || !h.amb.has(v.ID) {
		t.Fatal("cross-author a-tag deleted the victim's event")
	}
	if _, ok := h.mgmt.CoordDeletedUntil(coordOf(v)); ok {
		t.Fatal("cross-author a-tag recorded a tombstone")
	}
}

// The 2026-10-29 incident: the ban is recorded and the Bolt row is gone, but
// the coordinate-keyed doc survived in the search collection. The read gate
// hides it immediately; re-calling banevent then removes it.
func TestBan_AddressableLeftoverInCollectionOnly(t *testing.T) {
	h := newDeletionHarness(t)
	sk := nostr.Generate()
	res := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://material.rpi-virtuell.de/material/x/"}})
	h.amb.put(res) // only in the collection, as in production
	if err := h.mgmt.BanEvent(res.ID, "incident"); err != nil {
		t.Fatal(err)
	}

	if got := h.served(t, nostr.Filter{Kinds: []nostr.Kind{30142}}); len(got) != 0 {
		t.Fatalf("banned leftover served: %d", len(got))
	}
	if got := h.served(t, nostr.Filter{IDs: []nostr.ID{res.ID}}); len(got) != 0 {
		t.Fatalf("banned leftover served by id: %d", len(got))
	}

	if err := h.purger.ban(h.ctx, res.ID, "incident"); err != nil {
		t.Fatal(err)
	}
	if h.amb.has(res.ID) {
		t.Fatal("re-calling banevent must delete the leftover doc by eventID")
	}
	if err := h.publish(t, res); err == nil || !strings.Contains(err.Error(), "banned") {
		t.Fatalf("re-publishing a banned event must be rejected, got %v", err)
	}
}

func TestBan_RemovesFromBoltContentAndEveryCollection(t *testing.T) {
	h := newDeletionHarness(t)
	sk := nostr.Generate()
	res := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "res"}})
	article := signedEvent(t, sk, 30023, 1000, nostr.Tags{{"d", "post"}, {"title", "t"}})
	h.mustPublish(t, res)
	h.mustPublish(t, article)
	cs := &ContentStore{DB: h.bolt.DB}
	if err := cs.Put(res.ID.Hex(), ContentEntry{Text: "fulltext"}); err != nil {
		t.Fatal(err)
	}

	for _, ev := range []nostr.Event{res, article} {
		if err := h.purger.ban(h.ctx, ev.ID, "spam"); err != nil {
			t.Fatal(err)
		}
	}
	if h.inBolt(res.ID) || h.inBolt(article.ID) || h.amb.has(res.ID) || h.other.has(article.ID) {
		t.Fatal("banevent left an addressable event in bolt or a collection")
	}
	if _, ok, _ := cs.Get(res.ID.Hex()); ok {
		t.Fatal("banevent left the content-store row")
	}
	if err := h.publish(t, res); err == nil || !strings.Contains(err.Error(), "banned") {
		t.Fatalf("re-publishing a banned event must be rejected, got %v", err)
	}
}

func TestGatedRawStore_SkipsBlockedSaves(t *testing.T) {
	db := openTestBolt(t)
	m := &ManagementStore{}
	if err := m.Init(db.DB); err != nil {
		t.Fatal(err)
	}
	sk := nostr.Generate()
	banned := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "a"}})
	ok := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "b"}})
	if err := m.BanEvent(banned.ID, "x"); err != nil {
		t.Fatal(err)
	}
	g := gatedRawStore{hydrateStore: db, blocked: m.isBlocked}
	for _, ev := range []nostr.Event{banned, ok} {
		if err := g.SaveEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	var got []nostr.ID
	for ev := range db.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{30142}}, 10) {
		got = append(got, ev.ID)
	}
	if len(got) != 1 || got[0] != ok.ID {
		t.Fatalf("bolt after gated hydrate = %v, want only the unbanned event", got)
	}
}

// A reindex reading Bolt concurrently with a ban must not re-project the
// banned event (the walk may have read it before the purge removed it).
func TestReindexStructured_SkipsBlockedEvents(t *testing.T) {
	db := openTestBolt(t)
	m := &ManagementStore{}
	if err := m.Init(db.DB); err != nil {
		t.Fatal(err)
	}
	sk := nostr.Generate()
	banned := signedEvent(t, sk, 30023, 1000, nostr.Tags{{"d", "a"}, {"title", "t"}})
	kept := signedEvent(t, sk, 30023, 1000, nostr.Tags{{"d", "b"}, {"title", "t"}})
	for _, ev := range []nostr.Event{banned, kept} {
		if err := db.SaveEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.BanEvent(banned.ID, "x"); err != nil { // banned, Bolt row not yet purged
		t.Fatal(err)
	}
	var projected []nostr.ID
	r := &Reindexer{boltDB: db, mgmt: m}
	r.reindexStructured(structuredReindexTarget{
		label:     "longform",
		kinds:     []nostr.Kind{30023},
		recreate:  func() error { return nil },
		reproject: func(ev nostr.Event) error { projected = append(projected, ev.ID); return nil },
	})
	if len(projected) != 1 || projected[0] != kept.ID {
		t.Fatalf("reprojected %v, want only the unbanned event", projected)
	}
}
