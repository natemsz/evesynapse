package humanize

import "testing"

// TestISKCompact pins the one compact ISK form: two decimals with the
// unit, whole numbers under 1,000, a figure that rounds up to a whole
// unit taking that unit, and the sign kept.
func TestISKCompact(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want string
	}{
		{0, "0"}, {940.4, "940"}, {999.4, "999"},
		{1000, "1.00K"}, {940_000, "940.00K"}, {999_996, "1.00M"},
		{312_400_000, "312.40M"}, {1_274_000_000, "1.27B"}, {4_210_000_000_000, "4.21T"},
		{-1_740_000_000, "-1.74B"}, {-0.2, "0"},
	} {
		if got := ISKCompact(c.v); got != c.want {
			t.Errorf("ISKCompact(%v) = %q, want %q", c.v, got, c.want)
		}
	}
	for _, c := range []struct {
		v    float64
		want string
	}{{820_500_000, "+820.50M"}, {-1_740_000_000, "-1.74B"}, {0, "0"}} {
		if got := ISKCompactSigned(c.v); got != c.want {
			t.Errorf("ISKCompactSigned(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}
