package main

import (
	"encoding/json"
	"errors"
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
)

// EventSink is the subset of *boltdb.BoltBackend the hydrate logic uses.
// Declared as an interface so unit tests don't need a real bolt file.
type EventSink interface {
	SaveEvent(evt nostr.Event) error
}

// EventStore is the subset of *boltdb.BoltBackend the verify pass uses.
// QueryEvents lets the verify pass check whether BoltDB has an entry keyed
// by the Typesense-side eventID (which can differ from the hash of the
// stored eventRaw payload when a TS doc's eventID field drifted from its
// eventRaw — the underlying cause of the silent-drop pagination bug this
// tool reconciles).
type EventStore interface {
	EventSink
	QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event]
}

// TSDocRef is a (eventID, eventRaw) pair for one Typesense document. The
// verify pass needs both: eventID is what the relay's BoltDB lookup uses,
// while eventRaw is the source of truth for the event contents the
// canonical save pass used.
type TSDocRef struct {
	EventID  string
	EventRaw string
}

// Outcome of a single HydrateOne call. The aggregate counts in Stats are
// derived from these.
type Outcome int

const (
	OutcomeSaved Outcome = iota
	OutcomeAlreadyPresent
	OutcomeParseError
	OutcomeSaveError
)

// Stats summarizes a HydrateBatch run.
type Stats struct {
	Scanned        int
	Saved          int
	AlreadyPresent int
	ParseErrors    int
	SaveErrors     int
}

// HydrateOne parses a single raw event JSON and writes it to the sink. The
// JSON shape comes from typesense30142's eventRaw field (a full nostr event
// payload). HydrateOne does NOT verify signatures — these events were
// already accepted at submission time and are only being re-stored to
// recover from a BoltDB-vs-Typesense skew. Re-verifying here would just
// reject events whose signing key has rotated since.
func HydrateOne(rawJSON string, sink EventSink) Outcome {
	var evt nostr.Event
	if err := json.Unmarshal([]byte(rawJSON), &evt); err != nil {
		return OutcomeParseError
	}
	if err := sink.SaveEvent(evt); err != nil {
		if errors.Is(err, eventstore.ErrDupEvent) {
			return OutcomeAlreadyPresent
		}
		return OutcomeSaveError
	}
	return OutcomeSaved
}

// HydrateBatch runs HydrateOne over a slice of raw event JSON strings and
// returns aggregate counts. Errors do not abort the run — the caller wants
// to see how much of a batch succeeded.
func HydrateBatch(rawEvents []string, sink EventSink) Stats {
	var s Stats
	for _, raw := range rawEvents {
		s.Scanned++
		switch HydrateOne(raw, sink) {
		case OutcomeSaved:
			s.Saved++
		case OutcomeAlreadyPresent:
			s.AlreadyPresent++
		case OutcomeParseError:
			s.ParseErrors++
		case OutcomeSaveError:
			s.SaveErrors++
		}
	}
	return s
}

// VerifyStats summarizes a verify pass run.
type VerifyStats struct {
	Scanned     int // TS docs inspected
	OK          int // BoltDB had an entry keyed at TS.eventID
	Mismatches  int // TS.eventID was missing from BoltDB
	Resaved     int // events successfully re-saved under TS.eventID (subset of Mismatches)
	ParseErrors int // eventRaw could not be decoded
	SaveErrors  int // SaveEvent for the re-saved-under-TS.eventID event failed
}

// VerifyOutcome enumerates what happened to one TS doc during verify.
type VerifyOutcome int

const (
	VerifyOK            VerifyOutcome = iota // TS.eventID was present in store
	VerifyResaved                            // TS.eventID was missing; re-saved under it
	VerifyAlreadyResave                      // TS.eventID was missing; re-save hit ErrDupEvent (raced)
	VerifyParseError                         // eventRaw could not be decoded
	VerifySaveError                          // re-save under TS.eventID failed
)

// VerifyOne checks whether the store has an entry keyed at tsEventID. If not,
// it parses eventRaw, overrides the event's ID with tsEventID, and saves the
// override into the store so future BoltDB lookups by tsEventID succeed.
//
// Why we override the ID: BoltDB keys events by evt.ID[16:24] (see
// nostrlib/eventstore/boltdb/save.go). The relay's query.go looks up by the
// Typesense-side eventID. If that field has drifted from sha256(eventRaw),
// no canonical save will ever populate the right key. Re-saving with the
// override puts a row at tsEventID[16:24] whose contents otherwise match
// the eventRaw — a deliberately non-canonical row, scoped to operational
// reconciliation. The relay-side fallback (typesense30142/query.go) handles
// correctness; this re-save is belt-and-suspenders so the BoltDB miss
// disappears entirely.
//
// Returns the outcome and the event ID we attempted to write (for logging).
func VerifyOne(tsEventID, eventRaw string, store EventStore) (VerifyOutcome, nostr.ID, error) {
	idBytes, err := nostr.IDFromHex(tsEventID)
	if err != nil {
		return VerifyParseError, nostr.ID{}, err
	}

	// Probe the store for an event matching the TS-side eventID.
	for range store.QueryEvents(nostr.Filter{IDs: []nostr.ID{idBytes}}, 1) {
		return VerifyOK, idBytes, nil
	}

	// Miss: parse the eventRaw, override its ID, and save.
	var evt nostr.Event
	if err := json.Unmarshal([]byte(eventRaw), &evt); err != nil {
		return VerifyParseError, idBytes, err
	}
	evt.ID = idBytes
	if err := store.SaveEvent(evt); err != nil {
		if errors.Is(err, eventstore.ErrDupEvent) {
			return VerifyAlreadyResave, idBytes, nil
		}
		return VerifySaveError, idBytes, err
	}
	return VerifyResaved, idBytes, nil
}
