package main

import "testing"

func nodeByID(g Graph, id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func hasEdge(g Graph, src, dst, kind string) bool {
	for _, e := range g.Edges {
		if e.Source == src && e.Target == dst && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestBuildGraph_TopAuthorFold(t *testing.T) {
	in := GraphInput{
		TopAuthors: 2,
		Authors: []AuthorStat{
			{Pubkey: "a1", Label: "A1", Count: 100},
			{Pubkey: "a2", Label: "A2", Count: 50},
			{Pubkey: "a3", Label: "A3", Count: 10},
			{Pubkey: "a4", Label: "A4", Count: 5},
		},
	}
	g := buildGraph(in)
	if _, ok := nodeByID(g, "author:a1"); !ok {
		t.Errorf("top author a1 missing")
	}
	if _, ok := nodeByID(g, "author:a3"); ok {
		t.Errorf("a3 should have been folded into others")
	}
	others, ok := nodeByID(g, "author:others")
	if !ok {
		t.Fatalf("others node missing")
	}
	if others.Weight != 15 {
		t.Errorf("others weight = %d, want 15", others.Weight)
	}
}

func TestBuildGraph_CommunityEdgesAndTree(t *testing.T) {
	in := GraphInput{
		TopAuthors: 10,
		Authors:    []AuthorStat{{Pubkey: "a1", Label: "A1", Count: 10}},
		Communities: []CommunityStat{{Pubkey: "c1", Label: "C1", Content: 5}},
		AuthorCommunities: []AuthorCommunity{{Author: "a1", Community: "c1", Weight: 3}},
		TK: []TKItem{
			{Coord: "30143:p:proj", Kind: 30143, Label: "Proj", Author: "a1", Community: "c1"},
			{Coord: "30144:p:m1", Kind: 30144, Label: "M1", ParentCoord: "30143:p:proj"},
			{Coord: "30145:p:pub", Kind: 30145, Label: "Pub", ParentCoord: "30144:p:m1"},
		},
	}
	g := buildGraph(in)
	if n, ok := nodeByID(g, "community:c1"); !ok || n.Type != "community" || n.Weight != 5 {
		t.Errorf("community node wrong: %+v ok=%v", n, ok)
	}
	if !hasEdge(g, "author:a1", "community:c1", "author_community") {
		t.Errorf("author_community edge missing")
	}
	if n, ok := nodeByID(g, "tk:30143:p:proj"); !ok || n.Type != "project" {
		t.Errorf("project node missing/wrong: %+v", n)
	}
	if _, ok := nodeByID(g, "tk:30144:p:m1"); !ok {
		t.Errorf("measure node missing")
	}
	if !hasEdge(g, "tk:30144:p:m1", "tk:30143:p:proj", "part_of") {
		t.Errorf("measure->project part_of edge missing")
	}
	if !hasEdge(g, "tk:30145:p:pub", "tk:30144:p:m1", "part_of") {
		t.Errorf("pub->measure part_of edge missing")
	}
	if !hasEdge(g, "author:a1", "tk:30143:p:proj", "author_project") {
		t.Errorf("author_project edge missing")
	}
	if !hasEdge(g, "tk:30143:p:proj", "community:c1", "community_project") {
		t.Errorf("community_project edge missing")
	}
}
