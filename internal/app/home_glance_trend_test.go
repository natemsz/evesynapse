package app

import (
	"testing"
	"time"
)

// TestGlanceTrends: the baseline is the newest sample at least a week
// old, else the oldest at least a day old; the arrow points the way the
// figure moved and is left out when nothing moved or there is no
// baseline.
func TestGlanceTrends(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	day := func(n int, isk, assets float64) netWorthDay {
		return netWorthDay{At: now.AddDate(0, 0, -n).Truncate(24 * time.Hour), ISK: isk, Assets: assets}
	}
	days := []netWorthDay{day(10, 50, 50), day(8, 80, 120), day(3, 90, 90), day(0, 100, 100)}
	base, ok := trendBaseline(days, now)
	if !ok || base.Total() != 200 {
		t.Fatalf("baseline %+v (%v); want the 8-day-old sample", base, ok)
	}
	if tr := newKPITrend(220, base.Total(), trendSpan(base.At, now)); tr == nil || !tr.Up || tr.Delta != "+20 (+10.0%)" || tr.Text != "+20 ISK (+10.0%) over 8 days" {
		t.Fatalf("up trend: %+v", tr)
	}
	if tr := newKPITrend(150, 200, "8 days"); tr == nil || tr.Up {
		t.Fatalf("down trend: %+v", tr)
	}
	if tr := newKPITrend(200.05, 200, "8 days"); tr != nil {
		t.Fatalf("a change that rounds to nothing still shows: %+v", tr)
	}
	if tr := newKPITrend(100, 0, "8 days"); tr != nil {
		t.Fatalf("a zero baseline still shows: %+v", tr)
	}
	// A young history: the oldest sample a day or more back.
	if base, ok := trendBaseline([]netWorthDay{day(2, 1, 1), day(0, 2, 2)}, now); !ok || base.ISK != 1 {
		t.Fatalf("young history baseline %+v (%v)", base, ok)
	}
	// Only today's sample: no baseline.
	if _, ok := trendBaseline([]netWorthDay{day(0, 2, 2)}, now); ok {
		t.Fatal("a same-day sample was used as a baseline")
	}
}
