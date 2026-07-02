package main

import "sort"

// Node is one entity in the relationship graph. Weight drives node size.
type Node struct {
	ID     string `json:"id"`
	Type   string `json:"type"` // community|author|project|measure|publication
	Label  string `json:"label"`
	Weight int    `json:"weight"`
}

// Edge connects two nodes. Weight drives line thickness.
type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Weight int    `json:"weight"`
	Kind   string `json:"kind"` // author_community|part_of|author_project|community_project
}

// Graph is the aggregated node/edge payload returned by /viz/graph.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// AuthorStat is one author (publisher pubkey) and how many resources it has.
type AuthorStat struct {
	Pubkey string
	Label  string
	Count  int
}

// CommunityStat is one community and how much content is shared into it.
type CommunityStat struct {
	Pubkey  string
	Label   string
	Content int
}

// AuthorCommunity records that an author contributes content to a community.
type AuthorCommunity struct {
	Author    string
	Community string
	Weight    int
}

// TKItem is one transferkiosk entity (project/measure/publication). ParentCoord
// is the partOf target ("" for a project). Author/Community are only set for
// projects (used to wire author_project / community_project edges).
type TKItem struct {
	Coord       string
	Kind        int
	Label       string
	Author      string
	ParentCoord string
	Community   string
}

// GraphInput bundles the aggregated facts buildGraph turns into a graph.
type GraphInput struct {
	Authors           []AuthorStat
	Communities       []CommunityStat
	AuthorCommunities []AuthorCommunity
	TK                []TKItem
	TopAuthors        int
}

func tkNodeType(kind int) string {
	switch kind {
	case 30143:
		return "project"
	case 30144:
		return "measure"
	case 30145:
		return "publication"
	default:
		return "publication"
	}
}

// buildGraph aggregates entities into nodes+edges. Authors beyond TopAuthors
// (ranked by Count desc) fold into a single author:others node. Content is
// never a node — only weight/thickness.
func buildGraph(in GraphInput) Graph {
	g := Graph{Nodes: []Node{}, Edges: []Edge{}}
	kept := map[string]bool{}

	// Communities — all kept.
	for _, c := range in.Communities {
		g.Nodes = append(g.Nodes, Node{ID: "community:" + c.Pubkey, Type: "community", Label: c.Label, Weight: c.Content})
	}

	// Authors — top-N by count, rest folded into others.
	authors := append([]AuthorStat(nil), in.Authors...)
	sort.SliceStable(authors, func(i, j int) bool { return authors[i].Count > authors[j].Count })
	othersWeight := 0
	for i, a := range authors {
		if in.TopAuthors > 0 && i >= in.TopAuthors {
			othersWeight += a.Count
			continue
		}
		g.Nodes = append(g.Nodes, Node{ID: "author:" + a.Pubkey, Type: "author", Label: a.Label, Weight: a.Count})
		kept["author:"+a.Pubkey] = true
	}
	if othersWeight > 0 {
		g.Nodes = append(g.Nodes, Node{ID: "author:others", Type: "author", Label: "other contributors", Weight: othersWeight})
	}

	// Author→community edges (only for kept author nodes).
	for _, ac := range in.AuthorCommunities {
		src := "author:" + ac.Author
		if !kept[src] {
			continue
		}
		g.Edges = append(g.Edges, Edge{Source: src, Target: "community:" + ac.Community, Weight: ac.Weight, Kind: "author_community"})
	}

	// Transferkiosk nodes + edges.
	for _, t := range in.TK {
		g.Nodes = append(g.Nodes, Node{ID: "tk:" + t.Coord, Type: tkNodeType(t.Kind), Label: t.Label, Weight: 1})
	}
	for _, t := range in.TK {
		if t.ParentCoord != "" {
			g.Edges = append(g.Edges, Edge{Source: "tk:" + t.Coord, Target: "tk:" + t.ParentCoord, Weight: 1, Kind: "part_of"})
		}
		if t.Kind == 30143 {
			if t.Author != "" && kept["author:"+t.Author] {
				g.Edges = append(g.Edges, Edge{Source: "author:" + t.Author, Target: "tk:" + t.Coord, Weight: 1, Kind: "author_project"})
			}
			if t.Community != "" {
				g.Edges = append(g.Edges, Edge{Source: "tk:" + t.Coord, Target: "community:" + t.Community, Weight: 1, Kind: "community_project"})
			}
		}
	}

	// Project weight = number of children (measures + publications).
	childCount := map[string]int{}
	for _, t := range in.TK {
		if t.ParentCoord != "" {
			childCount["tk:"+t.ParentCoord]++
		}
	}
	for i := range g.Nodes {
		if g.Nodes[i].Type == "project" {
			if c := childCount[g.Nodes[i].ID]; c > 0 {
				g.Nodes[i].Weight = c
			}
		}
	}

	return g
}
