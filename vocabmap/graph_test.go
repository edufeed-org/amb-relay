package vocabmap_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/edufeed-org/amb-relay/vocabmap"
)

func mp(from, rel, to string) vocabmap.Mapping {
	return vocabmap.Mapping{From: from, Relation: rel, To: []string{to}}
}

func expand(t *testing.T, g *vocabmap.Graph, id string, related bool, want ...string) {
	t.Helper()
	got, _ := g.Expand(id, related)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand(%s, related=%v) = %v, want %v", id, related, got, want)
	}
}

func TestExpandNarrowingDirection(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("G56", "broadMatch", "L2"), mp("L2", "narrowMatch", "HS")})
	expand(t, g, "L2", false, "L2", "G56", "HS")
	expand(t, g, "G56", false, "G56") // never broader
	expand(t, g, "HS", false, "HS")
}

func TestExpandExactTransitiveCloseOneHop(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{
		mp("A", "exactMatch", "B"), mp("B", "exactMatch", "C"),
		mp("C", "closeMatch", "D"), mp("D", "closeMatch", "E"),
	})
	expand(t, g, "A", false, "A", "B", "C", "D") // E is two close hops away
}

func TestExpandInSchemeHierarchyWithURIs(t *testing.T) {
	parent := vocabmap.Concept{ID: "https://p", Address: "39738:pk:p"}
	child := vocabmap.Concept{ID: "nostr:39738:pk:c", Address: "39738:pk:c", Broader: []string{"39738:pk:p"}}
	grandchild := vocabmap.Concept{ID: "nostr:39738:pk:g", Address: "39738:pk:g", Broader: []string{"39738:pk:c"}}
	g := vocabmap.NewGraph([]vocabmap.Concept{parent, child, grandchild}, []vocabmap.Mapping{mp("L3", "narrowMatch", "https://p")})
	expand(t, g, "L3", false, "L3", "https://p", "nostr:39738:pk:c", "nostr:39738:pk:g")
}

func TestExpandNarrowerTakesExactClosure(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("G56", "broadMatch", "L2"), mp("G56", "exactMatch", "X")})
	expand(t, g, "L2", false, "L2", "G56", "X")
}

func TestExpandRelatedOnlyOnRequestAndNotChained(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("GYM", "relatedMatch", "L3"), mp("GYM", "exactMatch", "Y")})
	expand(t, g, "L3", false, "L3")
	expand(t, g, "L3", true, "L3", "GYM")
}

func TestExpandConflictDropsExact(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{mp("A", "exactMatch", "B"), mp("A", "broadMatch", "B")})
	expand(t, g, "A", false, "A")
	expand(t, g, "B", false, "B", "A")
}

func TestExpandIgnoresOneToMany(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{{From: "A", Relation: "exactMatch", To: []string{"B", "C"}}})
	expand(t, g, "A", false, "A")
}

func TestExpandCyclesTerminate(t *testing.T) {
	g := vocabmap.NewGraph(nil, []vocabmap.Mapping{
		mp("A", "exactMatch", "B"), mp("B", "exactMatch", "A"),
		mp("C", "broadMatch", "A"), mp("A", "broadMatch", "C"),
	})
	expand(t, g, "A", false, "A", "B", "C")
}

func TestExpandDepthCap(t *testing.T) {
	var ms []vocabmap.Mapping
	for i := 0; i < 6; i++ {
		ms = append(ms, mp(fmt.Sprintf("N%d", i+1), "broadMatch", fmt.Sprintf("N%d", i)))
	}
	g := vocabmap.NewGraph(nil, ms)
	expand(t, g, "N0", false, "N0", "N1", "N2", "N3", "N4") // depth 4
}

func TestExpandResultCap(t *testing.T) {
	var ms []vocabmap.Mapping
	for i := 0; i < 250; i++ {
		ms = append(ms, mp(fmt.Sprintf("C%03d", i), "broadMatch", "ROOT"))
	}
	ids, truncated := vocabmap.NewGraph(nil, ms).Expand("ROOT", false)
	if len(ids) != vocabmap.MaxResults || !truncated || ids[0] != "ROOT" {
		t.Fatalf("len=%d truncated=%v first=%s", len(ids), truncated, ids[0])
	}
}

func TestExpandUnknownID(t *testing.T) {
	ids, truncated := vocabmap.NewGraph(nil, nil).Expand("Z", true)
	if !reflect.DeepEqual(ids, []string{"Z"}) || truncated {
		t.Fatalf("got %v %v", ids, truncated)
	}
}
