package main

import (
	"context"
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

// calendarRerankQuery wraps semantic.ChunkRerankQuery so a calendar topic+time
// REQ is semantically reranked AND time-windowed. The shared rerank path
// post-filters with filter.Matches, which rejects calendar events whenever the
// synthetic range params (start_after/…) are present (they are query params,
// not real event tags). So for a windowed calendar search we strip those four
// keys before rerank, take the full reranked pool (Limit=0), and post-window
// relay-side. Searches without a window — or non-calendar searches, or
// geohash (Bolt-only) searches — are left to the plain paths.
func calendarRerankQuery(ctx context.Context, filter nostr.Filter, searcher semantic.ChunkSearcher, fetch fetchFunc, maxLimit int, sk nostr.SecretKey) iter.Seq[nostr.Event] {
	if !calendar.HasCalendarKinds(filter) {
		return semantic.ChunkRerankQuery(ctx, filter, searcher, fetch, maxLimit, sk)
	}
	cf := calendar.ExtractCalendarFilter(filter)
	if len(cf.Geohashes) > 0 {
		// geohash-prefix is Bolt-only; rerank can't serve it.
		return fetch(filter, maxLimit)
	}
	if cf.StartAfter == 0 && cf.StartBefore == 0 && cf.EndAfter == 0 && cf.EndBefore == 0 {
		return semantic.ChunkRerankQuery(ctx, filter, searcher, fetch, maxLimit, sk)
	}
	limit := filter.Limit
	if limit <= 0 || limit > maxLimit {
		limit = maxLimit
	}
	stripped := calendar.RemoveCalendarTags(filter)
	stripped.Limit = 0 // take the full reranked pool; windowCalendar re-caps.
	ranked := semantic.ChunkRerankQuery(ctx, stripped, searcher, fetch, maxLimit, sk)
	return windowCalendar(ranked, cf, limit)
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
