package main

import (
	"encoding/json"
	"errors"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
)

// EventSink is the subset of *boltdb.BoltBackend the hydrate logic uses.
// Declared as an interface so unit tests don't need a real bolt file.
type EventSink interface {
	SaveEvent(evt nostr.Event) error
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
