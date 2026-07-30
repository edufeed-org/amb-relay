package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// reprojectStructured returns the project error (no HTTP attempted) when the
// event cannot be projected — a long-form event without a d-tag.
func TestReprojectStructured_ProjectErrorNoHTTP(t *testing.T) {
	evt := nostr.Event{Kind: 30023, Tags: nostr.Tags{{"title", "x"}}}
	evt.ID = evt.GetID()
	if err := reprojectStructured(nil, evt, nostrToLongform); err == nil {
		t.Fatal("expected project error for missing d-tag, got nil")
	}
}

// reprojectStructured upserts the projected doc and returns nil on a 200.
func TestReprojectStructured_HappyPath(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	ts := &typesense30142.TSBackend{Host: srv.URL, CollectionName: "c", ApiKey: "k"}

	evt := nostr.Event{
		Kind:   30023,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002"),
		Tags:   nostr.Tags{{"d", "a"}, {"title", "T"}},
	}
	evt.ID = evt.GetID()
	if err := reprojectStructured(ts, evt, nostrToLongform); err != nil {
		t.Fatalf("reprojectStructured: %v", err)
	}
	if !strings.Contains(string(gotBody), `"d":"a"`) {
		t.Errorf("upsert body missing projected d: %s", gotBody)
	}
}

// reindexStructuredEvents reprojects every event, counts results, and continues
// past a failing event (parity with the AMB batch path's per-error counting).
func TestReindexStructuredEvents_CountsAndContinues(t *testing.T) {
	events := []nostr.Event{
		{Kind: 30023, Tags: nostr.Tags{{"d", "a"}}},
		{Kind: 30023, Tags: nostr.Tags{{"d", "bad"}}},
		{Kind: 30023, Tags: nostr.Tags{{"d", "c"}}},
	}
	for i := range events {
		events[i].ID = events[i].GetID()
	}
	seq := func(yield func(nostr.Event) bool) {
		for _, e := range events {
			if !yield(e) {
				return
			}
		}
	}
	var seen []string
	reproject := func(e nostr.Event) error {
		d := e.Tags.GetD()
		seen = append(seen, d)
		if d == "bad" {
			return errors.New("boom")
		}
		return nil
	}
	total, indexed, superseded, errs := reindexStructuredEvents("longform", seq, reproject)
	if total != 3 || indexed != 2 || superseded != 0 || errs != 1 {
		t.Fatalf("total=%d indexed=%d superseded=%d errs=%d, want 3/2/0/1", total, indexed, superseded, errs)
	}
	if !slices.Equal(seen, []string{"a", "bad", "c"}) {
		t.Errorf("reproject call order = %v, want [a bad c]", seen)
	}
}

// TestReindexerAfterRunCallbackFires verifies that the afterRun callback is
// invoked when runAfter() is called.
func TestReindexerAfterRunCallbackFires(t *testing.T) {
	called := false
	r := &Reindexer{afterRun: func() { called = true }}
	r.runAfter()
	if !called {
		t.Fatal("afterRun must be invoked by runAfter")
	}
}

// classifyOrphans keeps any candidate row whose event still exists in BoltDB
// under any kind — only rows whose event is gone entirely are orphans.
func TestClassifyOrphans(t *testing.T) {
	kinds := map[string]nostr.Kind{
		"aaa": 30040, // publication content row — must NOT be deleted
		"bbb": 30142, // present in bolt → not orphan
	}
	kindOf := func(id string) (nostr.Kind, bool) {
		k, ok := kinds[id]
		return k, ok
	}
	orphans := classifyOrphans([]string{"aaa", "bbb", "ccc"}, kindOf)
	// Only "ccc" (event gone from BoltDB entirely) is an orphan.
	if len(orphans) != 1 || orphans[0] != "ccc" {
		t.Errorf("orphans = %v, want [ccc]", orphans)
	}
}

// reindexStructured must invoke after() once and fold its counters in.
func TestReindexStructuredAfterHookCounters(t *testing.T) {
	called := 0
	r := &Reindexer{}
	tgt := structuredReindexTarget{
		label:     "publications",
		kinds:     []nostr.Kind{30040},
		recreate:  func() error { return nil },
		reproject: func(nostr.Event) error { return nil },
		after: func() (int64, int64) {
			called++
			return 3, 1
		},
	}
	bb := openTestBolt(t)
	r.boltDB = bb
	r.reindexStructured(tgt)
	if called != 1 {
		t.Fatalf("after called %d times, want 1", called)
	}
	if r.contentPatched.Load() != 3 || r.errors.Load() != 1 {
		t.Errorf("counters = patched %d errs %d, want 3/1", r.contentPatched.Load(), r.errors.Load())
	}
}

// versioned builds an addressable event at a given created_at under one address.
func versioned(kind nostr.Kind, d string, createdAt nostr.Timestamp) nostr.Event {
	e := nostr.Event{Kind: kind, CreatedAt: createdAt, Tags: nostr.Tags{{"d", d}}}
	e.ID = e.GetID()
	return e
}

// The nostrlib#4 regression on the structured path. BoltDB can hold several
// versions of one addressable document and the walk is newest-first, so the
// unconditional upsert used to reproject the OLDEST version last and serve it.
// Only the newest version may reach reproject.
func TestReindexStructuredEvents_ResolvesAddressableVersions(t *testing.T) {
	events := []nostr.Event{
		versioned(30023, "same-address", 1785000556), // newest, walked first
		versioned(30023, "same-address", 1779543035),
		versioned(30023, "same-address", 1778576388), // oldest, used to win
		versioned(30023, "other-address", 1778576388),
	}
	var projected []nostr.Timestamp
	reproject := func(e nostr.Event) error {
		projected = append(projected, e.CreatedAt)
		return nil
	}

	total, indexed, superseded, errs := reindexStructuredEvents("longform", seqOf(events...), reproject)

	if total != 4 || indexed != 2 || superseded != 2 || errs != 0 {
		t.Fatalf("total=%d indexed=%d superseded=%d errs=%d, want 4/2/2/0", total, indexed, superseded, errs)
	}
	want := []nostr.Timestamp{1785000556, 1778576388}
	if !slices.Equal(projected, want) {
		t.Errorf("projected created_at = %v, want %v (newest of the duplicated address, plus the untouched one)", projected, want)
	}
}

// Order-independence: the walk order of an event store is not part of its
// contract, so an oldest-first walk must leave the same version served. Every
// version is reprojected here (each supersedes the last) and the final write —
// the one Typesense keeps — is the newest.
func TestReindexStructuredEvents_OldestFirstWalkStillServesNewest(t *testing.T) {
	events := []nostr.Event{
		versioned(30023, "a", 1778576388),
		versioned(30023, "a", 1785000556),
	}
	var projected []nostr.Timestamp
	_, indexed, superseded, _ := reindexStructuredEvents("longform", seqOf(events...), func(e nostr.Event) error {
		projected = append(projected, e.CreatedAt)
		return nil
	})

	if indexed != 2 || superseded != 0 {
		t.Fatalf("indexed=%d superseded=%d, want 2/0", indexed, superseded)
	}
	if projected[len(projected)-1] != 1785000556 {
		t.Errorf("last write = %d, want the newest version 1785000556", projected[len(projected)-1])
	}
}

// Non-addressable events in a structured collection key on their own event hex
// id, so each is its own document and none may be skipped as superseded. Kind
// 16 shares live in the same collection as kind 30222 and would otherwise
// collapse onto one another.
func TestReindexStructuredEvents_NonAddressableNeverSuperseded(t *testing.T) {
	var events []nostr.Event
	for i, ts := range []nostr.Timestamp{1785000556, 1779543035, 1778576388} {
		e := nostr.Event{Kind: 16, CreatedAt: ts, Tags: nostr.Tags{{"k", "30142"}}}
		e.Content = string(rune('a' + i)) // distinct ids
		e.ID = e.GetID()
		events = append(events, e)
	}

	total, indexed, superseded, errs := reindexStructuredEvents("shares", seqOf(events...), func(nostr.Event) error { return nil })

	if total != 3 || indexed != 3 || superseded != 0 || errs != 0 {
		t.Fatalf("total=%d indexed=%d superseded=%d errs=%d, want 3/3/0/0", total, indexed, superseded, errs)
	}
}

// Collections shared by several kinds fold the kind into the document id, so
// the same (pubkey, d) under two kinds are two documents and neither
// supersedes the other. Calendar 31922/31923 is the live case — the relay holds
// forked pairs at one coordinate.
func TestReindexStructuredEvents_SameAddressDifferentKindsAreDistinctDocs(t *testing.T) {
	events := []nostr.Event{
		versioned(31922, "shared-d", 1785000556),
		versioned(31923, "shared-d", 1778576388),
	}

	total, indexed, superseded, errs := reindexStructuredEvents("calendar", seqOf(events...), func(nostr.Event) error { return nil })

	if total != 2 || indexed != 2 || superseded != 0 || errs != 0 {
		t.Fatalf("total=%d indexed=%d superseded=%d errs=%d, want 2/2/0/0", total, indexed, superseded, errs)
	}
}

// structuredDedupKey must agree with the id each projection actually writes,
// or the reindex would resolve versions against a key nothing is stored under.
func TestStructuredDedupKey_MatchesProjectedDocumentIDs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        nostr.Kind
		addressable bool
	}{
		{"longform", 30023, true},
		{"wiki", 30818, true},
		{"shares targeted publication", 30222, true},
		{"calendar date-based", 31922, true},
		{"publication index", 30040, true},
		{"transferkiosk", 30143, true},
		{"shares generic repost", 16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := nostr.Event{Kind: tc.kind, Tags: nostr.Tags{{"d", "the-d"}}}
			e.ID = e.GetID()
			key, addressable := structuredDedupKey(e)
			if addressable != tc.addressable {
				t.Fatalf("addressable = %v, want %v", addressable, tc.addressable)
			}
			if !addressable {
				return
			}
			if want := docIDFor(tc.kind, e.PubKey.Hex(), "the-d"); key != want {
				t.Errorf("dedup key = %q, want the projected document id %q", key, want)
			}
		})
	}
}

// The AMB pass resolves versions under tsDocIDFromEvent, but Typesense stores
// the document under the id NostrToAMB puts on it. If those two ever diverge,
// the reindex would resolve versions against a key nothing is stored under and
// the stale-version bug would come back silently. Pin them together.
func TestTSDocIDFromEvent_MatchesProjectedAMBDocumentID(t *testing.T) {
	for _, d := range []string{
		"https://www.rpi-ekkw-ekhn.de/rpi-konfi_2-2022.pdf",
		"a68eacf6",
		"", // no d-tag at all: tsDocIDFromEvent must refuse rather than guess
	} {
		evt := nostr.Event{
			Kind:   30142,
			PubKey: nostr.MustPubKeyFromHex("776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4"),
			Tags:   nostr.Tags{{"name", "x"}},
		}
		if d != "" {
			evt.Tags = append(evt.Tags, nostr.Tag{"d", d})
		}
		evt.ID = evt.GetID()

		docID, err := tsDocIDFromEvent(evt)
		if d == "" {
			if err == nil {
				t.Errorf("tsDocIDFromEvent accepted an event with no d-tag (docID %q)", docID)
			}
			continue
		}
		if err != nil {
			t.Fatalf("tsDocIDFromEvent(%q): %v", d, err)
		}

		amb, err := typesense30142.NostrToAMB(&evt)
		if err != nil {
			t.Fatalf("NostrToAMB(%q): %v", d, err)
		}
		if amb.ID != docID {
			t.Errorf("d=%q: projection stores id %q but reindex dedupes on %q", d, amb.ID, docID)
		}
	}
}

// An event carrying {"d",""} is the gap a review caught: tsDocIDFromEvent
// rejects an empty d value, but NostrToAMB still assigns the real, collidable
// document id `{pubkey}:`. Deduping on the content key would have skipped
// resolution for exactly these events and left them on the unconditional
// last-write-wins path this whole change exists to close.
func TestAMBDedupKey_MatchesProjectedAMBDocumentID(t *testing.T) {
	pk := nostr.MustPubKeyFromHex("776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4")

	for _, tc := range []struct {
		name        string
		tags        nostr.Tags
		addressable bool
	}{
		{"normal d", nostr.Tags{{"name", "x"}, {"d", "https://example.org/r"}}, true},
		{"empty d value", nostr.Tags{{"name", "x"}, {"d", ""}}, true},
		{"no d tag at all", nostr.Tags{{"name", "x"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evt := nostr.Event{Kind: 30142, PubKey: pk, Tags: tc.tags}
			evt.ID = evt.GetID()

			key, addressable := ambDedupKey(evt)
			if addressable != tc.addressable {
				t.Fatalf("addressable = %v, want %v", addressable, tc.addressable)
			}

			amb, err := typesense30142.NostrToAMB(&evt)
			if err != nil {
				t.Fatalf("NostrToAMB: %v", err)
			}
			if !tc.addressable {
				if amb.ID != "" {
					t.Errorf("projection assigns id %q but dedup treats the event as unaddressable", amb.ID)
				}
				return
			}
			if key != amb.ID {
				t.Errorf("dedup key %q != the id the projection stores the document under %q", key, amb.ID)
			}
		})
	}
}

// Two versions of a d="" resource must resolve against each other rather than
// both being projected onto the one document id they share.
func TestAMBDedupKey_EmptyDVersionsCollide(t *testing.T) {
	pk := nostr.MustPubKeyFromHex("776c7bfe528c041cd1114efb6d48100b2e49d4faf27e301fb3f83c64a28694f4")
	mk := func(createdAt nostr.Timestamp) nostr.Event {
		e := nostr.Event{Kind: 30142, PubKey: pk, CreatedAt: createdAt, Tags: nostr.Tags{{"d", ""}}}
		e.ID = e.GetID()
		return e
	}
	newer, older := mk(1785000556), mk(1778576388)

	kNew, ok1 := ambDedupKey(newer)
	kOld, ok2 := ambDedupKey(older)
	if !ok1 || !ok2 || kNew != kOld {
		t.Fatalf("empty-d events must share a dedup key: %q vs %q (ok %v/%v)", kNew, kOld, ok1, ok2)
	}

	d := typesense30142.NewAddressDedup()
	if !d.Keep(kNew, newer) {
		t.Fatal("the newer version must be kept")
	}
	if d.Keep(kOld, older) {
		t.Error("the older version must be superseded, not projected onto the same document")
	}
}

// amb-relay#11: content rows are keyed by EVENT id but patched onto a document
// keyed by ADDRESS, and PatchContent is an unconditional PATCH. Where an
// address has several stored versions each carrying a row, all of them land on
// the one shared document — so without ordering, the document could end up with
// the newest version's metadata and an OLDER version's fulltext, chosen by
// BoltDB key order (lexicographic by event id hex, unrelated to time).
func TestOrderContentReplay_NewestWinsTheLastWrite(t *testing.T) {
	// ids deliberately in an order where lexicographic != chronological
	rows := []liveContent{
		{id: "aaa_newest"},
		{id: "bbb_oldest"},
		{id: "ccc_middle"},
	}
	createdAt := map[string]nostr.Timestamp{
		"aaa_newest": 1785390556,
		"bbb_oldest": 1778576388,
		"ccc_middle": 1779543035,
	}

	orderContentReplay(rows, createdAt)

	got := []string{rows[0].id, rows[1].id, rows[2].id}
	want := []string{"bbb_oldest", "ccc_middle", "aaa_newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("replay order = %v, want %v (oldest first, so the newest is patched last)", got, want)
	}
	if rows[len(rows)-1].id != "aaa_newest" {
		t.Error("the LAST write must be the newest version — that is the one the document keeps")
	}
}

// A row whose event was not walked has no timestamp. It must not be able to
// overwrite a known-newer row, so it sorts first.
func TestOrderContentReplay_UnknownEventSortsFirst(t *testing.T) {
	rows := []liveContent{
		{id: "known_new"},
		{id: "unknown"},
	}
	orderContentReplay(rows, map[string]nostr.Timestamp{"known_new": 1785390556})

	if rows[0].id != "unknown" || rows[1].id != "known_new" {
		t.Errorf("order = [%s %s], want [unknown known_new]", rows[0].id, rows[1].id)
	}
}

// Equal timestamps must keep their input order rather than shuffling between
// runs — a reindex that reorders content non-deterministically is the bug.
func TestOrderContentReplay_StableOnTies(t *testing.T) {
	mk := func() []liveContent {
		return []liveContent{{id: "first"}, {id: "second"}, {id: "third"}}
	}
	ts := map[string]nostr.Timestamp{"first": 100, "second": 100, "third": 100}

	a, b := mk(), mk()
	orderContentReplay(a, ts)
	orderContentReplay(b, ts)

	for i := range a {
		if a[i].id != b[i].id || a[i].id != mk()[i].id {
			t.Fatalf("tie ordering not stable: %v", []string{a[0].id, a[1].id, a[2].id})
		}
	}
}

// The ordering only matters because of what it does to the PATCH sequence, and
// that sequence is not observable through run(). This drives the replay itself
// and asserts the order the patches land in — so removing the sort from
// replayContent fails here, which the orderContentReplay unit tests above
// cannot do.
func TestReplayContent_PatchesOldestFirstOntoTheSharedDocument(t *testing.T) {
	// Three versions of ONE address: same document id, rows keyed by event id.
	// ForEach walks BoltDB key order, so that is the input order here.
	const docID = "pubkey:the-d-tag"
	rows := []liveContent{
		{id: "aaa", entry: ContentEntry{Text: "newest text"}},
		{id: "bbb", entry: ContentEntry{Text: "oldest text"}},
		{id: "ccc", entry: ContentEntry{Text: "middle text"}},
	}
	docIDs := map[string]string{"aaa": docID, "bbb": docID, "ccc": docID}
	createdAt := map[string]nostr.Timestamp{
		"aaa": 1785390556,
		"bbb": 1778576388,
		"ccc": 1779543035,
	}

	var gotText []string
	patched, errs := replayContent(rows, docIDs, createdAt,
		func(gotDoc string, entry ContentEntry) error {
			if gotDoc != docID {
				t.Errorf("patched doc id = %q, want %q", gotDoc, docID)
			}
			gotText = append(gotText, entry.Text)
			return nil
		})

	want := []string{"oldest text", "middle text", "newest text"}
	if !slices.Equal(gotText, want) {
		t.Fatalf("patch order = %v, want %v", gotText, want)
	}
	// The surviving fulltext is whatever the LAST patch wrote.
	if gotText[len(gotText)-1] != "newest text" {
		t.Errorf("document keeps %q, want the newest version's text", gotText[len(gotText)-1])
	}
	if patched != 3 || errs != 0 {
		t.Errorf("patched=%d errs=%d, want 3/0", patched, errs)
	}
}

// A row with no document id is skipped and counted as an error, and must not
// abort the rows after it — a single d-tag-less event should not cost the rest
// of the corpus its fulltext.
func TestReplayContent_SkipsRowsWithNoDocIDAndContinues(t *testing.T) {
	rows := []liveContent{
		{id: "no_doc", entry: ContentEntry{Text: "orphaned"}},
		{id: "has_doc", entry: ContentEntry{Text: "kept"}},
	}
	createdAt := map[string]nostr.Timestamp{"no_doc": 100, "has_doc": 200}

	var gotText []string
	patched, errs := replayContent(rows, map[string]string{"has_doc": "doc-1"}, createdAt,
		func(_ string, entry ContentEntry) error {
			gotText = append(gotText, entry.Text)
			return nil
		})

	if !slices.Equal(gotText, []string{"kept"}) {
		t.Fatalf("patched %v, want only the row that has a doc id", gotText)
	}
	if patched != 1 || errs != 1 {
		t.Errorf("patched=%d errs=%d, want 1/1", patched, errs)
	}
}

// A failing PATCH is counted and the replay carries on to the next row.
func TestReplayContent_PatchErrorCountedAndContinues(t *testing.T) {
	rows := []liveContent{
		{id: "boom", entry: ContentEntry{Text: "fails"}},
		{id: "ok", entry: ContentEntry{Text: "succeeds"}},
	}
	createdAt := map[string]nostr.Timestamp{"boom": 100, "ok": 200}

	var seen []string
	patched, errs := replayContent(rows, map[string]string{"boom": "d1", "ok": "d2"}, createdAt,
		func(docID string, entry ContentEntry) error {
			seen = append(seen, entry.Text)
			if entry.Text == "fails" {
				return errors.New("typesense down")
			}
			return nil
		})

	if !slices.Equal(seen, []string{"fails", "succeeds"}) {
		t.Fatalf("attempted %v, want both rows attempted in order", seen)
	}
	if patched != 1 || errs != 1 {
		t.Errorf("patched=%d errs=%d, want 1/1", patched, errs)
	}
}
