package app

import (
	"errors"
	"math"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// learnFixture draws n comps over two features in [0, 1], with
// target f(x) plus noise, seen one hour apart.
func learnFixture(n int, seed int64, f func(x []float64) float64, noise float64) []abyssalLearnRow {
	rng := rand.New(rand.NewSource(seed))
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]abyssalLearnRow, n)
	for i := range rows {
		x := []float64{rng.Float64(), rng.Float64()}
		rows[i] = abyssalLearnRow{
			ItemID: int64(1000 + i), X: x, W: 1,
			Y:    f(x) + noise*rng.NormFloat64(),
			Seen: t0.Add(time.Duration(i) * time.Hour),
		}
	}
	return rows
}

// TestAbyssalGBMMonotone: whatever the data says, a better roll
// never earns a smaller correction -- even trained on data where
// the second stat runs backwards.
func TestAbyssalGBMMonotone(t *testing.T) {
	rows := learnFixture(300, 1, func(x []float64) float64 { return 0.4*x[0] - 0.3*x[1] }, 0.05)
	m := fitAbyssalGBM(rows, abyssalGBMDefaults)
	for _, other := range []float64{0.1, 0.5, 0.9} {
		prev0, prev1 := math.Inf(-1), math.Inf(-1)
		for v := 0.0; v <= 1.0; v += 0.01 {
			p0 := m.Predict([]float64{v, other})
			p1 := m.Predict([]float64{other, v})
			if p0 < prev0-1e-12 || p1 < prev1-1e-12 {
				t.Fatalf("prediction fell as a stat improved at %v (other %v)", v, other)
			}
			prev0, prev1 = p0, p1
		}
	}
	// It still learned the real, monotone signal.
	if d := m.Predict([]float64{0.9, 0.5}) - m.Predict([]float64{0.1, 0.5}); d < 0.2 {
		t.Errorf("the first stat's premium went unlearned: %v", d)
	}
}

// TestAbyssalLearnGate: a family whose asks carry a real premium
// the ladder misses earns the correction on newer comps it never
// saw; a family of pure noise keeps the factor.
func TestAbyssalLearnGate(t *testing.T) {
	premium := learnFixture(240, 2, func(x []float64) float64 {
		if x[0] > 0.7 {
			return 0.35 // a "good enough" roll clears well above its rung
		}
		return 0
	}, 0.08)
	l := abyssalLearnFamily(premium)
	if !l.Fit.HasBacktest || !l.Fit.UseModel {
		t.Fatalf("a real premium must earn the correction: %+v", l.Fit)
	}
	if !(l.Fit.MAEModel < l.Fit.MAELadder) {
		t.Errorf("model error %v must beat ladder %v", l.Fit.MAEModel, l.Fit.MAELadder)
	}
	hi, _ := l.Correction(-1, []float64{0.9, 0.5})
	lo, _ := l.Correction(-1, []float64{0.2, 0.5})
	if !(hi > lo) || hi > abyssalLearnBandMax || lo < abyssalLearnBandMin {
		t.Errorf("correction must lift the premium rolls within the band: hi %v lo %v", hi, lo)
	}

	noise := learnFixture(240, 3, func([]float64) float64 { return 0 }, 0.3)
	if n := abyssalLearnFamily(noise); n.Fit.UseModel {
		t.Errorf("noise must not earn the correction: %+v", n.Fit)
	}
	if thin := abyssalLearnFamily(premium[:20]); thin.Fit.HasBacktest || thin.Fit.UseModel {
		t.Errorf("a thin family stays on the ladder: %+v", thin.Fit)
	}
}

// TestAbyssalLearnTrim: joke asks never reach the fit.
func TestAbyssalLearnTrim(t *testing.T) {
	rows := learnFixture(60, 4, func([]float64) float64 { return 0 }, 0.05)
	rows[3].Y, rows[7].Y = 4, -4 // 55x and 1/55x the ladder
	kept := abyssalTrimOutliers(rows)
	for _, r := range kept {
		if math.Abs(r.Y) > 1 {
			t.Fatalf("an outlier survived trimming: %+v", r)
		}
	}
	if len(kept) < 55 {
		t.Errorf("trimming took ordinary comps too: kept %d of 60", len(kept))
	}
}

// TestContractExitFromStatus: only a 204 before expiry is a sale.
func TestContractExitFromStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	status := func(code int) error { return &esi.StatusError{Method: "GET", Code: code} }
	cases := []struct {
		name    string
		err     error
		expires time.Time
		want    contractExit
	}{
		{"still listed", nil, now.Add(48 * time.Hour), exitLive},
		{"accepted before expiry", status(http.StatusNoContent), now.Add(48 * time.Hour), exitSold},
		{"accepted, expiry unknown but ruled out", status(http.StatusNoContent), time.Time{}, exitSold},
		{"204 after expiry", status(http.StatusNoContent), now.Add(-time.Hour), exitGone},
		{"deleted", status(http.StatusNotFound), now.Add(48 * time.Hour), exitGone},
		{"server error", status(http.StatusBadGateway), now.Add(48 * time.Hour), exitUnknown},
		{"transport error", errors.New("dial"), now.Add(48 * time.Hour), exitUnknown},
	}
	for _, c := range cases {
		if got := contractExitFromStatus(c.err, c.expires, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
