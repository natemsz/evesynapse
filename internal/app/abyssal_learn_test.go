package app

import (
	"context"
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
	kept := trimLearnOutliers(rows)
	for _, r := range kept {
		if math.Abs(r.Y) > 1 {
			t.Fatalf("an outlier survived trimming: %+v", r)
		}
	}
	if len(kept) < 55 {
		t.Errorf("trimming took ordinary comps too: kept %d of 60", len(kept))
	}
}

// TestContractExitFromStatus: only a 204 before expiry is a sale,
// and a 403 is final without being one.
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
		{"no longer public", status(http.StatusForbidden), now.Add(48 * time.Hour), exitClosed},
		{"server error", status(http.StatusBadGateway), now.Add(48 * time.Hour), exitUnknown},
		{"transport error", errors.New("dial"), now.Add(48 * time.Hour), exitUnknown},
	}
	for _, c := range cases {
		if got := contractExitFromStatus(c.err, c.expires, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAbyssalModelCacheParity: reusing a cached family model over identical
// corpus inputs produces identical estimates to a fresh fit, skips refits
// while the corpus is unchanged or inside the hourly window, refits once
// stale and changed, and leaves departed items frozen out of the due queue.
func TestAbyssalModelCacheParity(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := app.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	exec(`INSERT INTO sde_groups (group_id, name) VALUES (501, 'Test Damage Thing')`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id) VALUES
		(60001, 'Abyssal Test Damage', 501, 0), (30001, 'Test Damage II', 501, 9),
		(30010, 'Faction Test Damage', 501, 9), (30011, 'Officer Test Damage', 501, 9),
		(47735, 'Test Mutaplasmid', 501, 0)`)
	exec(`INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value) VALUES
		(64, 'damageMultiplier', 1, 1, 0, 0), (6, 'capacitorNeed', 1, 0, 0, 0), (50, 'cpu', 1, 0, 0, 0)`)
	exec(`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		(30001, 64, 2.0), (30001, 6, 10), (30001, 50, 20),
		(30010, 64, 2.3), (30010, 6, 9.5), (30010, 50, 20),
		(30011, 64, 2.6), (30011, 6, 9), (30011, 50, 22)`)
	exec(`INSERT INTO abyssal_family_weights (family, attribute_id, weight, penalty_only) VALUES
		('test_damage_thing', 64, 1.0, false), ('test_damage_thing', 6, 0.25, false),
		('test_damage_thing', 50, 0.5, true)`)
	exec(`INSERT INTO abyssal_ranges (mutator_type_id, attribute_id, min_mult, max_mult) VALUES
		(47735, 64, 0.90, 1.25), (47735, 6, 0.90, 1.20), (47735, 50, 0.85, 1.40)`)
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES
		(10000002, 30001, 100000000, $1), (10000002, 47735, 60000000, $1),
		(10000002, 30010, 300000000, $1), (10000002, 30011, 2000000000, $1)`, t0)

	r64 := abyssalRange{MinMult: 0.90, MaxMult: 1.25}
	r6 := abyssalRange{MinMult: 0.90, MaxMult: 1.20}
	r50 := abyssalRange{MinMult: 0.85, MaxMult: 1.40}
	seedItem := func(itemID, contractID int64, p64, p6, p50, ask float64, seen time.Time, departed bool) {
		t.Helper()
		attrs := mustAbyssalJSON(t, []abyssalRollKV{
			{AttributeID: 64, Value: rollAt(2.0, r64, p64, true)},
			{AttributeID: 6, Value: rollAt(10, r6, p6, false)},
			{AttributeID: 50, Value: rollAt(20, r50, p50, false)},
		})
		exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
			attributes, fetched_at, first_seen_at, last_seen_at)
			VALUES ($1, 60001, 30001, 47735, 9001, $2, $3, $3, $3)`, itemID, attrs, seen)
		if departed {
			exec(`INSERT INTO abyssal_sightings (item_id, contract_id, contract_type, price, region_id,
				location_id, date_expired, first_seen_at, last_seen_at, departed_at, depart_kind,
				issuer_id, abyssal_lines, wanted_lines, other_lines, other_unpriced, other_value)
				VALUES ($1, $2, 'item_exchange', $3, 10000002, 60003760, $4, $5, $5, $6, 'expired',
				$7, 1, 0, 0, 0, 0)`,
				itemID, contractID, ask, seen.Add(7*24*time.Hour), seen, seen.Add(2*time.Hour), itemID)
		} else {
			exec(`INSERT INTO abyssal_sightings (item_id, contract_id, contract_type, price, region_id,
				location_id, date_expired, first_seen_at, last_seen_at,
				issuer_id, abyssal_lines, wanted_lines, other_lines, other_unpriced, other_value)
				VALUES ($1, $2, 'item_exchange', $3, 10000002, 60003760, $4, $5, $5,
				$6, 1, 0, 0, 0, 0)`,
				itemID, contractID, ask, seen.Add(7*24*time.Hour), seen, itemID)
		}
	}

	// Seed 80 live items with a clear monotone premium on top damage rolls
	// so the learned correction earns UseModel = true on holdout backtest.
	const liveCount = 80
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < liveCount; i++ {
		p64 := rng.Float64()
		p6 := rng.Float64()
		p50 := 0.3 + 0.6*rng.Float64()
		baseAsk := 100e6 * math.Exp(2.2*p64)
		if p64 > 0.7 {
			baseAsk *= 1.45
		}
		baseAsk *= math.Exp(0.04 * rng.NormFloat64())
		seedItem(int64(910000+i), int64(710000+i), p64, p6, p50, baseAsk, t0.Add(-time.Duration(liveCount-i)*time.Hour), false)
	}
	// Seed one already-scored departed item; it must stay frozen.
	frozenAt := t0.Add(-12 * time.Hour)
	seedItem(999001, 799001, 0.5, 0.5, 0.5, 180e6, t0.Add(-24*time.Hour), true)
	exec(`UPDATE abyssal_items SET family = 'test_damage_thing', score = 0.5, est_value = 180000000, ladder_value = 180000000, scored_at = $1 WHERE item_id = 999001`, frozenAt)

	// Initial scoring populates score and ladder_value on the 80 live items.
	if n := app.scoreAbyssalItemsAt(ctx, t0); n != liveCount {
		t.Fatalf("initial pass scored %d, want %d (departed item must stay frozen)", n, liveCount)
	}
	frozen, err := q.GetAbyssalItem(ctx, 999001)
	if err != nil || !frozen.ScoredAt.Valid || !frozen.ScoredAt.Time.Equal(frozenAt) {
		t.Fatalf("departed item rescored: %+v (err=%v)", frozen.ScoredAt, err)
	}

	// Capture the pre-pass state of all 80 live items so we can run both a
	// fresh-fit pass and a cached-model pass from the exact same DB snapshot
	// at the exact same timestamp t1.
	t1 := t0.Add(4 * time.Hour)
	type itemSnap struct {
		id                         int64
		score, est, lo, hi, ladder float64
		basis, scope, mBasis       string
		comps                      int64
		premium, calib             float64
	}
	readSnaps := func() []itemSnap {
		t.Helper()
		out := make([]itemSnap, liveCount)
		for i := 0; i < liveCount; i++ {
			id := int64(910000 + i)
			ab, err := q.GetAbyssalItem(ctx, id)
			if err != nil {
				t.Fatalf("get item %d: %v", id, err)
			}
			out[i] = itemSnap{
				id: id, score: ab.Score.Float64, est: ab.EstValue.Float64,
				lo: ab.EstLow.Float64, hi: ab.EstHigh.Float64, ladder: ab.LadderValue.Float64,
				basis: ab.EstBasis, scope: ab.EstScope, mBasis: ab.MarketBasis,
				comps: ab.EstComps, premium: ab.PremiumMult, calib: ab.MarketCalib,
			}
		}
		return out
	}
	pre := readSnaps()
	restorePre := func(scoredAt time.Time) {
		t.Helper()
		for _, s := range pre {
			exec(`UPDATE abyssal_items SET score = $2, est_value = $3, est_low = $4, est_high = $5,
				ladder_value = $6, est_basis = $7, est_scope = $8, market_basis = $9,
				est_comps = $10, premium_mult = $11, market_calib = $12, scored_at = $13
				WHERE item_id = $1`,
				s.id, s.score, s.est, s.lo, s.hi, s.ladder, s.basis, s.scope, s.mBasis, s.comps, s.premium, s.calib, scoredAt)
		}
	}

	// Pass A (fresh fit): empty cache at t1.
	app.abyssalModelsMu.Lock()
	app.abyssalModels = nil
	app.abyssalModelsMu.Unlock()
	restorePre(t1.Add(-4 * time.Hour))
	if n := app.scoreAbyssalItemsAt(ctx, t1); n != liveCount {
		t.Fatalf("fresh pass scored %d, want %d", n, liveCount)
	}
	freshOut := readSnaps()
	fitA, err := q.GetAbyssalFamilyFit(ctx, "test_damage_thing")
	if err != nil {
		t.Fatalf("read family fit: %v", err)
	}
	if !fitA.UseModel || !fitA.FittedAt.Equal(t1) {
		t.Fatalf("fresh fit = %+v, want UseModel=true FittedAt=%v", fitA, t1)
	}

	// Pass B (cached model): identical DB state and timestamp t1, cache warm.
	restorePre(t1.Add(-4 * time.Hour))
	if n := app.scoreAbyssalItemsAt(ctx, t1); n != liveCount {
		t.Fatalf("cached pass scored %d, want %d", n, liveCount)
	}
	cachedOut := readSnaps()
	for i := range freshOut {
		if freshOut[i] != cachedOut[i] {
			t.Fatalf("item %d mismatch: fresh=%+v cached=%+v", freshOut[i].id, freshOut[i], cachedOut[i])
		}
	}

	// Unchanged corpus after > 1h: must still hit cache and skip refit.
	t2 := t1.Add(4 * time.Hour)
	if n := app.scoreAbyssalItemsAt(ctx, t2); n != liveCount {
		t.Fatalf("unchanged pass at t2 scored %d, want %d", n, liveCount)
	}
	fitAfterUnchanged, err := q.GetAbyssalFamilyFit(ctx, "test_damage_thing")
	if err != nil || !fitAfterUnchanged.FittedAt.Equal(t1) {
		t.Fatalf("unchanged corpus refit at t2: fitted_at=%v, want %v (err=%v)", fitAfterUnchanged.FittedAt, t1, err)
	}

	// New sighting inside 1h of t1: hourly refit cap still reuses cached model.
	tWithin := t1.Add(30 * time.Minute)
	seedItem(920001, 720001, 0.85, 0.6, 0.7, 900e6, tWithin, false)
	if n := app.scoreAbyssalItemsAt(ctx, tWithin); n != 1 {
		t.Fatalf("new item pass scored %d, want 1", n)
	}
	fitThrottled, err := q.GetAbyssalFamilyFit(ctx, "test_damage_thing")
	if err != nil || !fitThrottled.FittedAt.Equal(t1) {
		t.Fatalf("refit inside 1h window: fitted_at=%v, want %v (err=%v)", fitThrottled.FittedAt, t1, err)
	}

	// Once >= 1h has elapsed and the corpus changed, the next pass refits.
	tAfter := t1.Add(90 * time.Minute)
	seedItem(920002, 720002, 0.82, 0.55, 0.65, 850e6, tAfter, false)
	if n := app.scoreAbyssalItemsAt(ctx, tAfter); n != 1 {
		t.Fatalf("post-window item pass scored %d, want 1", n)
	}
	fitRefit, err := q.GetAbyssalFamilyFit(ctx, "test_damage_thing")
	if err != nil || !fitRefit.FittedAt.Equal(tAfter) {
		t.Fatalf("changed corpus after 1h did not refit: fitted_at=%v, want %v (err=%v)", fitRefit.FittedAt, tAfter, err)
	}

	// The departed item remained frozen through every pass.
	frozen, err = q.GetAbyssalItem(ctx, 999001)
	if err != nil || !frozen.ScoredAt.Valid || !frozen.ScoredAt.Time.Equal(frozenAt) {
		t.Fatalf("departed item was rescored: %+v (err=%v)", frozen.ScoredAt, err)
	}
}
