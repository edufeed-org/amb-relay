package main

import (
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/semantic"
	"fiatjaf.com/nostr/nip52"
)

// windowCalendar post-filters a reranked sequence by the calendar window.
// ChunkRerankQuery interleaves a kind-21142 snippet AFTER each parent it
// keeps; we keep a snippet iff its parent passed the window. Non-calendar
// kinds pass through untouched. The limit counts parents only and is applied
// after windowing (ChunkRerankQuery was called with Limit=0 to hand us the
// full reranked pool).
func windowCalendar(seq iter.Seq[nostr.Event], cf calendar.CalendarFilter, limit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		n := 0
		keptParent := false
		done := false
		for e := range seq {
			if e.Kind == semantic.KindSearchSnippet {
				if keptParent {
					if !yield(e) {
						return
					}
				}
				if done {
					return
				}
				continue
			}
			if done {
				return
			}
			keep := !calendar.IsCalendarEventKind(e.Kind) || inCalendarWindow(e, cf)
			keptParent = keep
			if !keep {
				continue
			}
			if !yield(e) {
				return
			}
			n++
			if limit > 0 && n >= limit {
				done = true
			}
		}
	}
}

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
