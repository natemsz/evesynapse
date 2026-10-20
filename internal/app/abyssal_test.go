package app

// The abyssal fairness suite: the model's fairness
// properties stated as tests, per the design contract.
// Monotonicity (a better roll never appraises lower),
// primary gating (a bricked main roll caps, and the cap
// names itself), honest gaps (no inputs, no number),
// performance-not-provenance (identical stats price
// identically whatever they were mutated from, apart from a
// measured premium), and the contamination guard (an item's
// own ask is never an input to its estimate).

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// rollAt returns the rolled value whose direction-adjusted
// percentile in the band is exactly pct.
func rollAt(base float64, r abyssalRange, pct float64, highGood bool) float64 {
	pos := pct
	if !highGood {
		pos = 1 - pct
	}
	return base * (r.MinMult + pos*(r.MaxMult-r.MinMult))
}

// testFamilyInputs builds the fixture family: a damage
// attribute (64, primary), a cap-use attribute (6,
// secondary, lower is better), and CPU (50, penalty-only).
func testFamilyInputs(pcts map[int64]float64) abyssalScoringInputs {
	ranges := map[int64]abyssalRange{
		64:  {MinMult: 0.90, MaxMult: 1.25},
		204: {MinMult: 0.90, MaxMult: 1.10},
		6:   {MinMult: 0.90, MaxMult: 1.20},
		50:  {MinMult: 0.85, MaxMult: 1.40},
	}
	base := map[int64]float64{64: 2.0, 204: 0.95, 6: 10, 50: 20}
	high := map[int64]bool{64: true, 204: true, 6: false, 50: false}
	rolls := map[int64]float64{}
	for attr, pct := range pcts {
		rolls[attr] = rollAt(base[attr], ranges[attr], pct, high[attr])
	}
	return abyssalScoringInputs{
		Family:   "test_damage",
		Rolls:    rolls,
		Ranges:   ranges,
		Base:     base,
		HighGood: high,
		Weights: []db.AbyssalFamilyWeight{
			{Family: "test_damage", AttributeID: 64, Weight: 1.0},
			{Family: "test_damage", AttributeID: 204, Weight: 0.5},
			{Family: "test_damage", AttributeID: 6, Weight: 0.25},
			{Family: "test_damage", AttributeID: 50, Weight: 0.5, PenaltyOnly: true},
		},
	}
}

func TestAbyssalPercentileDirection(t *testing.T) {
	r := abyssalRange{MinMult: 0.9, MaxMult: 1.3}
	if p, ok := abyssalAttrPercentile(100*0.9, 100, r, true); !ok || p != 0 {
		t.Errorf("high-good worst roll: %v/%v, want 0/true", p, ok)
	}
	if p, ok := abyssalAttrPercentile(100*1.3, 100, r, true); !ok || p != 1 {
		t.Errorf("high-good best roll: %v/%v, want 1/true", p, ok)
	}
	if p, ok := abyssalAttrPercentile(100*1.3, 100, r, false); !ok || p != 0 {
		t.Errorf("low-good worst roll (the max value): %v/%v, want 0/true", p, ok)
	}
	if p, ok := abyssalAttrPercentile(100*0.9, 100, r, false); !ok || p != 1 {
		t.Errorf("low-good best roll: %v/%v, want 1/true", p, ok)
	}
	if p, _ := abyssalAttrPercentile(100*1.6, 100, r, true); p != 1 {
		t.Errorf("a roll beyond the band clamps: %v, want 1", p)
	}
	if _, ok := abyssalAttrPercentile(50, 0, r, true); ok {
		t.Error("a zero base supports no percentile")
	}
	if _, ok := abyssalAttrPercentile(50, 100, abyssalRange{MinMult: 1, MaxMult: 1}, true); ok {
		t.Error("an empty band supports no percentile")
	}
}

// TestAbyssalMonotonicity is the property test: over random
// and adversarial roll vectors, improving any single roll,
// all else equal, never lowers the score or the estimate.
func TestAbyssalMonotonicity(t *testing.T) {
	rng := rand.New(rand.NewSource(20261017))
	attrs := []int64{64, 204, 6, 50}
	improve := func(pcts map[int64]float64, attr int64, delta float64) map[int64]float64 {
		out := map[int64]float64{}
		for k, v := range pcts {
			out[k] = v
		}
		out[attr] += delta
		if out[attr] > 1 {
			out[attr] = 1
		}
		return out
	}
	check := func(tag string, pcts map[int64]float64) {
		t.Helper()
		base := scoreAbyssal(testFamilyInputs(pcts))
		if !base.HasScore {
			t.Fatalf("%s: fixture must score", tag)
		}
		baseEst := estimateAbyssal(abyssalEstimateInput{
			Score: base.Score, SourceTypical: 100e6, SourceTypeID: 1,
		})
		for _, attr := range attrs {
			if pcts[attr] >= 1 {
				continue
			}
			up := scoreAbyssal(testFamilyInputs(improve(pcts, attr, 0.07)))
			if up.Score < base.Score-1e-12 {
				t.Errorf("%s: improving attr %d lowered the score: %v -> %v (pcts %v)",
					tag, attr, base.Score, up.Score, pcts)
			}
			upEst := estimateAbyssal(abyssalEstimateInput{
				Score: up.Score, SourceTypical: 100e6, SourceTypeID: 1,
			})
			if upEst.Value < baseEst.Value-1e-6 {
				t.Errorf("%s: improving attr %d lowered the estimate: %v -> %v",
					tag, attr, baseEst.Value, upEst.Value)
			}
		}
	}
	for i := 0; i < 400; i++ {
		pcts := map[int64]float64{}
		for _, attr := range attrs {
			pcts[attr] = rng.Float64()
		}
		check("random", pcts)
	}
	// Adversarial vectors: the corners and the gates.
	for _, pcts := range []map[int64]float64{
		{64: 0, 204: 0, 6: 0, 50: 0},
		{64: 1, 204: 1, 6: 1, 50: 1},
		{64: 0.01, 204: 1, 6: 1, 50: 1}, // bricked primary, everything else perfect
		{64: 1, 204: 1, 6: 1, 50: 0.01}, // bricked fitting, everything else perfect
		{64: 0.5, 204: 0.5, 6: 0.5, 50: 0.5},
		{64: 0.199, 204: 0.9, 6: 0.9, 50: 0.9}, // just under the primary cap knee
		{64: 0.201, 204: 0.9, 6: 0.9, 50: 0.9},
	} {
		check("adversarial", pcts)
	}
}

// TestAbyssalLadderMonotone sweeps the ladder: over one fixed
// ladder, a higher performance index never prices lower, even
// when the rungs' own prices are out of order (a cheap faction
// module out-performing a dear deadspace one).
func TestAbyssalLadderMonotone(t *testing.T) {
	rungs := []abyssalRung{
		{TypeID: 1, Price: 10e6, Index: 0.3},
		{TypeID: 2, Price: 80e6, Index: 0.55},
		{TypeID: 3, Price: 60e6, Index: 0.7}, // out of order
		{TypeID: 4, Price: 900e6, Index: 1.1},
	}
	prev := 0.0
	for idx := -0.2; idx <= 1.5; idx += 0.02 {
		est := estimateAbyssal(abyssalEstimateInput{Score: 0.5, Index: idx, HasIndex: true, Rungs: rungs})
		if !est.OK || est.Basis != "ladder" {
			t.Fatalf("index %v: the ladder must speak (got %+v)", idx, est)
		}
		if est.Value < prev-1e-6 {
			t.Errorf("ladder estimate fell as performance rose: at %v, %v < %v", idx, est.Value, prev)
		}
		if est.Low >= est.Value || est.High <= est.Value {
			t.Errorf("index %v: an estimate never ships as a bare point: %+v", idx, est)
		}
		prev = est.Value
	}
}

func TestAbyssalPrimaryGating(t *testing.T) {
	// The primary rolls at the 4th percentile; the rest is
	// perfect. The weighted sum alone would say 0.45; the
	// gate caps at 0.2 + 0.8*0.04 and names the attribute.
	sc := scoreAbyssal(testFamilyInputs(map[int64]float64{64: 0.04, 204: 1, 6: 1, 50: 1}))
	if !sc.HasScore {
		t.Fatal("fixture must score")
	}
	wantCap := abyssalCapFloor + (1-abyssalCapFloor)*0.04
	if math.Abs(sc.Score-wantCap) > 1e-9 {
		t.Errorf("gated score = %v, want the cap %v", sc.Score, wantCap)
	}
	if sc.CapKind != "primary" || sc.CapAttributeID != 64 {
		t.Errorf("cap = %q/%d, want primary/64", sc.CapKind, sc.CapAttributeID)
	}

	// A deeply bricked fitting roll caps too, and a merely
	// poor one only drags.
	sc = scoreAbyssal(testFamilyInputs(map[int64]float64{64: 0.95, 204: 0.95, 6: 0.95, 50: 0.05}))
	if sc.Score > abyssalFitCap+1e-9 || sc.CapKind != "fitting" || sc.CapAttributeID != 50 {
		t.Errorf("fitting brick: score %v cap %q/%d, want <= %v fitting/50",
			sc.Score, sc.CapKind, sc.CapAttributeID, abyssalFitCap)
	}
	sc = scoreAbyssal(testFamilyInputs(map[int64]float64{64: 0.95, 204: 0.95, 6: 0.95, 50: 0.55}))
	if sc.CapKind != "" {
		t.Errorf("a mid fitting roll must not cap: got %q/%d", sc.CapKind, sc.CapAttributeID)
	}
	if sc.Score >= 0.95 {
		t.Errorf("a mid fitting roll should still drag below the raw rolls: %v", sc.Score)
	}
	// A perfect fitting roll earns nothing: the score equals
	// the non-fitting weighted mean.
	sc = scoreAbyssal(testFamilyInputs(map[int64]float64{64: 0.8, 204: 0.8, 6: 0.8, 50: 1}))
	if math.Abs(sc.Score-0.8) > 1e-9 {
		t.Errorf("perfect fitting roll: score %v, want 0.8 (no bonus)", sc.Score)
	}
}

func TestAbyssalNoEstimateGaps(t *testing.T) {
	// No ranges: no percentiles, no score.
	in := testFamilyInputs(map[int64]float64{64: 0.8, 204: 0.8, 6: 0.8, 50: 0.8})
	in.Ranges = map[int64]abyssalRange{}
	if sc := scoreAbyssal(in); sc.HasScore || len(sc.Percentiles) != 0 {
		t.Errorf("no ranges: score %v pcts %d, want none of either", sc.HasScore, len(sc.Percentiles))
	}
	// Ranges but an unseeded family: percentiles exist, no
	// score, and the estimator is never reached with one.
	in = testFamilyInputs(map[int64]float64{64: 0.8, 204: 0.8, 6: 0.8, 50: 0.8})
	in.Weights = nil
	if sc := scoreAbyssal(in); sc.HasScore || len(sc.Percentiles) != 4 {
		t.Errorf("unseeded family: score %v pcts %d, want no score, 4 percentiles", sc.HasScore, len(sc.Percentiles))
	}
	// A score with nothing to anchor on: no figure.
	if est := estimateAbyssal(abyssalEstimateInput{Score: 0.8}); est.OK {
		t.Errorf("no anchor: estimate %+v, want a gap", est)
	}
	// A score and a source price: the labeled prior speaks.
	est := estimateAbyssal(abyssalEstimateInput{Score: 0.5, SourceTypical: 200})
	if !est.OK || est.Basis != "prior" || est.Value != 200 {
		t.Errorf("prior at score 0.5: %+v, want value 200 (curve is 1.0 at the middle)", est)
	}
	if est.Low >= est.Value || est.High <= est.Value {
		t.Errorf("an estimate never ships as a bare point: %+v", est)
	}
}

// TestAbyssalPerformanceNotProvenance: two items with the
// same absolute stats, mutated from different source tiers,
// land in the same place on their ladder and price the same:
// the price is the item's performance among ordinary modules,
// never what it was made from.
func TestAbyssalPerformanceNotProvenance(t *testing.T) {
	ladder := map[int64]map[int64]float64{
		1: {64: 2.0, 6: 10, 50: 20},  // Tech II, 100m
		2: {64: 2.3, 6: 9.5, 50: 20}, // faction, 300m
		3: {64: 2.6, 6: 9, 50: 22},   // officer, 2b
	}
	prices := map[int64]float64{1: 100e6, 2: 300e6, 3: 2e9}
	rolls := map[int64]float64{64: 2.45, 6: 9.3, 50: 21}
	priceFrom := func(source int64) abyssalEstimate {
		in := testFamilyInputs(nil)
		in.Base = ladder[source]
		in.Rolls = rolls
		idx, ok := abyssalLadderIndex(in, rolls)
		if !ok {
			t.Fatalf("source %d: no index", source)
		}
		var rungs []abyssalRung
		for id, vals := range ladder {
			ri, ok := abyssalLadderIndex(in, vals)
			if !ok {
				t.Fatalf("rung %d: no index", id)
			}
			rungs = append(rungs, abyssalRung{TypeID: id, Price: prices[id], Index: ri})
		}
		return estimateAbyssal(abyssalEstimateInput{Score: 0.7, SourceTypical: prices[source],
			Index: idx, HasIndex: true, Rungs: rungs})
	}
	a, b := priceFrom(1), priceFrom(2)
	if !a.OK || !b.OK || a.Basis != "ladder" || a.Scope != "between" {
		t.Fatalf("ladder must speak between rungs: %+v / %+v", a, b)
	}
	if a.LadderLo != 2 || a.LadderHi != 3 || b.LadderLo != 2 || b.LadderHi != 3 {
		t.Errorf("between faction and officer: %+v / %+v", a, b)
	}
	if d := math.Abs(a.Value-b.Value) / a.Value; d > 0.05 {
		t.Errorf("same stats, different tiers: %v vs %v (%.1f%% apart)", a.Value, b.Value, d*100)
	}
	if a.Value <= prices[2] || a.Value >= prices[3] {
		t.Errorf("value %v must sit between its rungs", a.Value)
	}
}

func TestAbyssalDerivedRepPerSec(t *testing.T) {
	ranges := map[int64]abyssalRange{
		84: {MinMult: 0.85, MaxMult: 1.30},
		73: {MinMult: 0.85, MaxMult: 1.15},
		50: {MinMult: 0.90, MaxMult: 1.30},
	}
	base := map[int64]float64{84: 400, 73: 4500, 50: 20}
	high := map[int64]bool{84: true, 73: false, 50: false}
	mk := func(amountPct, durPct float64) abyssalScoringInputs {
		return abyssalScoringInputs{
			Family: "armor_repair_unit",
			Rolls: map[int64]float64{
				84: rollAt(base[84], ranges[84], amountPct, true),
				73: rollAt(base[73], ranges[73], durPct, false),
				50: rollAt(base[50], ranges[50], 1, false),
			},
			Ranges: ranges, Base: base, HighGood: high,
			Weights: []db.AbyssalFamilyWeight{
				{Family: "armor_repair_unit", AttributeID: 84, Weight: 1.0},
				{Family: "armor_repair_unit", AttributeID: 73, Weight: 0.5},
				{Family: "armor_repair_unit", AttributeID: 50, Weight: 0.5, PenaltyOnly: true},
			},
		}
	}
	// X out-rolls Y on raw amount but cycles far slower; Y
	// repairs more per second and must score higher.
	x := scoreAbyssal(mk(0.9, 0.1))
	y := scoreAbyssal(mk(0.55, 0.9))
	if len(x.Derived) != 1 || len(y.Derived) != 1 {
		t.Fatalf("repairers must carry the derived figure: %+v / %+v", x.Derived, y.Derived)
	}
	if !(y.Derived[0].Percentile > x.Derived[0].Percentile) {
		t.Errorf("derived percentiles: Y %v must beat X %v", y.Derived[0].Percentile, x.Derived[0].Percentile)
	}
	if !(y.Score > x.Score) {
		t.Errorf("scores must follow repair per second here: Y %v, X %v", y.Score, x.Score)
	}
}

func TestAbyssalRecreationCheapestPath(t *testing.T) {
	in := testFamilyInputs(map[int64]float64{64: 0.8, 204: 0.5, 6: 0.5, 50: 0.8})
	sc := scoreAbyssal(in)
	if !sc.HasScore || sc.Attempts <= 1 {
		t.Fatalf("fixture: score %v attempts %v", sc.HasScore, sc.Attempts)
	}
	target := in.Rolls
	own := abyssalBaseCandidate{SourceTypeID: 1, Typical: 200e6, Base: map[int64]float64{64: 2.0, 204: 0.95, 6: 10, 50: 20}}
	cheap := abyssalBaseCandidate{SourceTypeID: 2, Typical: 8e6, Base: map[int64]float64{64: 1.9, 204: 0.9, 6: 9, 50: 18}}
	cost, attempts, ok := abyssalRecreationCost(sc, in, target, []abyssalBaseCandidate{own, cheap}, 5e6)
	if !ok {
		t.Fatal("a reachable priced path exists")
	}
	if want := attempts * (8e6 + 5e6); math.Abs(cost-want) > 1 {
		t.Errorf("recreation = %v, want the cheapest path %v", cost, want)
	}
	// A near-perfect target the cheap base cannot reach and
	// the dear one barely can: priced via the dear one; and
	// with no reachable base at all, no figure.
	in2 := testFamilyInputs(map[int64]float64{64: 0.999, 204: 0.5, 6: 0.5, 50: 0.8})
	sc2 := scoreAbyssal(in2)
	if _, _, ok := abyssalRecreationCost(sc2, in2, in2.Rolls, []abyssalBaseCandidate{cheap}, 5e6); ok {
		t.Error("an unreachable base must not price recreation")
	}
	if _, _, ok := abyssalRecreationCost(sc2, in2, in2.Rolls, nil, 5e6); ok {
		t.Error("no candidates, no recreation figure")
	}
}

func TestAbyssalCurveShape(t *testing.T) {
	prev := -1.0
	for s := 0.0; s <= 1.0001; s += 0.01 {
		m := abyssalCurveMult(math.Min(s, 1))
		if m < prev {
			t.Fatalf("curve fell at %v: %v < %v", s, m, prev)
		}
		prev = m
	}
	if got := abyssalCurveMult(0.5); got != 1.0 {
		t.Errorf("curve(0.5) = %v, want 1.0 (a mid roll is worth the base module)", got)
	}
	if got := abyssalCurveMult(0); got != 0.3 {
		t.Errorf("curve(0) = %v, want 0.3 (a bricked mutation trades under the base)", got)
	}
}

// TestAbyssalScoringPassEndToEnd seeds a corpus and runs
// the real pass: percentiles and score stored, the estimate
// anchored on the corpus (never the item's own ask), and
// the appraisal pass then pricing the contract's abyssal
// line from the stored estimate.
func TestAbyssalScoringPassEndToEnd(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (501, 'Test Damage Thing', 7)`)
	// The ladder: the group's market-listed modules (the
	// mutated type and the mutaplasmid are not on the market).
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
		(10000002, 30001, 100000000, $1), (10000002, 47735, 5000000, $1),
		(10000002, 30010, 300000000, $1), (10000002, 30011, 2000000000, $1)`, now.Add(-time.Hour))

	rollsJSON := func(p64, p6, p50 float64) string {
		r64 := abyssalRange{MinMult: 0.90, MaxMult: 1.25}
		r6 := abyssalRange{MinMult: 0.90, MaxMult: 1.20}
		r50 := abyssalRange{MinMult: 0.85, MaxMult: 1.40}
		return mustAbyssalJSON(t, []abyssalRollKV{
			{AttributeID: 64, Value: rollAt(2.0, r64, p64, true)},
			{AttributeID: 6, Value: rollAt(10, r6, p6, false)},
			{AttributeID: 50, Value: rollAt(20, r50, p50, false)},
		})
	}
	item := func(itemID int64, attrs string) {
		t.Helper()
		exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
			attributes, fetched_at, first_seen_at, last_seen_at)
			VALUES ($1, 60001, 30001, 47735, 9001, $2, $3, $3, $3)`, itemID, attrs, now)
	}
	contract := func(id int64, price float64) {
		t.Helper()
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, price, buyout,
			date_issued, date_expired, items_state, last_cycle, first_seen, last_seen)
			VALUES ($1, 10000002, 'item_exchange', $2, 0, $3, $4, 'stored', 1, $3, $3)`,
			id, price, now.Add(-time.Hour), now.Add(7*24*time.Hour))
	}
	line := func(rec, contractID, itemID int64) {
		t.Helper()
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, item_id, quantity, is_included)
			VALUES ($1, $2, 60001, $3, 1, true)`, rec, contractID, itemID)
	}
	sighting := func(itemID, contractID int64, price float64) {
		t.Helper()
		exec(`INSERT INTO abyssal_sightings (item_id, contract_id, contract_type, price, region_id,
			location_id, date_expired, first_seen_at, last_seen_at)
			VALUES ($1, $2, 'item_exchange', $3, 10000002, 60003760, $4, $5, $5)`,
			itemID, contractID, price, now.Add(7*24*time.Hour), now.Add(-time.Hour))
	}

	// The protagonist: strong rolls, listed CHEAP (150m) --
	// its own ask must not anchor its estimate.
	item(900001, rollsJSON(0.8, 0.6, 0.7))
	contract(8001, 150e6)
	line(1, 8001, 900001)
	sighting(900001, 8001, 150e6)
	// The corpus: eight same-module listings around 205m at
	// like scores.
	asks := []float64{170e6, 180e6, 190e6, 200e6, 210e6, 220e6, 230e6, 240e6}
	for i, ask := range asks {
		id := int64(900101 + i)
		item(id, rollsJSON(0.75, 0.5, 0.7))
		contract(8100+int64(i), ask)
		line(int64(100+i), 8100+int64(i), id)
		sighting(id, 8100+int64(i), ask)
	}

	if n := app.scoreAbyssalItems(ctx); n != 9 {
		t.Fatalf("scored %d items, want 9", n)
	}
	ab, err := q.GetAbyssalItem(ctx, 900001)
	if err != nil {
		t.Fatalf("read protagonist: %v", err)
	}
	if ab.Family != "test_damage_thing" {
		t.Errorf("family = %q, want test_damage_thing (group slug)", ab.Family)
	}
	if !ab.Score.Valid || ab.Score.Float64 < 0.6 || ab.Score.Float64 > 0.85 {
		t.Errorf("score = %v, want ~0.73", ab.Score)
	}
	if !ab.EstValue.Valid {
		t.Fatalf("no estimate stored: %+v", ab)
	}
	// The item rolled 2.36 damage: past the faction rung
	// (2.3), short of the officer (2.6). It prices on that
	// step of the ladder; its own 150m ask, and the cheap
	// listings around it, never set the figure.
	if ab.EstBasis != "ladder" || ab.EstScope != "between" {
		t.Errorf("basis = %q/%q, want ladder/between", ab.EstBasis, ab.EstScope)
	}
	if ab.LadderLoTypeID != 30010 || ab.LadderHiTypeID != 30011 {
		t.Errorf("rungs = %d/%d, want faction 30010 below and officer 30011 above", ab.LadderLoTypeID, ab.LadderHiTypeID)
	}
	if ab.EstValue.Float64 <= 300e6 || ab.EstValue.Float64 >= 2e9 {
		t.Errorf("estimate = %v, want between the faction (300m) and officer (2b) rungs", ab.EstValue.Float64)
	}
	if ab.MarketCalib < abyssalCalibMin || ab.MarketCalib > abyssalCalibMax {
		t.Errorf("market factor %v outside its bounds", ab.MarketCalib)
	}
	if !ab.EstLow.Valid || !ab.EstHigh.Valid || ab.EstLow.Float64 > ab.EstValue.Float64 || ab.EstHigh.Float64 < ab.EstValue.Float64 {
		t.Errorf("spread must bracket the estimate: %+v", ab)
	}
	if !ab.RecreationCost.Valid || ab.RecreationCost.Float64 <= 0 {
		t.Errorf("recreation cross-check missing: %+v", ab.RecreationCost)
	}
	pcts := map[string]float64{}
	if err := json.Unmarshal(ab.Percentiles, &pcts); err != nil {
		t.Fatalf("stored percentiles: %v", err)
	}
	if math.Abs(pcts["64"]-0.8) > 0.01 {
		t.Errorf("stored percentile for 64 = %v, want 0.8", pcts["64"])
	}

	// The appraisal pass prices the contract from the stored
	// estimate: full coverage, abyssal tier, deal visible.
	if n := app.appraisePublicContracts(ctx); n == 0 {
		t.Fatal("appraisal pass priced nothing")
	}
	c1, err := q.GetPublicContract(ctx, 8001)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	if c1.TierMix != "abyssal:1" {
		t.Errorf("tier mix = %q, want abyssal:1", c1.TierMix)
	}
	if math.Abs(c1.OfferedValue-ab.EstValue.Float64) > 1 {
		t.Errorf("offered value = %v, want the stored estimate %v", c1.OfferedValue, ab.EstValue.Float64)
	}
	if !c1.DealRatio.Valid {
		t.Error("a fully priced abyssal contract must carry its deal ratio")
	}
	// And the core seam agrees, line for line.
	res, err := app.AppraiseLines(ctx, []module.AppraisalLine{
		{TypeID: 60001, Quantity: 1, ItemID: 900001, Included: true},
		{TypeID: 60001, Quantity: 1, ItemID: 424242, Included: true}, // unprobed instance
	})
	if err != nil {
		t.Fatalf("AppraiseLines: %v", err)
	}
	if res.Lines[0].Tier != module.AppraisalTierAbyssal || !res.Lines[0].Priced {
		t.Errorf("abyssal line = %+v, want priced at the abyssal tier", res.Lines[0])
	}
	if res.Lines[1].Priced {
		t.Errorf("an unprobed instance must stay unpriced, got %+v", res.Lines[1])
	}
}

func mustAbyssalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAbyssalLadderSizeClass: a ladder holds only the item's
// size class. A medium shield booster is never priced against
// extra-large ones that share its SDE group.
func TestAbyssalLadderSizeClass(t *testing.T) {
	in := testFamilyInputs(nil)
	in.Base = map[int64]float64{64: 2.0, 6: 10, 50: 20}
	if !abyssalSameSize(in, map[int64]float64{64: 2.6, 6: 9, 50: 22}) {
		t.Error("an officer variant of the same size must stay on the ladder")
	}
	if abyssalSameSize(in, map[int64]float64{64: 8.0, 6: 40, 50: 80}) {
		t.Error("a module four times the size must be off the ladder")
	}
	if abyssalSameSize(in, map[int64]float64{6: 10}) {
		t.Error("a type without the primary figure cannot be placed")
	}
}
