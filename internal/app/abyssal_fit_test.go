package app

import (
	"database/sql"
	"math"
	"testing"
	"time"
)

// TestAbyssalAttributedAsk: a contract's price is an item's
// own ask only when the item is all it offers; small priced
// passengers are subtracted; bundles of abyssals, exchanges
// and passenger-priced bundles are refused; unstamped legacy
// sightings speak quietly.
func TestAbyssalAttributedAsk(t *testing.T) {
	cases := []struct {
		name   string
		in     abyssalAttribution
		ok     bool
		ask, w float64
	}{
		{"alone", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 1}, true, 100, 1},
		{"small passenger", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 1, OtherLines: 2, OtherValue: 10}, true, 90, 0.9},
		{"big passenger", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 1, OtherLines: 1, OtherValue: 40}, false, 0, 0},
		{"unpriced passenger", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 1, OtherLines: 1, OtherUnpriced: 1}, false, 0, 0},
		{"two abyssals", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 2}, false, 0, 0},
		{"exchange", abyssalAttribution{Price: 100, Stamped: true, AbyssalLines: 1, WantedLines: 1}, false, 0, 0},
		{"legacy", abyssalAttribution{Price: 100}, true, 100, abyssalUnstampedWeight},
	}
	for _, c := range cases {
		ask, w, ok := abyssalAttributedAsk(c.in)
		if ok != c.ok || math.Abs(ask-c.ask) > 1e-9 || math.Abs(w-c.w) > 1e-9 {
			t.Errorf("%s: got (%v, %v, %v), want (%v, %v, %v)", c.name, ask, w, ok, c.ask, c.w, c.ok)
		}
	}
}

// TestAbyssalMarketCalibration: listings only nudge. The factor
// is shrunk toward none until enough clean listings speak, can
// never pass its bounds however extreme the asks, and a few
// bait or joke asks cannot drag its median.
func TestAbyssalMarketCalibration(t *testing.T) {
	comps := func(n int, ratio float64) []abyssalComp {
		out := make([]abyssalComp, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, abyssalComp{ItemID: int64(i + 1), Ask: 100e6 * ratio, LadderValue: 100e6, DynamicsWeight: 1})
		}
		return out
	}
	if f, _, _ := abyssalMarketCalibration(nil); f != 1 {
		t.Errorf("no listings: factor %v, want exactly 1", f)
	}
	few, _, _ := abyssalMarketCalibration(comps(3, 1.2))
	many, _, _ := abyssalMarketCalibration(comps(200, 1.2))
	if !(few > 1 && few < many && many <= 1.2+1e-9) {
		t.Errorf("shrinkage: 3 listings %v, 200 listings %v; want 1 < few < many <= 1.2", few, many)
	}
	if f, _, _ := abyssalMarketCalibration(comps(500, 10)); f > abyssalCalibMax+1e-9 {
		t.Errorf("asks ten times the ladder moved the factor to %v, past its %v ceiling", f, abyssalCalibMax)
	}
	if f, _, _ := abyssalMarketCalibration(comps(500, 0.01)); f < abyssalCalibMin-1e-9 {
		t.Errorf("dumped asks moved the factor to %v, under its %v floor", f, abyssalCalibMin)
	}
	clean := comps(60, 1.0)
	baited := append(append([]abyssalComp(nil), clean...), comps(8, 1000)...)
	baited = append(baited, comps(8, 0.001)...)
	if f, _, _ := abyssalMarketCalibration(baited); math.Abs(f-1) > 0.01 {
		t.Errorf("bait and joke asks moved the factor to %v", f)
	}
}

// TestAbyssalRecreationCaps: nobody needs to pay more than
// rolling another; the ceiling holds the figure and the range.
func TestAbyssalRecreationCaps(t *testing.T) {
	rungs := []abyssalRung{{TypeID: 1, Price: 100e6, Index: 0.3}, {TypeID: 2, Price: 2e9, Index: 0.9}}
	free := estimateAbyssal(abyssalEstimateInput{Index: 0.85, HasIndex: true, Rungs: rungs})
	capped := estimateAbyssal(abyssalEstimateInput{Index: 0.85, HasIndex: true, Rungs: rungs, Recreation: 400e6})
	if !(free.Value > 400e6) {
		t.Fatalf("fixture: the uncapped figure %v should sit above the cap", free.Value)
	}
	if capped.Value != 400e6 || capped.High > 400e6 || capped.Low > capped.Value {
		t.Errorf("capped estimate %+v, want value and high held at 400m", capped)
	}
}

// TestAbyssalLadderRange: the range is tight beside a rung and
// widest midway across a big price gap.
func TestAbyssalLadderRange(t *testing.T) {
	rungs := []abyssalRung{{TypeID: 1, Price: 100e6, Index: 0.3}, {TypeID: 2, Price: 2e9, Index: 0.9}}
	width := func(idx float64) float64 {
		est := estimateAbyssal(abyssalEstimateInput{Index: idx, HasIndex: true, Rungs: rungs})
		return math.Log(est.High / est.Low)
	}
	if !(width(0.31) < width(0.6)) {
		t.Errorf("range beside a rung %.3f should be tighter than mid-gap %.3f", width(0.31), width(0.6))
	}
}

// TestAbyssalAgeWeight: recency decays toward a floor, and a
// listing that sits live loses say while one that left keeps
// its exit dynamics to itself.
func TestAbyssalAgeWeight(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	gone := func(d time.Duration) sql.NullTime { return sql.NullTime{Time: now.Add(-d), Valid: true} }
	day := 24 * time.Hour
	fresh := abyssalAgeWeight(gone(0), now.Add(-2*day), now, now)
	month := abyssalAgeWeight(gone(30*day), now.Add(-32*day), now.Add(-30*day), now)
	ancient := abyssalAgeWeight(gone(400*day), now.Add(-402*day), now.Add(-400*day), now)
	if math.Abs(fresh-1) > 1e-9 || math.Abs(month-0.5) > 1e-6 || ancient != 0.1 {
		t.Errorf("recency: fresh %v, a month %v, ancient %v; want 1, 0.5, 0.1", fresh, month, ancient)
	}
	liveNew := abyssalAgeWeight(sql.NullTime{}, now.Add(-3*day), now, now)
	liveStale := abyssalAgeWeight(sql.NullTime{}, now.Add(-45*day), now, now)
	if !(liveStale < liveNew) || liveStale < 0.35-1e-9 {
		t.Errorf("staleness: new %v, stale %v; want stale lower and floored at 0.35", liveNew, liveStale)
	}
}

// TestAbyssalLadderEndsBounded: the runaway shape seen in stored
// figures -- two top rungs at nearly the same index (a vertical
// fitted step) and an item far past them. Past the top rung the
// price is the top rung's; below the bottom it never falls past
// a tenth of it; a runaway stored figure reads as missing.
func TestAbyssalLadderEndsBounded(t *testing.T) {
	rungs := []abyssalRung{
		{TypeID: 1, Price: 50e6, Index: 0.2},
		{TypeID: 2, Price: 400e6, Index: 0.5},
		{TypeID: 3, Price: 900e6, Index: 0.5000001},
	}
	for _, idx := range []float64{0.6, 3, 400} {
		logV, sd, lo, hi, scope, ok := abyssalLadderPrice(idx, rungs)
		if !ok || scope != "above" || lo != 3 || hi != 0 {
			t.Fatalf("index %v: %v %v/%v ok=%v", idx, scope, lo, hi, ok)
		}
		if v := math.Exp(logV); math.Abs(v-900e6) > 1 || math.IsInf(sd, 0) || sd > 1 {
			t.Errorf("index %v: above the top rung prices at it: %v (sd %v)", idx, v, sd)
		}
	}
	for _, idx := range []float64{0.1, -50} {
		logV, sd, _, _, scope, _ := abyssalLadderPrice(idx, rungs)
		if v := math.Exp(logV); scope != "below" || v < 5e6-1 || v > 50e6 || sd > 2 {
			t.Errorf("index %v: below the bottom rung %v (sd %v), want within [5m, 50m]", idx, v, sd)
		}
	}
	single := []abyssalRung{{TypeID: 7, Price: 100e6, Index: 0.4}}
	if logV, _, _, _, _, _ := abyssalLadderPrice(90, single); math.Abs(math.Exp(logV)-100e6) > 1 {
		t.Errorf("a single rung is the ceiling too: %v", math.Exp(logV))
	}
	for _, bad := range []float64{math.Inf(1), math.NaN(), 3.4e174, -5} {
		if got := abyssalSaneLadder(sql.NullFloat64{Float64: bad, Valid: true}); got != 0 {
			t.Errorf("stored ladder %v must read as missing, got %v", bad, got)
		}
	}
	if got := abyssalSaneLadder(sql.NullFloat64{Float64: 3e8, Valid: true}); got != 3e8 {
		t.Errorf("a sane stored ladder passes through: %v", got)
	}
}
