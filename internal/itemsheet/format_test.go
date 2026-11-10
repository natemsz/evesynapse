package itemsheet

import "testing"

// A wrong conversion shows a wrong stat with nothing to flag it, so
// each of the game's encodings is pinned to the figure the game shows.
func TestFormatValueShowsTheGamesFigures(t *testing.T) {
	cases := []struct {
		unit int64
		name string
		v    float64
		want string
	}{
		{unitInversePercent, "%", 0.4, "60%"},      // resonance 0.4 is 60% resistance
		{unitInverseModifier, "%", 0.895, "10.5%"}, // 0.895 is 10.5% faster
		{unitModifierPercent, "%", 1.1, "+10%"},
		{unitModifierPercent, "%", 0.85, "-15%"},
		{unitMilliseconds, "s", 4500, "4.5 s"},
		{unitMeter, "m", 2500, "2,500 m"},
		{unitMeter, "m", 24000, "24 km"},
		{unitMeter, "m", 2147483647000, "14.36 AU"},
		{unitMultiplier, "x", 1.25, "1.25x"},
		{unitPercent, "%", -10, "-10%"},
		{unitAbsolutePercent, "%", 0.05, "5%"},
		{unitSizeClass, "", 2, "Medium"},
		{106, "tf", 12.5, "12.5 tf"},
		{0, "", 0.075, "0.075"},
	}
	for _, c := range cases {
		if got := FormatValue(c.unit, c.name, c.v); got != c.want {
			t.Errorf("FormatValue(%d, %v) = %q, want %q", c.unit, c.v, got, c.want)
		}
	}
}
