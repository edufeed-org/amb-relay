package main

import (
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/nip52"
)

// inCalendarWindow reports whether a calendar event satisfies every range
// bound present in the REQ, inclusively. A missing gated field fails its
// bound — matching the Typesense numeric path (a doc missing `end` fails an
// `end:<=` clause), and deliberately NOT khatru/calendar's unexported
// matchesCalendarFilter (which treats a missing upper-bounded field as
// passing).
func inCalendarWindow(e nostr.Event, cf calendar.CalendarFilter) bool {
	cal := nip52.ParseCalendarEvent(e)
	hasStart := !cal.Start.IsZero()
	hasEnd := !cal.End.IsZero()
	var start, end int64
	if hasStart {
		start = cal.Start.Unix()
	}
	if hasEnd {
		end = cal.End.Unix()
	}
	if cf.StartAfter > 0 && (!hasStart || start < cf.StartAfter) {
		return false
	}
	if cf.StartBefore > 0 && (!hasStart || start > cf.StartBefore) {
		return false
	}
	if cf.EndAfter > 0 && (!hasEnd || end < cf.EndAfter) {
		return false
	}
	if cf.EndBefore > 0 && (!hasEnd || end > cf.EndBefore) {
		return false
	}
	return true
}
