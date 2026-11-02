package appraise

import "testing"

// Order prices carry at most four significant figures, so the
// price one tick under a station best depends on the best's
// size. These cases pin the grid, including the decade
// boundaries and a legacy off-grid best.
func TestTickDown(t *testing.T) {
	cases := []struct {
		best  float64
		under float64
		ok    bool
	}{
		{6.40, 6.39, true},
		{45.60, 45.59, true},
		{123.40, 123.30, true},
		{999.90, 999.80, true},
		{100.00, 99.99, true},
		{10000.00, 9999.00, true},
		{1000000.00, 999900.00, true},
		{1112000.00, 1111000.00, true},
		{1905944.00, 1905000.00, true},
		{0.02, 0.01, true},
		{0.01, 0, false},
	}
	for _, c := range cases {
		under, ok := tickDown(c.best)
		if ok != c.ok || (ok && under != c.under) {
			t.Errorf("tickDown(%v) = %v, %v; want %v, %v", c.best, under, ok, c.under, c.ok)
		}
	}
}
