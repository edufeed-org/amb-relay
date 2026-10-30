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

// TestExpandOrderIndependent guards against Expand's output depending on the
// order of NewGraph's input slices: adjacency lists must be sorted so that
// which identifiers survive once MaxResults truncates is deterministic.
func TestExpandOrderIndependent(t *testing.T) {
	root := vocabmap.Concept{ID: "R", Address: "39738:pk:r"}
	c1 := vocabmap.Concept{ID: "C1", Address: "39738:pk:c1", Broader: []string{"39738:pk:r"}}
	c2 := vocabmap.Concept{ID: "C2", Address: "39738:pk:c2", Broader: []string{"39738:pk:r"}}
	c3 := vocabmap.Concept{ID: "C3", Address: "39738:pk:c3", Broader: []string{"39738:pk:r"}}
	concepts := []vocabmap.Concept{root, c1, c2, c3}

	mappings := []vocabmap.Mapping{
		mp("R", "exactMatch", "R2"),
		mp("R", "closeMatch", "K1"),
		mp("R", "closeMatch", "K2"),
		mp("R", "closeMatch", "K3"),
		mp("R", "relatedMatch", "X1"),
		mp("R", "relatedMatch", "X2"),
		mp("R", "relatedMatch", "X3"),
	}

	reverse := func(cs []vocabmap.Concept) []vocabmap.Concept {
		out := make([]vocabmap.Concept, len(cs))
		for i, c := range cs {
			out[len(cs)-1-i] = c
		}
		return out
	}
	reverseM := func(ms []vocabmap.Mapping) []vocabmap.Mapping {
		out := make([]vocabmap.Mapping, len(ms))
		for i, m := range ms {
			out[len(ms)-1-i] = m
		}
		return out
	}

	forward := vocabmap.NewGraph(concepts, mappings)
	reversed := vocabmap.NewGraph(reverse(concepts), reverseM(mappings))

	gotForward, truncForward := forward.Expand("R", true)
	gotReversed, truncReversed := reversed.Expand("R", true)

	if !reflect.DeepEqual(gotForward, gotReversed) || truncForward != truncReversed {
		t.Fatalf("Expand depends on input order: forward=%v (truncated=%v), reversed=%v (truncated=%v)",
			gotForward, truncForward, gotReversed, truncReversed)
	}

	want := []string{"R", "R2", "K1", "K2", "K3", "C1", "C2", "C3", "X1", "X2", "X3"}
	if !reflect.DeepEqual(gotForward, want) {
		t.Fatalf("Expand(R, true) = %v, want %v", gotForward, want)
	}
}
