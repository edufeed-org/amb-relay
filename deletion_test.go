package main

import (
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

func TestValidateDeletion(t *testing.T) {
	served := map[nostr.Kind]bool{30142: true, 30023: true}
	validate := validateDeletion(served)

	cases := []struct {
		name   string
		tags   nostr.Tags
		reject bool
		msg    string
	}{
		{"e tag only", nostr.Tags{{"e", "abc"}}, false, ""},
		{"a tag served kind", nostr.Tags{{"a", "30142:pubkey:d-tag"}}, false, ""},
		{"a tag foreign kind", nostr.Tags{{"a", "1:pubkey:d-tag"}}, true, "deletion references no kind served by this relay"},
		{"a foreign but e present", nostr.Tags{{"a", "1:pubkey:d"}, {"e", "abc"}}, false, ""},
		{"a served among foreign", nostr.Tags{{"a", "1:p:d"}, {"a", "30023:p:d"}}, false, ""},
		{"k tag only, no target", nostr.Tags{{"k", "30142"}}, true, "missing 'e' or 'a' tag referencing the event to delete"},
		{"no tags", nostr.Tags{}, true, "missing 'e' or 'a' tag referencing the event to delete"},
		{"malformed a coord", nostr.Tags{{"a", "notacoord"}}, true, "deletion references no kind served by this relay"},
		{"empty tag value skipped", nostr.Tags{{"e", ""}}, true, "missing 'e' or 'a' tag referencing the event to delete"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := nostr.Event{Kind: nostr.KindDeletion, Tags: c.tags}
			reject, msg := validate(ev)
			if reject != c.reject || msg != c.msg {
				t.Errorf("validate = (%v, %q), want (%v, %q)", reject, msg, c.reject, c.msg)
			}
		})
	}
}

func openTestBolt(t *testing.T) *boltdb.BoltBackend {
	t.Helper()
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "events.db")}
	if err := db.Init(); err != nil {
		t.Fatalf("bolt init: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func signedEvent(t *testing.T, sk nostr.SecretKey, kind nostr.Kind, createdAt nostr.Timestamp, tags nostr.Tags) nostr.Event {
	t.Helper()
	ev := nostr.Event{Kind: kind, CreatedAt: createdAt, Tags: tags}
	if err := ev.Sign(sk); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return ev
}

func TestDeletionContentTypeFetchAndCount(t *testing.T) {
	db := openTestBolt(t)
	sk := nostr.Generate()
	pk := nostr.GetPublicKey(sk)

	target := signedEvent(t, sk, 30142, 1700000000, nostr.Tags{{"d", "res-1"}, {"name", "Resource"}})
	coord := "30142:" + pk.Hex() + ":res-1"
	del := signedEvent(t, sk, nostr.KindDeletion, 1700000001, nostr.Tags{
		{"e", target.ID.Hex()},
		{"a", coord},
		{"k", "30142"},
	})
	for _, ev := range []nostr.Event{target, del} {
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	ct := deletionContentType(db, map[nostr.Kind]bool{30142: true})

	if len(ct.kinds) != 1 || ct.kinds[0] != nostr.KindDeletion {
		t.Fatalf("kinds = %v, want [5]", ct.kinds)
	}

	cases := []struct {
		name   string
		filter nostr.Filter
		want   int
	}{
		{"by kind+author", nostr.Filter{Kinds: []nostr.Kind{5}, Authors: []nostr.PubKey{pk}, Limit: 10}, 1},
		{"by ids", nostr.Filter{IDs: []nostr.ID{del.ID}}, 1},
		{"by #e", nostr.Filter{Kinds: []nostr.Kind{5}, Tags: nostr.TagMap{"e": {target.ID.Hex()}}, Limit: 10}, 1},
		{"by #a", nostr.Filter{Kinds: []nostr.Kind{5}, Tags: nostr.TagMap{"a": {coord}}, Limit: 10}, 1},
		{"by #k", nostr.Filter{Kinds: []nostr.Kind{5}, Tags: nostr.TagMap{"k": {"30142"}}, Limit: 10}, 1},
		{"kind-agnostic clamps to kind 5", nostr.Filter{Limit: 10}, 1},
		{"other author", nostr.Filter{Kinds: []nostr.Kind{5}, Authors: []nostr.PubKey{nostr.GetPublicKey(nostr.Generate())}, Limit: 10}, 0},
		{"ids query must not leak non-deletion events", nostr.Filter{IDs: []nostr.ID{target.ID}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collect(ct.fetch(c.filter, 100))
			if len(got) != c.want {
				t.Fatalf("fetch = %d events, want %d", len(got), c.want)
			}
			for _, ev := range got {
				if ev.Kind != nostr.KindDeletion {
					t.Fatalf("non-kind-5 event leaked: kind %d", ev.Kind)
				}
			}
		})
	}

	if n, err := ct.count(nostr.Filter{Limit: 10}); err != nil || n != 1 {
		t.Fatalf("count = %d %v, want 1 nil", n, err)
	}
	if err := ct.deleteID(del.ID); err != nil {
		t.Fatalf("deleteID must be a nil no-op, got %v", err)
	}
	if ct.chunked {
		t.Fatal("deletions must not be chunked")
	}
}
