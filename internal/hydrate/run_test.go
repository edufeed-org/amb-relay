package hydrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// fakeTS serves a sequence of canned /collections/.../documents/search
// pages. perPage and pages drive the response shape so tests can simulate
// both empty corpora and multi-page walks. Tests inject `docs[page]` for
// each 1-indexed page; an out-of-range page returns an empty hits array
// (the natural terminator).
type fakeTS struct {
	perPage int
	docs    map[int][]TSDocRef
	hits    int // total HTTP hits — verifies the walker re-requests on verify pass
}

func newFakeTS(perPage int, docs map[int][]TSDocRef) *fakeTS {
	return &fakeTS{perPage: perPage, docs: docs}
}

func (f *fakeTS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		docs := f.docs[page]
		hits := make([]map[string]any, 0, len(docs))
		for _, d := range docs {
			hits = append(hits, map[string]any{
				"document": map[string]string{
					"eventRaw": d.EventRaw,
					"eventID":  d.EventID,
				},
			})
		}
		body := map[string]any{"hits": hits}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

func TestRun_EmptyTypesense(t *testing.T) {
	fake := newFakeTS(250, map[int][]TSDocRef{})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sink := &stubSink{}
	store := &stubStore{}

	stats, verify, err := Run(context.Background(), Config{
		TSHost:       srv.URL,
		TSAPIKey:     "x",
		TSCollection: "amb",
		PageSize:     250,
	}, sink, store, silentLogger{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Scanned != 0 || verify.Scanned != 0 {
		t.Errorf("expected zero scans, got stats=%+v verify=%+v", stats, verify)
	}
	if len(sink.saved) != 0 {
		t.Errorf("nothing should be saved, got %d", len(sink.saved))
	}
}

// TestRun_MismatchResavedViaVerify is the headline regression test for the
// startup hook. The TS doc reports eventID = X but its eventRaw decodes to
// a different id. The canonical save pass saves the decoded event under
// its real id; the verify pass then notices BoltDB has nothing keyed at X
// and resaves the eventRaw with .ID overridden to X.
func TestRun_MismatchResavedViaVerify(t *testing.T) {
	canonicalID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	staleTSEventID := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	fake := newFakeTS(250, map[int][]TSDocRef{
		1: {{EventID: staleTSEventID, EventRaw: rawEventJSON(canonicalID)}},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// presentIDs starts empty; canonical save pass populates it via SaveEvent.
	// To do that, we route SaveEvent into presentIDs so the verify pass sees
	// the canonical row but NOT the stale TS.eventID row.
	store := &recordingStore{
		stubStore: stubStore{presentIDs: map[string]nostr.Event{}},
	}
	store.recordSaves = true

	_, verify, err := Run(context.Background(), Config{
		TSHost:       srv.URL,
		TSAPIKey:     "x",
		TSCollection: "amb",
		PageSize:     250,
	}, store, store, silentLogger{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if verify.Mismatches != 1 || verify.Resaved != 1 {
		t.Errorf("expected mismatches=1 resaved=1, got %+v", verify)
	}
	// Two saves should have occurred: canonical (decoded id) + resave (TS.eventID).
	if len(store.saved) != 2 {
		t.Fatalf("expected 2 saves, got %d (%v)", len(store.saved), store.saved)
	}
	gotIDs := map[string]bool{
		store.saved[0].ID.Hex(): true,
		store.saved[1].ID.Hex(): true,
	}
	if !gotIDs[canonicalID] || !gotIDs[staleTSEventID] {
		t.Errorf("expected saved IDs to include both %s and %s; got %v",
			canonicalID, staleTSEventID, gotIDs)
	}
}

// recordingStore extends stubStore so the canonical SaveEvent also seeds
// presentIDs — making the verify-pass QueryEvents see what the hydrate-pass
// stored. Without this, verify would think nothing exists and resave both.
type recordingStore struct {
	stubStore
	recordSaves bool
}

func (r *recordingStore) SaveEvent(evt nostr.Event) error {
	if err := r.stubStore.SaveEvent(evt); err != nil {
		return err
	}
	if r.recordSaves {
		r.stubStore.presentIDs[evt.ID.Hex()] = evt
	}
	return nil
}

// silentLogger discards Printf calls so tests don't spam stdout.
type silentLogger struct{}

func (silentLogger) Printf(_ string, _ ...any) {}

// helper: assert two-page walks emit GET requests for page=1 and page=2
// (regression against a regression we already saw — be explicit about it).
func TestRun_PageBoundaryStopsAtShortPage(t *testing.T) {
	// Page 1 returns 2 docs (less than perPage=3) — walker should stop here
	// without requesting page 2.
	fake := newFakeTS(3, map[int][]TSDocRef{
		1: {
			{EventID: hex64('a'), EventRaw: rawEventJSON(hex64('a'))},
			{EventID: hex64('b'), EventRaw: rawEventJSON(hex64('b'))},
		},
	})
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sink := &stubSink{}
	store := &recordingStore{
		stubStore: stubStore{presentIDs: map[string]nostr.Event{}},
	}
	store.recordSaves = true

	stats, _, err := Run(context.Background(), Config{
		TSHost:       srv.URL,
		TSAPIKey:     "x",
		TSCollection: "amb",
		PageSize:     3,
	}, sink, store, silentLogger{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Scanned != 2 {
		t.Errorf("expected 2 scanned, got %d", stats.Scanned)
	}
	// Walker hits once per pass: page=1 for hydrate, page=1 for verify = 2.
	if fake.hits != 2 {
		t.Errorf("expected 2 TS hits (one per pass), got %d", fake.hits)
	}
}

// hex64 returns a 64-char hex string of the given byte repeated, for use as
// a fake event id. Avoids needing fresh literal IDs in every test case.
func hex64(c byte) string {
	return strings.Repeat(fmt.Sprintf("%x", c), 64)
}
