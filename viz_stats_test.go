package main

import "testing"

func TestBuildStats(t *testing.T) {
	in := StatsInput{
		TypeCounts: []TypeCount{
			{Kind: 30142, Label: "AMB resource", Count: 100},
			{Kind: 30023, Label: "Long-form", Count: 20},
			{Kind: 30818, Label: "Wiki", Count: 0},
		},
		Subjects:    []FacetValue{{Value: "Chemie", Count: 40}, {Value: "Physik", Count: 10}},
		Authors:     7,
		Communities: 3,
		Projects:    5,
	}
	s := buildStats(in)
	if s.Totals.Resources != 120 {
		t.Errorf("resources = %d, want 120", s.Totals.Resources)
	}
	if s.Totals.ContentTypes != 2 {
		t.Errorf("content_types = %d, want 2 (kinds with >=1 doc)", s.Totals.ContentTypes)
	}
	if s.Totals.Authors != 7 || s.Totals.Communities != 3 || s.Totals.Projects != 5 {
		t.Errorf("totals wrong: %+v", s.Totals)
	}
	if len(s.Subjects) != 2 || s.Subjects[0].Label != "Chemie" || s.Subjects[0].Count != 40 {
		t.Errorf("subjects wrong: %+v", s.Subjects)
	}
}
