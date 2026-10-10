package vocabmap_test

import (
	"reflect"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/edufeed-org/amb-relay/vocabmap"
	vt "github.com/edufeed-org/amb-relay/vocabmap/vocabmaptest"
)

// Vectors computed with nostr-vocab-core 0.3.1 mappingIdentifier (the JS
// reference implementation of the NIP-VOCAB sameness identifier).
func TestMappingIdentifierVectors(t *testing.T) {
	cases := []struct {
		from, rel string
		to        []string
		want      string
	}{
		{"https://example.org/a", "exactMatch", []string{"https://example.org/b"},
			"mapping:1a14a04c5ab1c27eafc07b372852f727bf73f5332da3536ed4c18ee7db254960"},
		{"nostr:39738:pk:5-6", "broadMatch", []string{"https://w3id.org/kim/educationalLevel/level_2"},
			"mapping:b7f659c41777195ddfb0024fa49fcaa37b278f250049606e84f2e57d61eed241"},
		// decomposed é (NFD) + duplicate and unsorted targets
		{"https://x/Café", "closeMatch", []string{"https://y/z", "https://y/b", "https://y/z"},
			"mapping:68c8b1a4a104ebe7319644a3e9eff24fd576da1f8f4d70fcea3adda553bf5c7d"},
	}
	for _, c := range cases {
		if got := vocabmap.MappingIdentifier(c.from, c.to, c.rel); got != c.want {
			t.Errorf("MappingIdentifier(%q,%v,%q) = %s, want %s", c.from, c.to, c.rel, got, c.want)
		}
	}
}

func TestParseMapping(t *testing.T) {
	part := vt.ConcordanceAddr(vt.Owner, "c")
	ev := vt.Mapping(vt.Owner, "A", "broadMatch", "B", "S1", "S2", part, vt.SchemeAddr(vt.Owner, "s1"))
	m, ok := vocabmap.ParseMapping(ev)
	if !ok {
		t.Fatal("valid mapping rejected")
	}
	want := vocabmap.Mapping{D: ev.Tags.Find("d")[1], Relation: "broadMatch", From: "A", To: []string{"B"},
		FromScheme: "S1", ToScheme: "S2", PartOf: part, Author: vt.Owner.Hex(), Schemes: []string{vt.SchemeAddr(vt.Owner, "s1")}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	bad := ev
	bad.Tags = append(nostr.Tags{}, ev.Tags...)
	bad.Tags[0] = nostr.Tag{"d", "mapping:wrong"}
	if _, ok := vocabmap.ParseMapping(bad); ok {
		t.Error("d != sameness identifier must be rejected")
	}
	twoRel := ev
	twoRel.Tags = append(append(nostr.Tags{}, ev.Tags...), nostr.Tag{"relation", "exactMatch"})
	if _, ok := vocabmap.ParseMapping(twoRel); ok {
		t.Error("two relation tags must be rejected")
	}
	unknown := vt.Mapping(vt.Owner, "A", "sameAs", "B", "S1", "S2", "")
	unknown.Tags[0] = nostr.Tag{"d", vocabmap.MappingIdentifier("A", []string{"B"}, "exactMatch")}
	if _, ok := vocabmap.ParseMapping(unknown); ok {
		t.Error("unknown relation must be rejected")
	}
}

func TestParseConcept(t *testing.T) {
	c, ok := vocabmap.ParseConcept(vt.Concept(vt.Owner, "schulart", "berufsschule", "", "berufliche-schule"))
	if !ok {
		t.Fatal("rejected")
	}
	if c.ID != vt.ID(vt.Owner, "berufsschule") || c.Address != "39738:"+vt.Owner.Hex()+":berufsschule" {
		t.Errorf("id/address = %q %q", c.ID, c.Address)
	}
	if !reflect.DeepEqual(c.Broader, []string{"39738:" + vt.Owner.Hex() + ":berufliche-schule"}) ||
		!reflect.DeepEqual(c.Schemes, []string{vt.SchemeAddr(vt.Owner, "schulart")}) {
		t.Errorf("broader/schemes = %v %v", c.Broader, c.Schemes)
	}
	withURI, _ := vocabmap.ParseConcept(vt.Concept(vt.Owner, "educational-level", "level_a", "https://w3id.org/kim/educationalLevel/level_A"))
	if withURI.ID != "https://w3id.org/kim/educationalLevel/level_A" {
		t.Errorf("i tag must win: %q", withURI.ID)
	}
}

func TestMembers(t *testing.T) {
	addr := vt.ConcordanceAddr(vt.Owner, "c")
	conc, ok := vocabmap.ParseConcordance(vt.Concordance(vt.Owner, "c", "S1", "S2", nil, vt.Maintainer))
	if !ok || conc.Address != addr || !reflect.DeepEqual(conc.Maintainers, []string{vt.Maintainer.Hex()}) {
		t.Fatalf("concordance = %+v ok=%v", conc, ok)
	}
	parse := func(ev nostr.Event) vocabmap.Mapping { m, _ := vocabmap.ParseMapping(ev); return m }
	owner := parse(vt.Mapping(vt.Owner, "A", "exactMatch", "B", "S1", "S2", addr))
	maint := parse(vt.Mapping(vt.Maintainer, "C", "exactMatch", "D", "S1", "S2", addr))
	stranger := parse(vt.Mapping(vt.Other, "E", "exactMatch", "F", "S1", "S2", addr))
	wrongScheme := parse(vt.Mapping(vt.Owner, "G", "exactMatch", "H", "S1", "S9", addr))
	otherConc := parse(vt.Mapping(vt.Owner, "I", "exactMatch", "J", "S1", "S2", vt.ConcordanceAddr(vt.Owner, "x")))
	got := vocabmap.Members(conc, []vocabmap.Mapping{owner, maint, stranger, wrongScheme, otherConc, owner})
	if !reflect.DeepEqual(got, []vocabmap.Mapping{owner, maint}) {
		t.Fatalf("members = %+v", got)
	}
}

func TestLatestByAddress(t *testing.T) {
	old := vt.Concept(vt.Owner, "s", "a", "")
	newer := vt.Concept(vt.Owner, "s", "a", "https://new")
	newer.CreatedAt = 2
	other := vt.Concept(vt.Owner, "s", "b", "")
	got := vocabmap.LatestByAddress([]nostr.Event{old, other, newer})
	if len(got) != 2 || got[0].Tags.Find("i")[1] != "https://new" || got[1].Tags.Find("d")[1] != "b" {
		t.Fatalf("latest = %+v", got)
	}
}
