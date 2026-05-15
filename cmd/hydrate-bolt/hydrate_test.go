package main

import (
	"errors"
	"iter"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
)

// stubSink records SaveEvent calls and lets tests inject specific errors per
// event ID. ID-keyed errors model "this id is already present" (ErrDupEvent)
// or "this id triggers an unexpected store error" without stateful bookkeeping.
type stubSink struct {
	mu      sync.Mutex
	saved   []nostr.Event
	errByID map[string]error
	saveErr error // returned for any id not in errByID
}

func (s *stubSink) SaveEvent(evt nostr.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.errByID[evt.ID.Hex()]; ok {
		return err
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, evt)
	return nil
}

// A minimal valid event JSON, parameterized by id and kind so each test
// case gets a distinct identity. Pubkey / sig / created_at are set to fixed
// dummy values — Hydrate does not verify, it just stores.
func rawEventJSON(idHex string) string {
	return `{` +
		`"id":"` + idHex + `",` +
		`"kind":30142,` +
		`"pubkey":"a01bd200e4cf622a60e34eb50cb16a4aa1bd82509d64dcb9c71e9d7221ce5b4b",` +
		`"created_at":1769704564,` +
		`"tags":[["d","test/` + idHex + `"]],` +
		`"content":"x",` +
		`"sig":"deadbeef"` +
		`}`
}

func TestHydrateOne_Saved(t *testing.T) {
	sink := &stubSink{}
	idHex := "1111111111111111111111111111111111111111111111111111111111111111"

	out := HydrateOne(rawEventJSON(idHex), sink)

	if out != OutcomeSaved {
		t.Errorf("outcome = %v, want OutcomeSaved", out)
	}
	if len(sink.saved) != 1 || sink.saved[0].ID.Hex() != idHex {
		t.Errorf("expected exactly one saved event with id %s, got %+v", idHex, sink.saved)
	}
}

func TestHydrateOne_AlreadyPresent(t *testing.T) {
	idHex := "2222222222222222222222222222222222222222222222222222222222222222"
	sink := &stubSink{errByID: map[string]error{idHex: eventstore.ErrDupEvent}}

	out := HydrateOne(rawEventJSON(idHex), sink)

	if out != OutcomeAlreadyPresent {
		t.Errorf("outcome = %v, want OutcomeAlreadyPresent", out)
	}
	if len(sink.saved) != 0 {
		t.Errorf("expected zero saved events on dup, got %+v", sink.saved)
	}
}

func TestHydrateOne_ParseError(t *testing.T) {
	sink := &stubSink{}

	out := HydrateOne(`{not valid json`, sink)

	if out != OutcomeParseError {
		t.Errorf("outcome = %v, want OutcomeParseError", out)
	}
	if len(sink.saved) != 0 {
		t.Errorf("malformed input must not be saved, got %+v", sink.saved)
	}
}

func TestHydrateOne_SaveError(t *testing.T) {
	idHex := "3333333333333333333333333333333333333333333333333333333333333333"
	sink := &stubSink{errByID: map[string]error{idHex: errors.New("disk full")}}

	out := HydrateOne(rawEventJSON(idHex), sink)

	if out != OutcomeSaveError {
		t.Errorf("outcome = %v, want OutcomeSaveError", out)
	}
}

// stubStore extends stubSink with QueryEvents so it satisfies the EventStore
// interface needed by VerifyOne. The presentIDs set models which event IDs
// the BoltDB-equivalent backing store would yield on a lookup.
type stubStore struct {
	stubSink
	presentIDs map[string]nostr.Event
}

func (s *stubStore) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for _, id := range filter.IDs {
			if evt, ok := s.presentIDs[id.Hex()]; ok {
				if !yield(evt) {
					return
				}
			}
		}
	}
}

func TestVerifyOne_OK(t *testing.T) {
	idHex := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id, err := nostr.IDFromHex(idHex)
	if err != nil {
		t.Fatalf("IDFromHex: %v", err)
	}
	store := &stubStore{
		presentIDs: map[string]nostr.Event{idHex: {ID: id}},
	}

	outcome, gotID, err := VerifyOne(idHex, rawEventJSON(idHex), store)
	if err != nil {
		t.Fatalf("VerifyOne err = %v", err)
	}
	if outcome != VerifyOK {
		t.Errorf("outcome = %v, want VerifyOK", outcome)
	}
	if gotID.Hex() != idHex {
		t.Errorf("returned id = %s, want %s", gotID.Hex(), idHex)
	}
	if len(store.saved) != 0 {
		t.Errorf("VerifyOK must not save anything, got %d", len(store.saved))
	}
}

// TestVerifyOne_MismatchResaves is the headline regression test. The
// Typesense doc reports eventID = X, but the eventRaw payload decodes to
// an event with id = Y. BoltDB has Y (from the canonical hydrate pass)
// but nothing keyed at X — the exact silent-drop skew. VerifyOne must
// detect this and save a row whose .ID has been overridden to X so the
// relay's lookup-by-X succeeds.
func TestVerifyOne_MismatchResaves(t *testing.T) {
	canonicalID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	staleTSEventID := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	canonical, err := nostr.IDFromHex(canonicalID)
	if err != nil {
		t.Fatalf("IDFromHex: %v", err)
	}

	// BoltDB has the canonical row only. The stale TS.eventID points
	// at a hash BoltDB doesn't know about.
	store := &stubStore{
		presentIDs: map[string]nostr.Event{canonicalID: {ID: canonical}},
	}

	outcome, gotID, err := VerifyOne(staleTSEventID, rawEventJSON(canonicalID), store)
	if err != nil {
		t.Fatalf("VerifyOne err = %v", err)
	}
	if outcome != VerifyResaved {
		t.Errorf("outcome = %v, want VerifyResaved", outcome)
	}
	if gotID.Hex() != staleTSEventID {
		t.Errorf("returned id = %s, want %s", gotID.Hex(), staleTSEventID)
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected one resave, got %d", len(store.saved))
	}
	// The saved event must carry the overridden TS-side ID so future
	// BoltDB lookups by that ID find it.
	if store.saved[0].ID.Hex() != staleTSEventID {
		t.Errorf("resaved event ID = %s, want override to %s",
			store.saved[0].ID.Hex(), staleTSEventID)
	}
}

func TestVerifyOne_ParseErrorOnBadHex(t *testing.T) {
	store := &stubStore{}
	outcome, _, err := VerifyOne("not-hex", rawEventJSON("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"), store)
	if outcome != VerifyParseError {
		t.Errorf("outcome = %v, want VerifyParseError", outcome)
	}
	if err == nil {
		t.Errorf("expected an error for non-hex TS.eventID, got nil")
	}
}

func TestVerifyOne_ParseErrorOnBadEventRaw(t *testing.T) {
	tsEventID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	store := &stubStore{} // nothing present → triggers the parse step
	outcome, _, err := VerifyOne(tsEventID, `{not valid json`, store)
	if outcome != VerifyParseError {
		t.Errorf("outcome = %v, want VerifyParseError", outcome)
	}
	if err == nil {
		t.Errorf("expected an error for invalid eventRaw, got nil")
	}
}

func TestVerifyOne_SaveError(t *testing.T) {
	tsEventID := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	store := &stubStore{
		// Stub Sink saveErr drives the override SaveEvent to fail.
		stubSink: stubSink{saveErr: errors.New("disk full")},
	}
	outcome, gotID, err := VerifyOne(tsEventID, rawEventJSON(tsEventID), store)
	if outcome != VerifySaveError {
		t.Errorf("outcome = %v, want VerifySaveError", outcome)
	}
	if gotID.Hex() != tsEventID {
		t.Errorf("returned id = %s, want %s", gotID.Hex(), tsEventID)
	}
	if err == nil {
		t.Errorf("expected save error to bubble up")
	}
}

func TestVerifyOne_AlreadyResave(t *testing.T) {
	tsEventID := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	store := &stubStore{
		stubSink: stubSink{errByID: map[string]error{tsEventID: eventstore.ErrDupEvent}},
	}
	outcome, gotID, err := VerifyOne(tsEventID, rawEventJSON(tsEventID), store)
	if err != nil {
		t.Fatalf("VerifyOne err = %v", err)
	}
	if outcome != VerifyAlreadyResave {
		t.Errorf("outcome = %v, want VerifyAlreadyResave", outcome)
	}
	if gotID.Hex() != tsEventID {
		t.Errorf("returned id = %s, want %s", gotID.Hex(), tsEventID)
	}
}

func TestHydrateBatch_Aggregates(t *testing.T) {
	dupID := "4444444444444444444444444444444444444444444444444444444444444444"
	okID := "5555555555555555555555555555555555555555555555555555555555555555"
	sink := &stubSink{errByID: map[string]error{dupID: eventstore.ErrDupEvent}}

	stats := HydrateBatch([]string{
		rawEventJSON(okID),
		rawEventJSON(dupID),
		`{not json`,
	}, sink)

	if stats.Saved != 1 {
		t.Errorf("Saved = %d, want 1", stats.Saved)
	}
	if stats.AlreadyPresent != 1 {
		t.Errorf("AlreadyPresent = %d, want 1", stats.AlreadyPresent)
	}
	if stats.ParseErrors != 1 {
		t.Errorf("ParseErrors = %d, want 1", stats.ParseErrors)
	}
	if stats.SaveErrors != 0 {
		t.Errorf("SaveErrors = %d, want 0", stats.SaveErrors)
	}
	if stats.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", stats.Scanned)
	}
}
