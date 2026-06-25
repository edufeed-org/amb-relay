package main

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru/calendar"
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
