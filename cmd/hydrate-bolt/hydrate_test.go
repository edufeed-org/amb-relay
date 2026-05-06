package main

import (
	"errors"
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
