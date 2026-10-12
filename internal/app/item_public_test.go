package app

import (
	"strings"
	"testing"
)

// The public item and ship pages draw their history server-side:
// no script, crawlable, and honest about gaps.
func TestSparkline(t *testing.T) {
	if got := sparkline(nil); got != "" {
		t.Errorf("empty sparkline = %q, want empty", got)
	}
	two := string(sparkline([]float64{100, 120}))
	if !strings.Contains(two, "<svg") || !strings.Contains(two, "polyline") {
		t.Errorf("two-point sparkline missing svg polyline: %q", two)
	}
	if c := strings.Count(two, ","); c != 2 {
		t.Errorf("two-point sparkline plots %d coordinates, want 2", c)
	}
	flat := string(sparkline([]float64{50, 50, 50}))
	if !strings.Contains(flat, "<svg") {
		t.Errorf("flat sparkline missing svg: %q", flat)
	}
	long := make([]float64, 200)
	for i := range long {
		long[i] = float64(i)
	}
	if c := strings.Count(string(sparkline(long)), ","); c != 90 {
		t.Errorf("long sparkline plots %d coordinates, want capped 90", c)
	}
}
