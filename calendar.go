package main

import (
	"fmt"
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/nip52"
)

// CalendarDocument is the Typesense document shape for NIP-52 calendar events
// (kinds 31922/31923/31924/31925) stored in the calendar_31922 collection. It
// embeds the shared structuredEnvelope so the raw-event fields are promoted to
// top-level JSON keys, identical to the long-form/wiki layout. start/end are
// Unix seconds (date-based "YYYY-MM-DD" starts are parsed to start-of-day UTC);
// geohash/status/location are present only on the kinds that carry them.
type CalendarDocument struct {
	ID       string `json:"id"`
	D        string `json:"d"`
	Title    string `json:"title,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Content  string `json:"content,omitempty"`
	Location string `json:"location,omitempty"`
	Start    int64  `json:"start,omitempty"`
	End      int64  `json:"end,omitempty"`
	Geohash  string `json:"geohash,omitempty"`
	Status   string `json:"status,omitempty"` // RSVP (31925): accepted/declined/tentative
	structuredEnvelope
}

// nostrToCalendar projects a NIP-52 event into a CalendarDocument. For the
// date/time event kinds (31922/31923) it uses nip52.ParseCalendarEvent to fill
// title/start/end/location/geohash; for 31924/31925 those are zero and
// title/summary/status come straight from the tags.
func nostrToCalendar(event *nostr.Event) (*CalendarDocument, error) {
	dTag := event.Tags.GetD()
	if dTag == "" {
		return nil, fmt.Errorf("calendar event %s missing required 'd' tag", event.ID.Hex())
	}
	env, err := newStructuredEnvelope(event)
	if err != nil {
		return nil, err
	}
	doc := &CalendarDocument{
		ID:                 typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag),
		D:                  dTag,
		Content:            event.Content,
		structuredEnvelope: env,
	}

	// Only 31922/31923 carry start/end/location/geohash; 31924 (calendar) and
	// 31925 (RSVP) have no time/location data, so they take the tag-only path.
	if calendar.IsCalendarEventKind(event.Kind) {
		cal := nip52.ParseCalendarEvent(*event)
		doc.Title = cal.Title
		if !cal.Start.IsZero() {
			doc.Start = cal.Start.Unix()
		}
		if !cal.End.IsZero() {
			doc.End = cal.End.Unix()
		}
		if len(cal.Locations) > 0 {
			doc.Location = cal.Locations[0]
		}
		if len(cal.Geohashes) > 0 {
			doc.Geohash = cal.Geohashes[0]
		}
	}

	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "title":
			if doc.Title == "" { // event kinds already set it from the parse
				doc.Title = tag[1]
			}
		case "summary":
			doc.Summary = tag[1]
		case "status":
			doc.Status = tag[1]
		}
	}
	return doc, nil
}

// storeCalendar projects and upserts a NIP-52 event to the calendar Typesense
// collection via the shared structured-collection helper.
func storeCalendar(enabled bool, ts *typesense30142.TSBackend, event nostr.Event) {
	storeStructured(enabled, ts, event, "calendar", nostrToCalendar)
}

// calendarFetch routes a calendar query: range/geo params over the event kinds
// (31922/31923) go to the Bolt index (boltFetch); everything else — full-text
// search, plain kind listing, 31924/31925 — goes to Typesense (tsFetch). When
// both range params and a search term are present, the Bolt path wins and the
// search term is ignored for that REQ (documented precedence; combined intent
// is composed client-side by the future MCP server).
func calendarFetch(boltFetch, tsFetch fetchFunc) fetchFunc {
	return func(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
		cf := calendar.ExtractCalendarFilter(filter)
		if cf.HasCalendarParams() && calendar.HasCalendarKinds(filter) {
			return boltFetch(filter, maxLimit)
		}
		return tsFetch(filter, maxLimit)
	}
}

// calendarSchema returns the Typesense collection schema for NIP-52 calendar
// events. Mirrors longformSchema's envelope naming so the shared query path
// reconstructs events identically; start/end are int64 facets for range
// filtering, geohash/status string facets.
func calendarSchema(name string) typesense30142.CollectionSchema {
	return typesense30142.CollectionSchema{
		Name:                name,
		DefaultSortingField: "eventCreatedAt",
		Fields: append([]typesense30142.Field{
			{Name: "id", Type: "string"},
			{Name: "d", Type: "string"},
			{Name: "title", Type: "string", Optional: true},
			{Name: "summary", Type: "string", Optional: true},
			{Name: "content", Type: "string", Optional: true},
			{Name: "location", Type: "string", Optional: true},
			{Name: "start", Type: "int64", Optional: true, Facet: true},
			{Name: "end", Type: "int64", Optional: true, Facet: true},
			{Name: "geohash", Type: "string", Optional: true, Facet: true},
			{Name: "status", Type: "string", Optional: true, Facet: true},
		}, structuredEnvelopeFields()...),
	}
}
