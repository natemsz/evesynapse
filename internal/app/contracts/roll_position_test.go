package contracts

import "testing"

// TestAbyssalPositionText pins the roll-position wording
// across the median boundary: above the median the label
// is the elite share above the roll, at or below it the
// label is the share at or below it -- a below-median
// roll must never read as "Top".
func TestAbyssalPositionText(t *testing.T) {
	cases := []struct {
		name string
		p    float64
		want string
	}{
		{"perfect roll", 1.0, "Top 0%"},
		{"99th percentile", 0.99, "Top 1%"},
		{"80th percentile", 0.80, "Top 20%"},
		{"75th percentile", 0.75, "Top 25%"},
		{"just above median", 0.51, "Top 49%"},
		{"exact median", 0.50, "Bottom 50%"},
		{"30th percentile", 0.30, "Bottom 30%"},
		{"5th percentile", 0.05, "Bottom 5%"},
		{"worst roll", 0.0, "Bottom 0%"},
	}
	for _, c := range cases {
		if got := abyssalPositionText(c.p); got != c.want {
			t.Errorf("%s: abyssalPositionText(%v) = %q, want %q", c.name, c.p, got, c.want)
		}
	}
}
