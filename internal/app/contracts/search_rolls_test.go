package contracts

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// TestParseRollBands pins the roll sliders' parameter
// contract: rollmin_<id> / rollmax_<id> percentiles are
// read as 0..1 fractions, clamped to the percentile range,
// and garbage (bad id, bad value, unknown key shape) drops
// that one bound instead of failing the search.
func TestParseRollBands(t *testing.T) {
	cases := []struct {
		name string
		q    url.Values
		min  map[int64]float64
		max  map[int64]float64
	}{
		{"unset", url.Values{}, map[int64]float64{}, map[int64]float64{}},
		{"min only", url.Values{"rollmin_64": {"50"}},
			map[int64]float64{64: 0.5}, map[int64]float64{}},
		{"max only", url.Values{"rollmax_64": {"25"}},
			map[int64]float64{}, map[int64]float64{64: 0.25}},
		{"both sides, two attributes", url.Values{
			"rollmin_64": {"50"}, "rollmax_64": {"90"}, "rollmin_6": {"10"}},
			map[int64]float64{64: 0.5, 6: 0.1}, map[int64]float64{64: 0.9}},
		{"clamped above", url.Values{"rollmin_64": {"250"}},
			map[int64]float64{64: 1}, map[int64]float64{}},
		{"clamped below", url.Values{"rollmax_64": {"-5"}},
			map[int64]float64{}, map[int64]float64{64: 0}},
		{"empty value is unset", url.Values{"rollmin_64": {""}},
			map[int64]float64{}, map[int64]float64{}},
		{"garbage value ignored", url.Values{"rollmin_64": {"banana"}},
			map[int64]float64{}, map[int64]float64{}},
		{"nonfinite value ignored", url.Values{"rollmin_64": {"NaN"}, "rollmax_64": {"+Inf"}},
			map[int64]float64{}, map[int64]float64{}},
		{"garbage id ignored", url.Values{"rollmin_x": {"50"}, "rollmax_6x": {"50"}},
			map[int64]float64{}, map[int64]float64{}},
		{"zero and negative ids ignored", url.Values{"rollmin_0": {"50"}, "rollmin_-3": {"50"}},
			map[int64]float64{}, map[int64]float64{}},
	}
	for _, c := range cases {
		mins, maxs := parseRollBands(c.q)
		if len(mins) != len(c.min) || len(maxs) != len(c.max) {
			t.Errorf("%s: got mins %v maxs %v, want mins %v maxs %v", c.name, mins, maxs, c.min, c.max)
			continue
		}
		for id, want := range c.min {
			if mins[id] != want {
				t.Errorf("%s: min[%d] = %v, want %v", c.name, id, mins[id], want)
			}
		}
		for id, want := range c.max {
			if maxs[id] != want {
				t.Errorf("%s: max[%d] = %v, want %v", c.name, id, maxs[id], want)
			}
		}
	}
}

// TestSearchFiltersRollBands pins how the parsed bounds
// reach the queries: AbActive follows the bands, and the
// JSONB argument carries each band as [min, max] with an
// unset side at the range end and crossed bounds swapped
// into order.
func TestSearchFiltersRollBands(t *testing.T) {
	f := parseSearchFilters(url.Values{"rollmin_64": {"50"}})
	if !f.AbActive {
		t.Error("a roll band should activate the abyssal filters")
	}
	var bands map[string][2]float64
	if err := json.Unmarshal(f.rollBandsJSON(), &bands); err != nil {
		t.Fatalf("bands JSON: %v", err)
	}
	if got := bands["64"]; got != [2]float64{0.5, 1} {
		t.Errorf("min-only band = %v, want [0.5 1]", got)
	}

	f = parseSearchFilters(url.Values{"rollmin_64": {"90"}, "rollmax_64": {"50"}})
	if err := json.Unmarshal(f.rollBandsJSON(), &bands); err != nil {
		t.Fatalf("bands JSON: %v", err)
	}
	if got := bands["64"]; got != [2]float64{0.5, 0.9} {
		t.Errorf("crossed band = %v, want [0.5 0.9] (swapped into order)", got)
	}

	f = parseSearchFilters(url.Values{})
	if f.AbActive {
		t.Error("no filters should leave the abyssal filters inactive")
	}
	if got := string(f.rollBandsJSON()); got != "{}" {
		t.Errorf("unset bands JSON = %q, want {}", got)
	}
	sp, cp := f.params(time.Now().UTC())
	if string(sp.AbRollBands) != "{}" || string(cp.AbRollBands) != "{}" {
		t.Error("search and count params should share the empty bands object")
	}
}
