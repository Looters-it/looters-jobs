package turnclose

import (
	"maps"
	"testing"
)

func shares(weights ...int) []Share {
	ids := []string{"a", "b", "c", "d"}
	out := make([]Share, len(weights))
	for i, weight := range weights {
		out[i] = Share{TerritoryID: ids[i], Weight: weight}
	}
	return out
}

func TestIncome(t *testing.T) {
	// Defaults: 50 plus 10 for each municipality.
	if got := Income(50, 10, 1); got != 60 {
		t.Errorf("one municipality earns %d, want 60", got)
	}
	if got := Income(50, 10, 12); got != 170 {
		t.Errorf("twelve municipalities earn %d, want 170", got)
	}
}

func TestDistribute(t *testing.T) {
	cases := map[string]struct {
		income  int
		mode    string
		shares  []Share
		capital string
		want    map[string]int
	}{
		"equal parts":                 {90, ModeEqual, shares(1, 1, 1), "b", map[string]int{"a": 30, "b": 30, "c": 30}},
		"remainder to the capital":    {100, ModeEqual, shares(1, 1, 1), "b", map[string]int{"a": 33, "b": 34, "c": 33}},
		"equal ignores weights":       {100, ModeEqual, shares(9, 1), "a", map[string]int{"a": 50, "b": 50}},
		"by weight":                   {100, ModeWeighted, shares(3, 1), "a", map[string]int{"a": 75, "b": 25}},
		"weighted remainder":          {100, ModeWeighted, shares(1, 1, 1), "c", map[string]int{"a": 33, "b": 33, "c": 34}},
		"a zero weight earns nothing": {100, ModeWeighted, shares(0, 4), "b", map[string]int{"a": 0, "b": 100}},
		"all weights zero":            {100, ModeWeighted, shares(0, 0), "a", map[string]int{"a": 50, "b": 50}},
		"capital not among them":      {10, ModeEqual, shares(1, 1, 1), "elsewhere", map[string]int{"a": 4, "b": 3, "c": 3}},
		"a single territory":          {60, ModeEqual, shares(1), "a", map[string]int{"a": 60}},
		"no territories":              {60, ModeEqual, nil, "a", map[string]int{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Distribute(tc.income, tc.mode, tc.shares, tc.capital)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			total := 0
			for _, amount := range got {
				total += amount
			}
			if len(tc.shares) > 0 && total != tc.income {
				t.Fatalf("distributed %d of %d", total, tc.income)
			}
		})
	}
}
