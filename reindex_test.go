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
	total, indexed, errs := reindexStructuredEvents("longform", seq, reproject)
	if total != 3 || indexed != 2 || errs != 1 {
		t.Fatalf("total=%d indexed=%d errs=%d, want 3/2/1", total, indexed, errs)
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
