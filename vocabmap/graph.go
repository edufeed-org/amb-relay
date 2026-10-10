package vocabmap

const (
	// MaxDepth bounds the narrowing step of Expand.
	MaxDepth = 4
	// MaxResults bounds the identifiers Expand returns per queried identifier.
	MaxResults = 200
)

// Graph holds the trusted relations between concept identifiers. exact, close
// and related are symmetric; narrower maps a concept to its narrower ones.
type Graph struct {
	exact, close, related, narrower map[string][]string
	has                             map[[3]string]bool
}

func (g *Graph) link(rel, from, to string) {
	key := [3]string{rel, from, to}
	if from == to || g.has[key] {
		return
	}
	g.has[key] = true
	switch rel {
	case "exact":
		g.exact[from] = append(g.exact[from], to)
	case "close":
		g.close[from] = append(g.close[from], to)
	case "related":
		g.related[from] = append(g.related[from], to)
	case "narrower":
		g.narrower[from] = append(g.narrower[from], to)
	}
}

func (g *Graph) both(rel, a, b string) {
	g.link(rel, a, b)
	g.link(rel, b, a)
}

// NewGraph builds the graph from in-scheme hierarchy links and 1:1 mappings.
// A pair that is exactMatch and also broad/narrow/related contributes no
// exact edge (NIP-VOCAB conflict rule).
func NewGraph(concepts []Concept, mappings []Mapping) *Graph {
	g := &Graph{
		exact: map[string][]string{}, close: map[string][]string{},
		related: map[string][]string{}, narrower: map[string][]string{},
		has: map[[3]string]bool{},
	}
	idByAddr := make(map[string]string, len(concepts))
	for _, c := range concepts {
		idByAddr[c.Address] = c.ID
	}
	resolve := func(addr string) string {
		if id, ok := idByAddr[addr]; ok {
			return id
		}
		return "nostr:" + addr
	}
	for _, c := range concepts {
		for _, b := range c.Broader {
			g.link("narrower", resolve(b), c.ID)
		}
		for _, n := range c.Narrower {
			g.link("narrower", c.ID, resolve(n))
		}
	}

	pair := func(a, b string) [2]string {
		if a < b {
			return [2]string{a, b}
		}
		return [2]string{b, a}
	}
	nonExact := map[[2]string]bool{}
	for _, m := range mappings {
		if len(m.To) == 1 && m.Relation != "exactMatch" && m.Relation != "closeMatch" {
			nonExact[pair(m.From, m.To[0])] = true
		}
	}
	for _, m := range mappings {
		if len(m.To) != 1 {
			continue
		}
		a, b := m.From, m.To[0]
		switch m.Relation {
		case "exactMatch":
			if !nonExact[pair(a, b)] {
				g.both("exact", a, b)
			}
		case "closeMatch":
			g.both("close", a, b)
		case "relatedMatch":
			g.both("related", a, b)
		case "broadMatch": // b is broader than a
			g.link("narrower", b, a)
		case "narrowMatch": // b is narrower than a
			g.link("narrower", a, b)
		}
	}
	return g
}

func (g *Graph) exactClosure(start string) []string {
	out := []string{start}
	seen := map[string]bool{start: true}
	for i := 0; i < len(out); i++ {
		for _, n := range g.exact[out[i]] {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// Expand returns id followed by what a filter on id should also match:
//  1. E: the exact closure of id (exactMatch is transitive)
//  2. the direct closeMatch neighbours of E (one hop, not chained)
//  3. everything narrower than those, transitively up to MaxDepth, each newly
//     reached concept with its exact closure
//  4. if related: the direct relatedMatch neighbours of E
//
// Broader concepts are never added. At most MaxResults identifiers, in that
// order; truncated reports whether the cap cut the result.
func (g *Graph) Expand(id string, related bool) (ids []string, truncated bool) {
	seen := map[string]bool{}
	add := func(x string) bool {
		if seen[x] {
			return true
		}
		if len(ids) >= MaxResults {
			truncated = true
			return false
		}
		seen[x] = true
		ids = append(ids, x)
		return true
	}

	exact := g.exactClosure(id)
	for _, x := range exact {
		if !add(x) {
			return
		}
	}
	frontier := append([]string{}, exact...)
	for _, x := range exact {
		for _, n := range g.close[x] {
			if !seen[n] {
				if !add(n) {
					return
				}
				frontier = append(frontier, n)
			}
		}
	}
	for depth := 0; depth < MaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, x := range frontier {
			for _, child := range g.narrower[x] {
				if seen[child] {
					continue
				}
				for _, y := range g.exactClosure(child) {
					if seen[y] {
						continue
					}
					if !add(y) {
						return
					}
					next = append(next, y)
				}
			}
		}
		frontier = next
	}
	if related {
		for _, x := range exact {
			for _, n := range g.related[x] {
				if !add(n) {
					return
				}
			}
		}
	}
	return
}
