package main

// TypeCount is one content kind and its document count.
type TypeCount struct {
	Kind  int    `json:"kind"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// SubjectCount is one subject facet bucket.
type SubjectCount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Totals are the stat-strip headline numbers.
type Totals struct {
	Resources    int `json:"resources"`
	ContentTypes int `json:"content_types"`
	Authors      int `json:"authors"`
	Communities  int `json:"communities"`
	Projects     int `json:"projects"`
}

// Stats is the /viz/stats response payload.
type Stats struct {
	Totals   Totals         `json:"totals"`
	ByType   []TypeCount    `json:"by_type"`
	Subjects []SubjectCount `json:"subjects"`
}

// StatsInput bundles the raw facts buildStats turns into the Stats payload.
type StatsInput struct {
	TypeCounts  []TypeCount
	Subjects    []FacetValue
	Authors     int
	Communities int
	Projects    int
}

// buildStats assembles the stat payload. Resources = sum of all type counts;
// ContentTypes = number of kinds with >=1 doc. Subjects fold FacetValue into
// the API shape (Value is used for both id and label — the AMB `about` facet
// stores the human label; a future taxonomy join can split them).
func buildStats(in StatsInput) Stats {
	resources := 0
	types := 0
	for _, tc := range in.TypeCounts {
		resources += tc.Count
		if tc.Count > 0 {
			types++
		}
	}
	subjects := make([]SubjectCount, 0, len(in.Subjects))
	for _, f := range in.Subjects {
		subjects = append(subjects, SubjectCount{ID: f.Value, Label: f.Value, Count: f.Count})
	}
	return Stats{
		Totals: Totals{
			Resources:    resources,
			ContentTypes: types,
			Authors:      in.Authors,
			Communities:  in.Communities,
			Projects:     in.Projects,
		},
		ByType:   in.TypeCounts,
		Subjects: subjects,
	}
}
