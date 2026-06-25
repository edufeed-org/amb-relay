package main

import (
	"iter"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/khatru/semantic"
)

func calEvent(t *testing.T, start, end string) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"d", "e1"}, {"title", "T"}}
	if start != "" {
		tags = append(tags, nostr.Tag{"start", start})
	}
	if end != "" {
		tags = append(tags, nostr.Tag{"end", end})
	}
	e := nostr.Event{Kind: 31923, Tags: tags}
	e.ID = e.GetID()
	return e
}

func TestInCalendarWindow(t *testing.T) {
	// event: start=1718600000, end=1718603600
	e := calEvent(t, "1718600000", "1718603600")

	cases := []struct {
		name string
		cf   calendar.CalendarFilter
		want bool
	}{
		{"no bounds", calendar.CalendarFilter{}, true},
		{"start_after pass", calendar.CalendarFilter{StartAfter: 1718500000}, true},
		{"start_after fail", calendar.CalendarFilter{StartAfter: 1718700000}, false},
		{"start_before pass", calendar.CalendarFilter{StartBefore: 1718700000}, true},
		{"start_before fail", calendar.CalendarFilter{StartBefore: 1718500000}, false},
		{"end_after pass", calendar.CalendarFilter{EndAfter: 1718600000}, true},
		{"end_after fail", calendar.CalendarFilter{EndAfter: 1718700000}, false},
		{"end_before pass", calendar.CalendarFilter{EndBefore: 1718700000}, true},
		{"end_before fail", calendar.CalendarFilter{EndBefore: 1718600000}, false},
		{"inclusive lower", calendar.CalendarFilter{StartAfter: 1718600000}, true},
		{"inclusive upper", calendar.CalendarFilter{StartBefore: 1718600000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inCalendarWindow(e, c.cf); got != c.want {
				t.Errorf("inCalendarWindow = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInCalendarWindowMissingFieldFailsBound(t *testing.T) {
	// event with no end tag: any end bound must fail.
	e := calEvent(t, "1718600000", "")
	if inCalendarWindow(e, calendar.CalendarFilter{EndBefore: 1718700000}) {
		t.Error("missing end with EndBefore set: want false")
	}
	if inCalendarWindow(e, calendar.CalendarFilter{EndAfter: 1}) {
		t.Error("missing end with EndAfter set: want false")
	}
	// no start tag: any start bound must fail.
	e2 := calEvent(t, "", "1718603600")
	if inCalendarWindow(e2, calendar.CalendarFilter{StartAfter: 1}) {
		t.Error("missing start with StartAfter set: want false")
	}
}

func seqOf(events ...nostr.Event) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for _, e := range events {
			if !yield(e) {
				return
			}
		}
	}
}

func snippetFor(t *testing.T, parentID string) nostr.Event {
	t.Helper()
	e := nostr.Event{
		Kind: semantic.KindSearchSnippet,
		Tags: nostr.Tags{{"e", parentID}},
	}
	e.ID = e.GetID()
	return e
}

func TestWindowCalendarDropsOutOfWindow(t *testing.T) {
	in := calEvent(t, "1718600000", "1718603600")  // in window
	out := calEvent(t, "1710000000", "1710003600") // before window
	cf := calendar.CalendarFilter{StartAfter: 1718000000, StartBefore: 1719000000}

	got := collectEvents(windowCalendar(seqOf(in, out), cf, 0))
	if len(got) != 1 || got[0].ID != in.ID {
		t.Fatalf("got %d events, want 1 (the in-window event)", len(got))
	}
}

func TestWindowCalendarKeepsSnippetWithKeptParent(t *testing.T) {
	parent := calEvent(t, "1718600000", "1718603600")
	snip := snippetFor(t, parent.ID.Hex())
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(parent, snip), cf, 0))
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (parent + its snippet)", len(got))
	}
	if got[1].Kind != semantic.KindSearchSnippet {
		t.Errorf("second event kind = %d, want snippet", got[1].Kind)
	}
}

func TestWindowCalendarDropsSnippetWithDroppedParent(t *testing.T) {
	parent := calEvent(t, "1710000000", "1710003600") // out of window
	snip := snippetFor(t, parent.ID.Hex())
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(parent, snip), cf, 0))
	if len(got) != 0 {
		t.Fatalf("got %d, want 0 (parent dropped, so snippet dropped)", len(got))
	}
}

func TestWindowCalendarAppliesLimit(t *testing.T) {
	a := calEvent(t, "1718600000", "1718603600")
	b := calEvent(t, "1718600001", "1718603601")
	c := calEvent(t, "1718600002", "1718603602")
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(a, b, c), cf, 2))
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (limit applied to parents)", len(got))
	}
}

func TestWindowCalendarPassesNonCalendar(t *testing.T) {
	cal := calEvent(t, "1710000000", "1710003600") // out of window calendar
	other := nostr.Event{Kind: 30142, Tags: nostr.Tags{{"d", "x"}}}
	other.ID = other.GetID()
	cf := calendar.CalendarFilter{StartAfter: 1718000000}

	got := collectEvents(windowCalendar(seqOf(cal, other), cf, 0))
	if len(got) != 1 || got[0].Kind != 30142 {
		t.Fatalf("want only the non-calendar 30142 event through, got %d", len(got))
	}
}
