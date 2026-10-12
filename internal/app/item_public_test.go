package app

import (
	"strconv"
	"strings"
	"testing"
)

// The public item and ship pages draw their history server-side:
// no script, crawlable, and honest about gaps.
func sparkPoints(t *testing.T, svg string) [][2]float64 {
	t.Helper()
	start := strings.Index(svg, `points="`)
	if start < 0 {
		t.Fatalf("sparkline has no points attribute: %q", svg)
	}
	rest := svg[start+len(`points="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("sparkline points attribute never closes: %q", svg)
	}
	var pts [][2]float64
	for _, pair := range strings.Split(rest[:end], " ") {
		xy := strings.Split(pair, ",")
		if len(xy) != 2 {
			t.Fatalf("sparkline pair %q is not x,y", pair)
		}
		x, err := strconv.ParseFloat(xy[0], 64)
		if err != nil {
			t.Fatalf("sparkline x %q: %v", xy[0], err)
		}
		y, err := strconv.ParseFloat(xy[1], 64)
		if err != nil {
			t.Fatalf("sparkline y %q: %v", xy[1], err)
		}
		pts = append(pts, [2]float64{x, y})
	}
	return pts
}

func TestSparkline(t *testing.T) {
	if got := sparkline(nil); got != "" {
		t.Errorf("empty sparkline = %q, want empty", got)
	}
	// Rising pair: first point at the left floor, last at the
	// right ceiling of the 600x120 box with 8px padding.
	two := sparkPoints(t, string(sparkline([]float64{100, 120})))
	if len(two) != 2 {
		t.Fatalf("two-point sparkline plots %d pairs, want 2", len(two))
	}
	if two[0] != [2]float64{0, 112} || two[1] != [2]float64{600, 8} {
		t.Errorf("two-point sparkline = %v, want [{0 112} {600 8}]", two)
	}
	// Flat series ride the 60px midline across the full width.
	flat := sparkPoints(t, string(sparkline([]float64{50, 50, 50})))
	if len(flat) != 3 {
		t.Fatalf("flat sparkline plots %d pairs, want 3", len(flat))
	}
	for i, p := range flat {
		if p[1] != 60 {
			t.Errorf("flat sparkline point %d y = %v, want 60", i, p[1])
		}
	}
	if flat[0][0] != 0 || flat[2][0] != 600 {
		t.Errorf("flat sparkline spans %v to %v, want 0 to 600", flat[0][0], flat[2][0])
	}
	// Long series cap at the newest 90 points.
	long := make([]float64, 200)
	for i := range long {
		long[i] = float64(i)
	}
	if pts := sparkPoints(t, string(sparkline(long))); len(pts) != 90 {
		t.Errorf("long sparkline plots %d pairs, want capped 90", len(pts))
	}
}
