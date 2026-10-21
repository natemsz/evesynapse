package scan

import (
	"math"
	"testing"
)

func TestDetectAndParse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind Kind
		rows int
	}{
		{"dscan", "11176\tCrow\tCrow\t1,234 km\r\n35832\tSome Astrahus\tAstrahus\t-\n40340\tJita IV - Moon 4\tMoon\t3.2 AU\n", KindDScan, 3},
		{"local", "Some Pilot\nAnother Pilot\nsome pilot\nO'Neil-X\n", KindLocal, 3},
		{"fleet", "Pilot A\tJita\tLoki\tStrategic Cruiser\tSquad Member\t0 - 0 - 5\tWing 1 / Squad 1\nPilot B\tJita\tGuardian\tLogistics\tSquad Commander\t0 - 0 - 5\tWing 1 / Squad 1\n", KindFleet, 2},
		{"probe", "ABC-123\tCosmic Signature\tWormhole\tUnstable Wormhole\t100.0%\t4.21 AU\nXYZ-987\tCosmic Signature\t\t\t0.0%\t12.10 AU\n", KindProbe, 2},
	}
	for _, c := range cases {
		r, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if r.Kind != c.kind {
			t.Fatalf("%s: kind %q, want %q", c.name, r.Kind, c.kind)
		}
		n := len(r.DScan) + len(r.Local) + len(r.Fleet) + len(r.Probe)
		if n != c.rows {
			t.Errorf("%s: %d rows, want %d", c.name, n, c.rows)
		}
	}
	if _, err := Parse("  \n\n"); err != ErrEmpty {
		t.Errorf("empty: %v", err)
	}
	if _, err := Parse("a\tb\nc\td\n"); err != ErrUnknown {
		t.Errorf("unknown: %v", err)
	}
}

func TestParseDistance(t *testing.T) {
	for in, want := range map[string]float64{
		"1,234 km":   1234,
		"1 234 km":   1234,
		"1 234 km":   1234,
		"1.234 km":   1234,
		"950 m":      0.95,
		"3.2 AU":     3.2 * AUKm,
		"3,2 AU":     3.2 * AUKm,
		"1,234.5 AU": 1234.5 * AUKm,
		"-":          -1,
		"":           -1,
		"far":        -1,
	} {
		if got := ParseDistance(in); math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Errorf("ParseDistance(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSystemFromCelestial(t *testing.T) {
	for in, want := range map[string]string{
		"Jita IV - Moon 4":     "Jita",
		"Jita IV":              "Jita",
		"Jita - Star":          "Jita",
		"1DQ1-A VIII - Moon 2": "1DQ1-A",
		"Old Man Star IX":      "Old Man Star",
		"Jita IV - Moon 4 - Caldari Navy Assembly Plant": "Jita",
		"Stargate (Perimeter)":                           "",
		"Crow":                                           "",
	} {
		if got := SystemFromCelestial(in); got != want {
			t.Errorf("SystemFromCelestial(%q) = %q, want %q", in, got, want)
		}
	}
}
