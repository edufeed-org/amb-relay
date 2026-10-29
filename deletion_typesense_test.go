package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Integration tests against a real Typesense. Opt-in, same convention as
// nostrlib's typesense30142 tests:
//
//	docker run --rm -p 8108:8108 typesense/typesense:28.0 --data-dir /tmp --api-key=test-key
//	TS_TEST_HOST=http://localhost:8108 TS_TEST_APIKEY=test-key go test -run Typesense ./...
func tsTestEnv(t *testing.T) (host, key string) {
	t.Helper()
	host, key = os.Getenv("TS_TEST_HOST"), os.Getenv("TS_TEST_APIKEY")
	if host == "" || key == "" {
		t.Skip("TS_TEST_HOST and TS_TEST_APIKEY not set; skipping Typesense integration test")
	}
	return host, key
}

func tsTestBackend(t *testing.T, base string, schema func(string) typesense30142.CollectionSchema, tagFields map[string]string) *typesense30142.TSBackend {
	t.Helper()
	host, key := tsTestEnv(t)
	name := fmt.Sprintf("deltest_%s_%d", base, time.Now().UnixNano())
	sch := schema(name)
	ts := &typesense30142.TSBackend{ApiKey: key, Host: host, CollectionName: name, Schema: &sch, TagFields: tagFields}
	if base == "shares" {
		ts.SearchFields = "eventID" // as main.go: shares carry no fulltext
	}
	if err := ts.Init(); err != nil {
		t.Fatalf("init %s: %v", name, err)
	}
	t.Cleanup(func() { _ = ts.DropCollection() })
	return ts
}

func tsHasID(t *testing.T, ts *typesense30142.TSBackend, id nostr.ID) bool {
	t.Helper()
	for range ts.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 10) {
		return true
	}
	return false
}

// Every collection keys addressable/replaceable docs by coordinate, never by
// event id. deleteID (TSBackend.DeleteEvent, filter_by=eventID:=<id>) must
// still remove them — with no Bolt row behind them, as in production.
func TestTypesense_DeleteByEventIDRemovesCoordinateKeyedDocs(t *testing.T) {
	tsTestEnv(t)
	sk := nostr.Generate()
	pk := nostr.GetPublicKey(sk)
	ambSchema := func(name string) typesense30142.CollectionSchema {
		s := typesense30142.DefaultSchema()
		s.Name = name
		ensureContentFields(&s)
		return s
	}
	cases := []struct {
		name   string
		schema func(string) typesense30142.CollectionSchema
		tags   map[string]string
		store  func(*typesense30142.TSBackend, nostr.Event)
		ev     nostr.Event
	}{
		{"amb", ambSchema, nil,
			func(ts *typesense30142.TSBackend, e nostr.Event) { _, _ = ts.ReplaceEvent(e) },
			signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://material.rpi-virtuell.de/material/x/"}, {"name", "Res"}, {"type", "LearningResource"}})},
		{"longform", longformSchema, nil,
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeLongform(true, ts, e) },
			signedEvent(t, sk, 30023, 1000, nostr.Tags{{"d", "post"}, {"title", "T"}})},
		{"wiki", wikiSchema, nil,
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeWiki(true, ts, e) },
			signedEvent(t, sk, 30818, 1000, nostr.Tags{{"d", "page"}, {"title", "T"}})},
		{"calendar", calendarSchema, calendarTagFields(),
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeCalendar(true, ts, e) },
			signedEvent(t, sk, 31922, 1000, nostr.Tags{{"d", "evt"}, {"title", "T"}, {"start", "2026-11-01"}})},
		{"profiles", profileSchema, nil,
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeProfile(true, ts, e, false) },
			func() nostr.Event {
				e := nostr.Event{Kind: 0, CreatedAt: 1000, Content: `{"name":"n"}`}
				_ = e.Sign(sk)
				return e
			}()},
		{"shares", sharesSchema, sharesTagFields(),
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeShare(true, ts, e) },
			signedEvent(t, sk, 30222, 1000, nostr.Tags{{"d", "share"}, {"a", "30142:" + pk.Hex() + ":x"}, {"h", pk.Hex()}})},
		{"transferkiosk", transferkioskSchema, transferkioskTagFields(),
			func(ts *typesense30142.TSBackend, e nostr.Event) { storeTransferkiosk(true, ts, e) },
			signedEvent(t, sk, 30143, 1000, nostr.Tags{{"d", "proj"}, {"name", "P"}})},
		{"publications", publicationsSchema, nil,
			func(ts *typesense30142.TSBackend, e nostr.Event) { storePublication(true, ts, e) },
			signedEvent(t, sk, 30040, 1000, nostr.Tags{{"d", "pub"}, {"title", "T"}})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := tsTestBackend(t, c.name, c.schema, c.tags)
			c.store(ts, c.ev)
			if !tsHasID(t, ts, c.ev.ID) {
				t.Fatalf("setup: %s doc not indexed", c.name)
			}
			if err := ts.DeleteEvent(c.ev.ID); err != nil {
				t.Fatalf("DeleteEvent: %v", err)
			}
			if tsHasID(t, ts, c.ev.ID) {
				t.Fatalf("%s: coordinate-keyed doc survived delete-by-eventID", c.name)
			}
		})
	}
}

// The production failure mode, end to end on the real AMB write path: an
// event's projection is still queued in the TSWriteBuffer when banevent runs.
// The direct delete finds nothing yet; the gated flush must not re-create the
// doc, and QueueDelete must remove one that was already flushed.
func TestTypesense_BanDuringQueuedProjectionDoesNotResurrect(t *testing.T) {
	ambSchema := func(name string) typesense30142.CollectionSchema {
		s := typesense30142.DefaultSchema()
		s.Name = name
		ensureContentFields(&s)
		return s
	}
	ts := tsTestBackend(t, "ambbuf", ambSchema, nil)
	m := openTestMgmt(t)
	sk := nostr.Generate()

	queued := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://x/queued/"}, {"name", "Q"}, {"type", "LearningResource"}})
	kept := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://x/kept/"}, {"name", "K"}, {"type", "LearningResource"}})

	buf := NewTSWriteBuffer(ts, 100, time.Hour, m.isBlocked) // no timed flush: stays queued
	buf.Queue(queued)
	buf.Queue(kept)

	// banevent while the projection is queued: ban first, delete directly.
	if err := m.BanEvent(queued.ID, "incident"); err != nil {
		t.Fatal(err)
	}
	if err := ts.DeleteEvent(queued.ID); err != nil {
		t.Fatal(err)
	}
	buf.QueueDelete(queued.ID)
	buf.Close() // drains: flush (gated) then the queued delete

	if tsHasID(t, ts, queued.ID) {
		t.Fatal("banned event re-created by a projection queued before the ban")
	}
	if !tsHasID(t, ts, kept.ID) {
		t.Fatal("unrelated queued event must still be indexed")
	}
}

// Sanity: the AMB doc id really is the coordinate, so this suite exercises
// the case the incident report suspected.
func TestTypesense_AMBDocIDIsCoordinate(t *testing.T) {
	sk := nostr.Generate()
	ev := signedEvent(t, sk, 30142, 1000, nostr.Tags{{"d", "https://x/y/"}})
	id, err := tsDocIDFromEvent(ev)
	if err != nil || !strings.HasPrefix(id, ev.PubKey.Hex()+":") || strings.Contains(id, ev.ID.Hex()) {
		t.Fatalf("doc id = %q (%v), want <pubkey>:<d>", id, err)
	}
}
