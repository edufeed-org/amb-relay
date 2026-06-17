# Calendar Absorption (NIP-52) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Absorb `calendar-relay` (NIP-52 kinds 31922/31923/31924/31925) into `amb-relay` as a `CALENDAR_ENABLED`-gated content type, dual-indexed into Typesense (full-text/kind/tag) and the existing nostrlib BoltDB start/end/geohash index (range/location).

**Architecture:** A new `calendar` `contentType` is appended to the existing registry (`content_registry.go`) exactly like long-form (30023) and wiki (30818) were. Projection/schema/validation live in a new `calendar.go` mirroring `longform.go`/`wiki.go`. A `calendar.CalendarStore` (from `fiatjaf.com/nostr/khatru/calendar`) wrapping the shared `boltDB` supplies the Bolt range/geohash index and the range-query path; a new Typesense backend `tsDB4` supplies full-text. Reindex reuses the existing `structuredReindexTarget` mechanism — no `reindex.go` change.

**Tech Stack:** Go, khatru relay framework, Typesense (`fiatjaf.com/nostr/eventstore/typesense30142`), BoltDB (`fiatjaf.com/nostr/eventstore/boltdb`), `fiatjaf.com/nostr/khatru/calendar`, `fiatjaf.com/nostr/nip52`.

## Global Constraints

- **No `nostrlib` changes.** Every calendar primitive is already exported. Do not edit anything under `../nostrlib`.
- **Flag-off byte-identity.** With `CALENDAR_ENABLED` unset, `tsDB4` is `nil`, no `CalendarStore` is created, the calendar `contentType` is absent, and `structuredTargets` gains nothing — behavior must be byte-for-byte unchanged.
- **Build verification command:** `GOWORK=off go build .` (standalone, pinned nostrlib) **and** `go build .` (workspace). Run **both**; both must succeed.
- **Test command:** `go test ./...` from the `amb-relay` directory.
- **Sandbox:** every `go`/`git` command must be run with `dangerouslyDisableSandbox: true` — they fail under the default sandbox.
- **Git safety:** never push, merge, force-push, or use `--no-verify`. Commit locally on branch `dev` only. Never commit `.env` or secrets.
- **Addressing convention (verbatim from existing code):** Typesense doc id = `typesense30142.GenerateDocumentID(event.PubKey.Hex(), dTag)`.
- **Envelope helpers (existing, in `structured.go`):** `newStructuredEnvelope(event) (structuredEnvelope, error)`, `structuredEnvelopeFields() []typesense30142.Field`, `storeStructured[T](enabled bool, ts *typesense30142.TSBackend, event nostr.Event, label string, project func(*nostr.Event)(*T,error))`, `reprojectStructured[T](ts, event, project) error`.

---

### Task 1: Calendar projection + schema (`calendar.go`)

**Files:**
- Create: `calendar.go`
- Test: `calendar_test.go`

**Interfaces:**
- Consumes: `structuredEnvelope`, `newStructuredEnvelope`, `structuredEnvelopeFields` (from `structured.go`); `typesense30142.GenerateDocumentID`, `typesense30142.CollectionSchema`, `typesense30142.Field`; `calendar.IsCalendarEventKind`; `nip52.ParseCalendarEvent`.
- Produces: `type CalendarDocument`, `func nostrToCalendar(*nostr.Event) (*CalendarDocument, error)`, `func storeCalendar(bool, *typesense30142.TSBackend, nostr.Event)`, `func calendarSchema(string) typesense30142.CollectionSchema`.

- [ ] **Step 1: Write the failing tests**

Create `calendar_test.go`:

```go
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
	// 2026-06-17T00:00:00Z = 1781481600
	if doc.Start != 1781481600 {
		t.Errorf("Start = %d, want 1781481600 (2026-06-17 UTC)", doc.Start)
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run (with `dangerouslyDisableSandbox: true`): `go test ./... -run TestNostrToCalendar -v`
Expected: FAIL — `undefined: nostrToCalendar`, `undefined: calendarSchema`.

- [ ] **Step 3: Write `calendar.go` (projection + schema part)**

Create `calendar.go`:

```go
package main

import (
	"fmt"

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

	if calendar.IsCalendarEventKind(event.Kind) { // 31922 / 31923
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestNostrToCalendar -v` then `go test ./... -run TestCalendarSchemaFields -v`
Expected: PASS. (If `TestNostrToCalendarDateBased` fails on the exact Unix value, confirm `nip52.DateFormat` parses as UTC; `time.Parse("2006-01-02", "2026-06-17").Unix()` = `1781481600`.)

- [ ] **Step 5: Commit**

```bash
git add calendar.go calendar_test.go
git commit -m "feat(calendar): nostrToCalendar projection + calendar Typesense schema"
```

---

### Task 2: Calendar validation (`validate.go`)

**Files:**
- Modify: `validate.go`
- Test: `calendar_test.go`

**Interfaces:**
- Consumes: `nostr.Event`, `event.Tags.GetD()`, `event.Tags.Has(string)`, `event.Tags.Find(string)`.
- Produces: `func validateCalendar(nostr.Event) (reject bool, msg string)`.

- [ ] **Step 1: Write the failing test**

Append to `calendar_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestValidateCalendar -v`
Expected: FAIL — `undefined: validateCalendar`.

- [ ] **Step 3: Add `validateCalendar` to `validate.go`**

Append to `validate.go` (after `validateWiki`):

```go
func validateCalendar(event nostr.Event) (reject bool, msg string) {
	switch event.Kind {
	case 31922, 31923: // date/time calendar events
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("title") {
			return true, "missing required 'title' tag"
		}
		if !event.Tags.Has("start") {
			return true, "missing required 'start' tag"
		}
	case 31924: // calendar
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("title") {
			return true, "missing required 'title' tag"
		}
	case 31925: // RSVP
		if event.Tags.GetD() == "" {
			return true, "missing required 'd' tag"
		}
		if !event.Tags.Has("a") {
			return true, "missing required 'a' tag"
		}
		status := ""
		if tag := event.Tags.Find("status"); tag != nil {
			status = tag[1]
		}
		if status != "accepted" && status != "declined" && status != "tentative" {
			return true, "missing or invalid 'status' tag (accepted/declined/tentative)"
		}
	}
	return false, ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestValidateCalendar -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add validate.go calendar_test.go
git commit -m "feat(calendar): validateCalendar NIP-52 structural checks"
```

---

### Task 3: Calendar fetch router (`calendar.go`)

**Files:**
- Modify: `calendar.go`
- Test: `calendar_test.go`

**Interfaces:**
- Consumes: `fetchFunc` (= `func(nostr.Filter, int) iter.Seq[nostr.Event]`, from `content_registry.go`); `calendar.ExtractCalendarFilter`, `CalendarFilter.HasCalendarParams`, `calendar.HasCalendarKinds`.
- Produces: `func calendarFetch(boltFetch, tsFetch fetchFunc) fetchFunc`.

Routing rule: when the filter carries calendar range/geo params (`#start_after`, `#start_before`, `#end_after`, `#end_before`, `#g`) **and** targets a calendar event kind (31922/31923), route to `boltFetch` (the Bolt range/geo index); otherwise route to `tsFetch` (Typesense full-text/kind/tag). Taking two `fetchFunc` closures (rather than the concrete `*CalendarStore`/`*TSBackend`) keeps the decision unit-testable with fakes.

- [ ] **Step 1: Write the failing test**

Append to `calendar_test.go` (note: `iter`, `fakeStore`, `collectEvents`, `idsOf` already exist in `query_combined_integration_test.go` — reuse them, do not redefine):

```go
// The router sends range/geo queries to the Bolt fetch and everything else
// (plain kind listing, full-text search, RSVP/calendar kinds) to Typesense.
func TestCalendarFetchRouter(t *testing.T) {
	bolt := &fakeStore{}
	ts := &fakeStore{}
	fetch := calendarFetch(bolt.fetch, ts.fetch)

	// start_after present + event kind -> Bolt path.
	collectEvents(fetch(nostr.Filter{
		Kinds: []nostr.Kind{31923},
		Tags:  nostr.TagMap{"start_after": []string{"1718600000"}},
	}, 100))
	if len(bolt.calls) != 1 || len(ts.calls) != 0 {
		t.Fatalf("range query: bolt=%d ts=%d, want bolt=1 ts=0", len(bolt.calls), len(ts.calls))
	}

	// geohash present + event kind -> Bolt path.
	collectEvents(fetch(nostr.Filter{
		Kinds: []nostr.Kind{31922, 31923},
		Tags:  nostr.TagMap{"g": []string{"u33d"}},
	}, 100))
	if len(bolt.calls) != 2 {
		t.Fatalf("geohash query: bolt=%d, want 2", len(bolt.calls))
	}

	// search, no calendar params -> Typesense path.
	collectEvents(fetch(nostr.Filter{Kinds: []nostr.Kind{31923}, Search: "yoga"}, 100))
	if len(ts.calls) != 1 {
		t.Fatalf("search query: ts=%d, want 1", len(ts.calls))
	}

	// RSVP kind (31925) is never a calendar-event kind -> Typesense even with params.
	collectEvents(fetch(nostr.Filter{
		Kinds: []nostr.Kind{31925},
		Tags:  nostr.TagMap{"start_after": []string{"1"}},
	}, 100))
	if len(ts.calls) != 2 {
		t.Fatalf("rsvp query: ts=%d, want 2", len(ts.calls))
	}

	// plain kind listing, no params -> Typesense path.
	collectEvents(fetch(nostr.Filter{Kinds: []nostr.Kind{31923}}, 100))
	if len(ts.calls) != 3 {
		t.Fatalf("plain query: ts=%d, want 3", len(ts.calls))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestCalendarFetchRouter -v`
Expected: FAIL — `undefined: calendarFetch`.

- [ ] **Step 3: Add `calendarFetch` to `calendar.go`**

Add the `iter` import to `calendar.go`'s import block and append:

```go
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
```

The import block of `calendar.go` becomes:

```go
import (
	"fmt"
	"iter"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
	"fiatjaf.com/nostr/khatru/calendar"
	"fiatjaf.com/nostr/nip52"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestCalendarFetchRouter -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add calendar.go calendar_test.go
git commit -m "feat(calendar): fetch router (Bolt range/geo vs Typesense full-text)"
```

---

### Task 4: Wire calendar into `main.go`

**Files:**
- Modify: `main.go`

**Interfaces:**
- Consumes: `calendar.NewCalendarStore(eventstore.Store, string) (*calendar.CalendarStore, error)`, `(*CalendarStore).GetIndex() *calendar.CalendarIndex`, `(*CalendarIndex).IndexEvent(nostr.Event) error`, `(*CalendarIndex).RemoveEvent(nostr.ID) error`, `(*CalendarIndex).Close() error`, `(*CalendarStore).QueryEvents(nostr.Filter, int) iter.Seq[nostr.Event]`; `calendar.IsCalendarEventKind`; `nostrToCalendar`, `storeCalendar`, `calendarSchema`, `calendarFetch`, `validateCalendar`, `reprojectStructured`; existing `contentType`, `structuredReindexTarget`, `fetchFunc`.
- Produces: a running relay that, when `CALENDAR_ENABLED=true`, accepts/stores/queries/reindexes kinds 31922-31925; when unset, is byte-identical to before.

This task is integration glue; its deliverable gate is a green `go build` (both modes) + full `go test ./...` (existing tests still pass, proving flag-off identity). Make the six edits below.

- [ ] **Step 1: Add the flag and retention kinds**

In `main.go`, after line `wikiEnabled := os.Getenv("WIKI_ENABLED") == "true"` add:

```go
	calendarEnabled := os.Getenv("CALENDAR_ENABLED") == "true"
```

In the retention block (currently appends 30023/30818), after the `wikiEnabled` append add:

```go
	if calendarEnabled {
		retentionKinds = append(retentionKinds, []int{31922}, []int{31923}, []int{31924}, []int{31925})
	}
```

- [ ] **Step 2: Add the import**

Add to `main.go`'s import block (alphabetical, beside the other `khatru/...` imports):

```go
	"fiatjaf.com/nostr/khatru/calendar"
```

- [ ] **Step 3: Init `tsDB4` and the `CalendarStore`**

Immediately after the wiki `tsDB3` init block (ends `fmt.Printf("Wiki (kind 30818) enabled — collection %s\n", wikiColl)` then `}`), insert:

```go
	// Calendar (NIP-52 kinds 31922-31925) backends — gated behind CALENDAR_ENABLED.
	// Dual-indexed: a Typesense collection (full-text/kind/tag) plus the nostrlib
	// calendar BoltDB index (start/end/geohash range queries). Both nil/unused
	// when the flag is off, so the registry simply omits calendar.
	var tsDB4 *typesense30142.TSBackend
	var calStore *calendar.CalendarStore
	if calendarEnabled {
		calColl := os.Getenv("TS_COLLECTION_CALENDAR")
		if calColl == "" {
			calColl = "calendar_31922"
		}
		calSchema := calendarSchema(calColl)
		tsDB4 = &typesense30142.TSBackend{
			ApiKey:         os.Getenv("TS_APIKEY"),
			Host:           os.Getenv("TS_HOST"),
			CollectionName: calColl,
			RawEventStore:  &boltDB,
			Schema:         &calSchema,
		}
		if err := tsDB4.Init(); err != nil {
			panic(fmt.Sprintf("calendar TSBackend init: %v", err))
		}

		calIndexPath := os.Getenv("CALENDAR_INDEX_PATH")
		if calIndexPath == "" {
			calIndexPath = "./data/calendar_index.db"
		}
		calStore, err = calendar.NewCalendarStore(&boltDB, calIndexPath)
		if err != nil {
			panic(fmt.Sprintf("calendar index init: %v", err))
		}
		// Close ONLY the index DB on shutdown. calStore.Close() would also close
		// the shared boltDB (double close); boltDB has its own defer in main.
		defer calStore.GetIndex().Close()
		fmt.Printf("Calendar (NIP-52) enabled — collection %s, index %s\n", calColl, calIndexPath)
	}
```

(Note: `err` here is the existing `err` variable already in scope from earlier `:=` uses in `main`; use `=`, not `:=`, for the `calStore, err = ...` assignment as written. If the compiler reports `calStore` is only assigned, that is fine — it is read below when `calendarEnabled`.)

- [ ] **Step 4: Append the calendar `contentType`**

After the `if wikiEnabled && tsDB3 != nil { ... }` block that appends to `contentTypes`, add:

```go
	if calendarEnabled && tsDB4 != nil {
		contentTypes = append(contentTypes, contentType{
			kinds:    []nostr.Kind{31922, 31923, 31924, 31925},
			validate: validateCalendar,
			store: func(e nostr.Event) {
				if calendar.IsCalendarEventKind(e.Kind) {
					if err := calStore.GetIndex().IndexEvent(e); err != nil {
						fmt.Printf("calendar index %s: %v\n", e.ID.Hex(), err)
					}
				}
				storeCalendar(true, tsDB4, e)
			},
			fetch:    calendarFetch(calStore.QueryEvents, tsDB4.QueryEvents),
			count:    tsDB4.CountEvents,
			deleteID: func(id nostr.ID) error {
				_ = calStore.GetIndex().RemoveEvent(id) // best-effort index cleanup
				return tsDB4.DeleteEvent(id)
			},
		})
	}
```

- [ ] **Step 5: Append the calendar reindex target**

After the `if wikiEnabled && tsDB3 != nil { ... }` block that appends to `structuredTargets`, add:

```go
	if calendarEnabled && tsDB4 != nil {
		structuredTargets = append(structuredTargets, structuredReindexTarget{
			label:    "calendar",
			kinds:    []nostr.Kind{31922, 31923, 31924, 31925},
			recreate: func() error { return tsDB4.RecreateCollection(tsDB4.Schema) },
			reproject: func(e nostr.Event) error {
				if calendar.IsCalendarEventKind(e.Kind) {
					if err := calStore.GetIndex().IndexEvent(e); err != nil {
						return err
					}
				}
				return reprojectStructured(tsDB4, e, nostrToCalendar)
			},
		})
	}
```

- [ ] **Step 6: Build (both modes) and run the full suite**

Run (each with `dangerouslyDisableSandbox: true`):

```bash
gofmt -l calendar.go validate.go main.go
go build .
GOWORK=off go build .
go test ./...
```

Expected: `gofmt -l` prints nothing (already formatted); both builds succeed; `go test ./...` PASS. The pre-existing tests passing with `CALENDAR_ENABLED` unset is the flag-off byte-identity gate.

If `gofmt -l` lists a file, run `gofmt -w <file>` and include it in the commit.

- [ ] **Step 7: Commit**

```bash
git add main.go
git commit -m "feat(calendar): wire NIP-52 contentType + reindex target behind CALENDAR_ENABLED"
```

---

### Task 5: Documentation (`.env.example`, `CLAUDE.md`)

**Files:**
- Modify: `.env.example`
- Modify: `CLAUDE.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: Add env vars to `.env.example`**

In `.env.example`, after the `TS_COLLECTION_WIKI="wiki_30818"` line, add:

```bash
# Calendar (NIP-52 kinds 31922/31923/31924/31925) support (optional). When
# enabled, the relay also accepts calendar events and dual-indexes them: a
# SEPARATE Typesense collection (full-text/kind/tag) plus a BoltDB start/end/
# geohash index for range and location queries (#start_after, #start_before,
# #end_after, #end_before, #g). The search path merges all enabled collections.
CALENDAR_ENABLED="false"
# Collection name for the calendar structured fields (default calendar_31922).
TS_COLLECTION_CALENDAR="calendar_31922"
# BoltDB file for the calendar start/end/geohash index (default ./data/calendar_index.db).
CALENDAR_INDEX_PATH="./data/calendar_index.db"
```

- [ ] **Step 2: Add a Calendar section + canonical query shapes to `CLAUDE.md`**

In `CLAUDE.md`, after the existing `**Wiki (NIP-54 kind-30818) ...**` block (just before `**Typesense Schema Management:**`), insert:

```markdown
**Calendar (NIP-52 kinds 31922/31923/31924/31925), gated behind `CALENDAR_ENABLED`:**
- When `CALENDAR_ENABLED=true`, the relay accepts NIP-52 date events (31922), time events (31923), calendars (31924), and RSVPs (31925). Validation (`validateCalendar`): event kinds require `d`+`title`+`start`; calendars require `d`+`title`; RSVPs require `d`+`a`+`status` ∈ {accepted,declined,tentative}.
- Calendar events are **dual-indexed**. Structured fields project (`nostrToCalendar`, `calendar.go`) into a SEPARATE Typesense collection (`calendar_31922` / `TS_COLLECTION_CALENDAR`) for full-text/kind/tag search; the nostrlib `khatru/calendar` BoltDB index (`CALENDAR_INDEX_PATH`, default `./data/calendar_index.db`) holds start/end/geohash for range and location queries. A `CalendarStore` wraps the shared BoltDB but is used ONLY for its index + range-query path (never `SaveEvent`/`Close`, since boltBuf persists and boltDB is closed elsewhere).
- The calendar `contentType`'s `fetch` routes per query (`calendarFetch`): a filter carrying range/geo params (`#start_after`, `#start_before`, `#end_after`, `#end_before`, `#g`) over the event kinds (31922/31923) hits the Bolt index; everything else — full-text `search`, plain kind listing, 31924/31925 — hits Typesense. When both range params and `search` are present, the Bolt path wins and `search` is ignored for that REQ.
- Reindex appends a `structuredReindexTarget` (in `main.go`) covering all four kinds: one BoltDB pass rebuilds both the Typesense collection and the Bolt index (`reproject` indexes event kinds then upserts to Typesense). The Bolt index is not cleared first, so entries for events no longer in BoltDB persist as harmless orphans.

**Canonical query shapes (the LLM/MCP contract):** the foundation for a future calendar MCP server that translates natural language into these filters.
- "Events in the next week" (time-based): `{"kinds":[31923],"#start_after":["<now>"],"#start_before":["<now+7d>"],"limit":100}` → Bolt time index. Date-based (31922) is identical; `start` "YYYY-MM-DD" is stored as the Unix start-of-day.
- "Events near a location": `{"kinds":[31922,31923],"#g":["u33d"],"limit":100}` → Bolt geohash prefix (prefix length = radius).
- "Events about a topic": `{"kinds":[31922,31923,31924],"search":"mathematik","limit":50}` → Typesense full-text.
- Combined intent ("next week, near Berlin, about math") is composed client-side: issue the Bolt range/geo REQ, then post-filter by topic against the returned events (the full event travels in `eventRaw`).
```

- [ ] **Step 3: Verify the docs render and are accurate**

Re-read the inserted `CLAUDE.md` section and `.env.example` lines. Confirm: env var names match `main.go` (`CALENDAR_ENABLED`, `TS_COLLECTION_CALENDAR`, `CALENDAR_INDEX_PATH`); defaults match (`calendar_31922`, `./data/calendar_index.db`); the four kinds and validation rules match `validateCalendar`.

- [ ] **Step 4: Commit**

```bash
git add .env.example CLAUDE.md
git commit -m "docs(calendar): document CALENDAR_ENABLED, dual-index, canonical query shapes"
```

---

## Final review (after all tasks)

- [ ] Run the full suite once more: `go test ./...` (with `dangerouslyDisableSandbox: true`). Expected PASS.
- [ ] Confirm both builds: `go build .` and `GOWORK=off go build .`. Expected: both succeed.
- [ ] Dispatch a final code reviewer over the whole branch diff, then use `superpowers:finishing-a-development-branch`.

## Notes on scope (from the spec)

- **No data migration here.** Moving existing `calendar-relay` events into amb-relay (negentropy sync or BoltDB copy + reindex) is an operational follow-up.
- **No MCP server here.** The `events_in_range` / `events_near` / `events_by_topic` natural-language tooling is the next sub-project (Spec B); this plan only delivers and documents the canonical filters it will emit.
