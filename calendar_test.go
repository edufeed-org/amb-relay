package main

import (
	"testing"

	"fiatjaf.com/nostr"
)

// 31923 time-based: start/end are Unix seconds; ParseCalendarEvent fills them.
func TestNostrToCalendarTimeBased(t *testing.T) {
	evt := nostr.Event{
		Kind:      31923,
		PubKey:    nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"),
		CreatedAt: 1700000700,
		Tags: nostr.Tags{
			{"d", "workshop-1"},
			{"title", "Bruchrechnen Workshop"},
			{"summary", "Ein Workshop."},
			{"start", "1718600000"},
			{"end", "1718603600"},
			{"location", "Berlin"},
			{"g", "u33dc0"},
		},
		Content: "Wir lernen Bruchrechnen.",
	}
	evt.ID = evt.GetID()

	doc, err := nostrToCalendar(&evt)
	if err != nil {
		t.Fatalf("nostrToCalendar: %v", err)
	}
	if doc.ID != evt.PubKey.Hex()+":workshop-1" {
		t.Errorf("ID = %q", doc.ID)
	}
	if doc.D != "workshop-1" {
		t.Errorf("D = %q", doc.D)
	}
	if doc.Title != "Bruchrechnen Workshop" {
		t.Errorf("Title = %q", doc.Title)
	}
	if doc.Summary != "Ein Workshop." {
		t.Errorf("Summary = %q", doc.Summary)
	}
	if doc.Content != evt.Content {
		t.Errorf("Content = %q", doc.Content)
	}
	if doc.Start != 1718600000 {
		t.Errorf("Start = %d, want 1718600000", doc.Start)
	}
	if doc.End != 1718603600 {
		t.Errorf("End = %d, want 1718603600", doc.End)
	}
	if doc.Location != "Berlin" {
		t.Errorf("Location = %q", doc.Location)
	}
	if doc.Geohash != "u33dc0" {
		t.Errorf("Geohash = %q", doc.Geohash)
	}
	if doc.EventKind != 31923 {
		t.Errorf("EventKind = %d", doc.EventKind)
	}
	if doc.EventID != evt.ID.Hex() {
		t.Errorf("EventID = %q", doc.EventID)
	}
}

// 31922 date-based: start "YYYY-MM-DD" parses to the Unix start-of-day.
func TestNostrToCalendarDateBased(t *testing.T) {
	evt := nostr.Event{
		Kind:   31922,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"),
		Tags: nostr.Tags{
			{"d", "conf-2026"},
			{"title", "Conference"},
			{"start", "2026-06-17"},
		},
	}
	evt.ID = evt.GetID()
	doc, err := nostrToCalendar(&evt)
	if err != nil {
		t.Fatalf("nostrToCalendar: %v", err)
	}
	// 2026-06-17T00:00:00Z = 1781654400 (time.Parse with layout "2006-01-02" returns UTC)
	if doc.Start != 1781654400 {
		t.Errorf("Start = %d, want 1781654400 (2026-06-17 UTC)", doc.Start)
	}
}

// 31924 calendar: title from tag, no start/end.
func TestNostrToCalendarKind31924(t *testing.T) {
	evt := nostr.Event{
		Kind:   31924,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"),
		Tags:   nostr.Tags{{"d", "my-cal"}, {"title", "My Calendar"}},
	}
	evt.ID = evt.GetID()
	doc, err := nostrToCalendar(&evt)
	if err != nil {
		t.Fatalf("nostrToCalendar: %v", err)
	}
	if doc.Title != "My Calendar" {
		t.Errorf("Title = %q", doc.Title)
	}
	if doc.Start != 0 || doc.End != 0 {
		t.Errorf("Start/End = %d/%d, want 0/0 for kind 31924", doc.Start, doc.End)
	}
}

// 31925 RSVP: status carried.
func TestNostrToCalendarRSVP(t *testing.T) {
	evt := nostr.Event{
		Kind:   31925,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"),
		Tags: nostr.Tags{
			{"d", "rsvp-1"},
			{"a", "31923:0000000000000000000000000000000000000000000000000000000000000004:workshop-1"},
			{"status", "accepted"},
		},
	}
	evt.ID = evt.GetID()
	doc, err := nostrToCalendar(&evt)
	if err != nil {
		t.Fatalf("nostrToCalendar: %v", err)
	}
	if doc.Status != "accepted" {
		t.Errorf("Status = %q, want accepted", doc.Status)
	}
}

func TestNostrToCalendarMissingDTag(t *testing.T) {
	evt := nostr.Event{Kind: 31923, Tags: nostr.Tags{{"title", "x"}, {"start", "1"}}}
	if _, err := nostrToCalendar(&evt); err == nil {
		t.Fatal("expected error for missing d tag")
	}
}

func TestCalendarSchemaFields(t *testing.T) {
	s := calendarSchema("calendar_31922")
	if s.Name != "calendar_31922" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.DefaultSortingField != "eventCreatedAt" {
		t.Errorf("DefaultSortingField = %q", s.DefaultSortingField)
	}
	want := map[string]bool{
		"id": true, "d": true, "title": true, "summary": true, "content": true,
		"location": true, "start": true, "end": true, "geohash": true, "status": true,
		"eventID": true, "eventKind": true, "eventPubKey": true,
		"eventCreatedAt": true, "eventRaw": true,
	}
	got := map[string]bool{}
	for _, f := range s.Fields {
		got[f.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("schema missing field %q", name)
		}
	}
}

func TestValidateCalendar(t *testing.T) {
	cases := []struct {
		name       string
		evt        nostr.Event
		wantReject bool
	}{
		{"31923 ok", nostr.Event{Kind: 31923, Tags: nostr.Tags{{"d", "a"}, {"title", "T"}, {"start", "1"}}}, false},
		{"31923 no d", nostr.Event{Kind: 31923, Tags: nostr.Tags{{"title", "T"}, {"start", "1"}}}, true},
		{"31923 no title", nostr.Event{Kind: 31923, Tags: nostr.Tags{{"d", "a"}, {"start", "1"}}}, true},
		{"31923 no start", nostr.Event{Kind: 31923, Tags: nostr.Tags{{"d", "a"}, {"title", "T"}}}, true},
		{"31922 ok", nostr.Event{Kind: 31922, Tags: nostr.Tags{{"d", "a"}, {"title", "T"}, {"start", "2026-06-17"}}}, false},
		{"31924 ok", nostr.Event{Kind: 31924, Tags: nostr.Tags{{"d", "a"}, {"title", "T"}}}, false},
		{"31924 no title", nostr.Event{Kind: 31924, Tags: nostr.Tags{{"d", "a"}}}, true},
		{"31925 ok", nostr.Event{Kind: 31925, Tags: nostr.Tags{{"d", "a"}, {"a", "31923:pk:wd"}, {"status", "accepted"}}}, false},
		{"31925 no a", nostr.Event{Kind: 31925, Tags: nostr.Tags{{"d", "a"}, {"status", "accepted"}}}, true},
		{"31925 bad status", nostr.Event{Kind: 31925, Tags: nostr.Tags{{"d", "a"}, {"a", "x"}, {"status", "maybe"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reject, msg := validateCalendar(c.evt)
			if reject != c.wantReject {
				t.Errorf("validateCalendar = (%v, %q), want reject=%v", reject, msg, c.wantReject)
			}
		})
	}
}
