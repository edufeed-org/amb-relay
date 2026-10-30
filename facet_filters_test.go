package main

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/typesense30142"
)

// Concept facets on the transferkiosk and calendar collections used to be
// filterable only by German label through NIP-50 search. A NIP-01 tag filter
// on the concept URI (#audience:id, the shape kind 30142 answers) went through
// the shared colon→dot rule to `audience.id`, a field neither schema has, and
// Typesense answered found:0 — indistinguishable from "no events match".
// Calendar #t had the same problem: `keywords` did not exist there.
//
// These pin projection, schema and tag mapping together, as the shares tests do.

func TestNostrToTransferkioskConceptIDs(t *testing.T) {
	ev := tkEvent(30144, nostr.Tags{
		{"d", "https://transferkiosk.net/p/1/act/2"},
		{"type", "TeachingMeasure"},
		{"name", "Maßnahme"},
		{"about:id", "https://transferkiosk.net/vocab/tk-massnahme-fachbereiche/1"},
		{"about:prefLabel:de", "Informatik"},
		{"about:id", "https://transferkiosk.net/vocab/tk-massnahme-faechergruppen/2"},
		{"about:prefLabel:de", "Ingenieurwissenschaften"},
		{"audience:id", "https://transferkiosk.net/vocab/tk-zielgruppen/100004"},
		{"audience:prefLabel:de", "Professor:innen"},
		{"actionField:id", "https://transferkiosk.net/vocab/tk-handlungsfeld/100002"},
		{"actionScope:id", "https://transferkiosk.net/vocab/tk-aktionsradius/100005"},
		{"transferability:id", "https://transferkiosk.net/vocab/tk-transferierbarkeit/100001"},
		{"ext:tk:fachbezug:id", "https://transferkiosk.net/vocab/tk-fachbezug/100001"},
	}, "")

	doc, err := nostrToTransferkiosk(ev)
	if err != nil {
		t.Fatalf("nostrToTransferkiosk: %v", err)
	}
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"AboutID", doc.AboutID, []string{"https://transferkiosk.net/vocab/tk-massnahme-fachbereiche/1", "https://transferkiosk.net/vocab/tk-massnahme-faechergruppen/2"}},
		{"AudienceID", doc.AudienceID, []string{"https://transferkiosk.net/vocab/tk-zielgruppen/100004"}},
		{"ActionFieldID", doc.ActionFieldID, []string{"https://transferkiosk.net/vocab/tk-handlungsfeld/100002"}},
		{"ActionScopeID", doc.ActionScopeID, []string{"https://transferkiosk.net/vocab/tk-aktionsradius/100005"}},
		{"TransferabilityID", doc.TransferabilityID, []string{"https://transferkiosk.net/vocab/tk-transferierbarkeit/100001"}},
		{"ExtTkFachbezugID", doc.ExtTkFachbezugID, []string{"https://transferkiosk.net/vocab/tk-fachbezug/100001"}},
	} {
		if strings.Join(c.got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// The label facets that NIP-50 search relies on stay as they were.
	if len(doc.Audience) != 1 || doc.Audience[0] != "Professor:innen" {
		t.Errorf("Audience labels = %v", doc.Audience)
	}
}

func TestTransferkioskTagFilters_ResolveToDeclaredFields(t *testing.T) {
	assertTagFieldsDeclared(t, "transferkioskSchema", transferkioskSchema("transferkiosk"), transferkioskTagFields())
}

func TestTransferkioskBackendFilterExpressions(t *testing.T) {
	ts := func() *typesense30142.TSBackend {
		return &typesense30142.TSBackend{CollectionName: "transferkiosk", SearchFields: "name,searchText,content", TagFields: transferkioskTagFields()}
	}
	for _, tc := range []struct {
		name   string
		filter nostr.Filter
		field  string
	}{
		{"Maßnahmen for a target group",
			nostr.Filter{Kinds: []nostr.Kind{30144}, Tags: nostr.TagMap{"audience:id": []string{"https://transferkiosk.net/vocab/tk-zielgruppen/100004"}}},
			"audienceId"},
		{"Projekte by subject group",
			nostr.Filter{Kinds: []nostr.Kind{30143}, Tags: nostr.TagMap{"about:id": []string{"https://transferkiosk.net/vocab/tk-faechergruppen/100002"}}},
			"aboutId"},
		{"Handlungsfeld",
			nostr.Filter{Kinds: []nostr.Kind{30144}, Tags: nostr.TagMap{"actionField:id": []string{"https://transferkiosk.net/vocab/tk-handlungsfeld/100002"}}},
			"actionFieldId"},
		{"ext:tk facet",
			nostr.Filter{Kinds: []nostr.Kind{30144}, Tags: nostr.TagMap{"ext:tk:fachbezug:id": []string{"https://transferkiosk.net/vocab/tk-fachbezug/100001"}}},
			"extTkFachbezugId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, called := stubFilterBy(t, ts(), tc.filter)
			if !called {
				t.Fatal("no request reached Typesense — the filter was reported unsatisfiable and would yield nothing")
			}
			if !strings.Contains(got, tc.field+":") {
				t.Errorf("filter_by = %q, want a clause on %q", got, tc.field)
			}
			if strings.Contains(got, ".id") {
				t.Errorf("filter_by = %q still uses the dotted field, which this collection does not have", got)
			}
		})
	}
}

func TestNostrToCalendarKeywordsAndConceptIDs(t *testing.T) {
	evt := nostr.Event{
		Kind:   31923,
		PubKey: nostr.MustPubKeyFromHex("0000000000000000000000000000000000000000000000000000000000000004"),
		Tags: nostr.Tags{
			{"d", "ws-1"},
			{"title", "Workshop"},
			{"start", "1718600000"},
			{"t", "studierende"},
			{"t", "hochschullehre"},
			{"about:id", "https://w3id.org/kim/hochschulfaechersystematik/n079"},
			{"about:prefLabel:de", "Informatik"},
			{"audience:id", "http://purl.org/dcx/lrmi-vocabs/educationalAudienceRole/teacher"},
			{"educationalLevel:id", "https://w3id.org/kim/educationalLevel/level_A"},
		},
	}
	evt.ID = evt.GetID()

	doc, err := nostrToCalendar(&evt)
	if err != nil {
		t.Fatalf("nostrToCalendar: %v", err)
	}
	for _, c := range []struct {
		name string
		got  []string
		want string
	}{
		{"Keywords", doc.Keywords, "studierende|hochschullehre"},
		{"AboutID", doc.AboutID, "https://w3id.org/kim/hochschulfaechersystematik/n079"},
		{"AudienceID", doc.AudienceID, "http://purl.org/dcx/lrmi-vocabs/educationalAudienceRole/teacher"},
		{"EducationalLevelID", doc.EducationalLevelID, "https://w3id.org/kim/educationalLevel/level_A"},
	} {
		if strings.Join(c.got, "|") != c.want {
			t.Errorf("%s = %v, want %s", c.name, c.got, c.want)
		}
	}
}

func TestCalendarTagFilters_ResolveToDeclaredFields(t *testing.T) {
	s := calendarSchema("calendar_31922")
	assertTagFieldsDeclared(t, "calendarSchema", s, calendarTagFields())
	// #t has no TagFields entry: the shared query path maps it to `keywords`.
	declared := false
	for _, f := range s.Fields {
		declared = declared || f.Name == "keywords"
	}
	if !declared {
		t.Error("calendarSchema declares no `keywords` field, so #t returns 0 for every input")
	}
}

func TestCalendarBackendFilterExpressions(t *testing.T) {
	ts := func() *typesense30142.TSBackend {
		return &typesense30142.TSBackend{CollectionName: "calendar_31922", SearchFields: "title,summary,content,location", TagFields: calendarTagFields()}
	}
	for _, tc := range []struct {
		name   string
		filter nostr.Filter
		field  string
	}{
		{"#t", nostr.Filter{Kinds: []nostr.Kind{31923}, Tags: nostr.TagMap{"t": []string{"studierende"}}}, "keywords"},
		{"#about:id", nostr.Filter{Kinds: []nostr.Kind{31923}, Tags: nostr.TagMap{"about:id": []string{"https://w3id.org/kim/hochschulfaechersystematik/n079"}}}, "aboutId"},
		{"#audience:id", nostr.Filter{Kinds: []nostr.Kind{31922}, Tags: nostr.TagMap{"audience:id": []string{"http://purl.org/dcx/lrmi-vocabs/educationalAudienceRole/teacher"}}}, "audienceId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, called := stubFilterBy(t, ts(), tc.filter)
			if !called {
				t.Fatal("no request reached Typesense")
			}
			if !strings.Contains(got, tc.field+":") {
				t.Errorf("filter_by = %q, want a clause on %q", got, tc.field)
			}
		})
	}
}

func assertTagFieldsDeclared(t *testing.T, schemaName string, s typesense30142.CollectionSchema, tagFields map[string]string) {
	t.Helper()
	declared := make(map[string]typesense30142.Field)
	for _, f := range s.Fields {
		declared[f.Name] = f
	}
	if len(tagFields) == 0 {
		t.Fatalf("no tag fields declared for %s", schemaName)
	}
	for tag, field := range tagFields {
		f, ok := declared[field]
		if !ok {
			t.Errorf("#%s maps to %q, which %s does not declare — the filter would return 0 for every input", tag, field, schemaName)
			continue
		}
		if f.Type != "string[]" || !f.Optional {
			t.Errorf("%s.%s should be an optional string[], got %+v", schemaName, field, f)
		}
	}
}

// The AMB backend aliases the expander's synthetic tag keys onto `concepts`;
// the schema must declare that field or every expanded query fails.
func TestAMBConceptTagFieldsDeclared(t *testing.T) {
	assertTagFieldsDeclared(t, "DefaultSchema", typesense30142.DefaultSchema(), conceptTagFields())
}
