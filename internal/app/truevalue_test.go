package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
)

// ---------------------------------------------------------------------------
// True values (schema 051): the contracts filter's honesty, the
// worker pass's arithmetic over a fixture SDE, and every
// consumer's resolution order -- The Forge's swept typical
// sell, then the estimated sell, then unpriced. The guide's
// average prices nothing, anywhere.
// ---------------------------------------------------------------------------

func TestTrueValueContractFilters(t *testing.T) {
	thin := map[int64]float64{11567: 550e9, 2001: 2e6}
	ships := map[int64]bool{11567: true}
	costs := []float64{2e6, 550e9}

	// Header band: a price at 0.1x of the Avatar's build cost
	// anchors nothing (scam range); one at 590B sits inside
	// [0.2x, 5x] of 550B.
	scam := tvPublicContract{ContractID: 1, Type: "item_exchange", Status: "outstanding", Price: 55e9}
	if tvContractHeaderCandidate(scam, costs) {
		t.Error("0.1x header passed the band filter")
	}
	real := tvPublicContract{ContractID: 2, Type: "item_exchange", Status: "outstanding", Price: 590e9}
	if !tvContractHeaderCandidate(real, costs) {
		t.Error("in-band header rejected")
	}
	if tvContractHeaderCandidate(tvPublicContract{Type: "auction", Status: "outstanding", Price: 590e9}, costs) {
		t.Error("an auction header passed")
	}
	if tvContractHeaderCandidate(tvPublicContract{Type: "item_exchange", Status: "finished", Price: 590e9}, costs) {
		t.Error("a finished contract passed")
	}
	if tvContractHeaderCandidate(tvPublicContract{Type: "item_exchange", Status: "outstanding", Price: 500_000}, costs) {
		t.Error("a sub-floor header passed")
	}

	// The netting pricer: stored prices in the house order
	// (Forge typical sell, then the stored estimated sell,
	// then the stored build cost, then adjusted). The guide's
	// average is never a source: 3005 carries one and nothing
	// else, and must come back unpriced. 3010 carries all
	// three real sources and the typical must win; 3002 only
	// an estimated sell; 3003 an estimate and an adjusted
	// (estimate first); 3004 only an adjusted; 3006 only a
	// build cost (a cost input to the netting arithmetic,
	// quoted as nobody's price); 3007 a legacy anchored row
	// -- its figure names no evidence tier, so it is no
	// estimate, and it too nets at its build cost.
	priceOf := tvStoredPricerFrom(
		map[int64]float64{34: 5.5, 3001: 250, 3010: 100, 3011: 1e10},
		map[int64]db.TrueValue{
			3002: {TypeID: 3002, EstSell: sql.NullFloat64{Float64: 45, Valid: true}, SellSource: tvSampleListing},
			3003: {TypeID: 3003, EstSell: sql.NullFloat64{Float64: 50, Valid: true}, SellSource: tvSampleListing},
			3006: {TypeID: 3006, BuildCost: 33},
			3007: {TypeID: 3007, BuildCost: 70, EstSell: sql.NullFloat64{Float64: 70, Valid: true}},
			3010: {TypeID: 3010, EstSell: sql.NullFloat64{Float64: 90, Valid: true}, SellSource: tvSampleListing},
		},
		map[int64]esi.MarketPrice{
			34:   {AveragePrice: 6, AdjustedPrice: 4},
			3001: {AveragePrice: 260, AdjustedPrice: 200},
			3003: {AdjustedPrice: 40},
			3004: {AdjustedPrice: 40},
			3005: {AveragePrice: 777},
			3010: {AveragePrice: 110, AdjustedPrice: 80},
		})
	for id, want := range map[int64]float64{34: 5.5, 3001: 250, 3002: 45, 3003: 50, 3004: 40, 3006: 33, 3007: 70, 3010: 100} {
		if got, ok := priceOf(id); !ok || got != want {
			t.Errorf("priceOf(%d) = (%v, %v), want (%v, true)", id, got, ok, want)
		}
	}
	if _, ok := priceOf(9999); ok {
		t.Error("priceOf priced a type nothing stored knows")
	}
	if _, ok := priceOf(3005); ok {
		t.Error("priceOf priced a type from the guide average, which is never a price source")
	}

	// The stored-estimate invariant: a figure counts as an
	// estimated sell only when an evidence tier stands behind
	// it. A legacy anchored row (the build cost copied into
	// the sell column, no tier named) reads as no estimate to
	// every consumer.
	if _, ok := storedEstimate(db.TrueValue{EstSell: sql.NullFloat64{Float64: 70, Valid: true}}); ok {
		t.Error("anchored row read as an estimate")
	}
	if est, ok := storedEstimate(db.TrueValue{EstSell: sql.NullFloat64{Float64: 70, Valid: true}, SellSource: tvSampleListing}); !ok || est != 70 {
		t.Errorf("tier-backed row = (%v, %v), want (70, true)", est, ok)
	}
	if _, ok := storedEstimate(db.TrueValue{}); ok {
		t.Error("empty row read as an estimate")
	}

	// Item-list filter: exactly the thin type, at an in-band
	// unit price (the single shape).
	item := func(id int64, qty int64) tvPublicContractItem {
		return tvPublicContractItem{TypeID: id, Quantity: qty, IsIncluded: true}
	}
	if id, unit, shape, ok := tvContractSampleFor(real.Price, []tvPublicContractItem{item(11567, 1)}, thin, ships, priceOf); !ok || id != 11567 || unit != 590e9 || shape != tvContractShapeSingle {
		t.Errorf("clean hull contract = (%d, %v, %q, %v), want (11567, 590e9, single, true)", id, unit, shape, ok)
	}
	if _, _, _, ok := tvContractSampleFor(scam.Price, []tvPublicContractItem{item(11567, 1)}, thin, ships, priceOf); ok {
		t.Error("scam-priced hull contract sampled")
	}
	if _, _, _, ok := tvContractSampleFor(real.Price, []tvPublicContractItem{item(11567, 2)}, thin, ships, priceOf); ok {
		t.Error("two-hull contract sampled (ships sample at quantity 1)")
	}
	if _, _, _, ok := tvContractSampleFor(real.Price, []tvPublicContractItem{item(9999, 1)}, thin, ships, priceOf); ok {
		t.Error("contract for a non-thin type sampled")
	}
	// A non-ship thin type samples per unit across quantity.
	if id, unit, shape, ok := tvContractSampleFor(9e6, []tvPublicContractItem{item(2001, 5)}, thin, ships, priceOf); !ok || id != 2001 || unit != 1.8e6 || shape != tvContractShapeSingle {
		t.Errorf("bulk contract = (%d, %v, %q, %v), want (2001, 1.8e6, single, true)", id, unit, shape, ok)
	}

	// The fitted shape: one hull plus other items, netted at
	// stored prices, the residual under the same band. A hull
	// with minerals aboard is a fit whose price the netting
	// can speak for -- the shape titans actually trade in.
	fitted := []tvPublicContractItem{item(11567, 1), item(3001, 2), item(3002, 3)}
	if id, unit, shape, ok := tvContractSampleFor(590e9, fitted, thin, ships, priceOf); !ok || id != 11567 || unit != 590e9-635 || shape != tvContractShapeFitted {
		t.Errorf("fitted hull contract = (%d, %v, %q, %v), want (11567, %v, fitted, true)", id, unit, shape, ok, 590e9-635)
	}
	if id, unit, shape, ok := tvContractSampleFor(real.Price, []tvPublicContractItem{item(11567, 1), item(34, 500)}, thin, ships, priceOf); !ok || id != 11567 || unit != 590e9-2750 || shape != tvContractShapeFitted {
		t.Errorf("hull plus minerals = (%d, %v, %q, %v), want (11567, %v, fitted, true)", id, unit, shape, ok, 590e9-2750)
	}
	// The residual must still land in the band: netting 30B
	// of modules out of a 130B contract leaves 100B against
	// a 110B floor.
	heavy := []tvPublicContractItem{item(11567, 1), item(3011, 3)}
	if _, _, _, ok := tvContractSampleFor(130e9, heavy, thin, ships, priceOf); ok {
		t.Error("fitted contract with an out-of-band residual sampled")
	}
	// An extra item stored prices cannot value rejects the
	// contract outright -- the residual would be a guess.
	if _, _, _, ok := tvContractSampleFor(590e9, []tvPublicContractItem{item(11567, 1), item(9999, 1)}, thin, ships, priceOf); ok {
		t.Error("fitted contract with an unpriceable extra sampled")
	}
	// Two thin hulls aboard: which one is the subject is
	// ambiguous, netting or not.
	thin2 := map[int64]float64{11567: 550e9, 2001: 2e6, 22852: 44e9}
	ships2 := map[int64]bool{11567: true, 22852: true}
	if _, _, _, ok := tvContractSampleFor(600e9, []tvPublicContractItem{item(11567, 1), item(22852, 1)}, thin2, ships2, priceOf); ok {
		t.Error("two-thin-hull contract sampled")
	}
	// A second thin type of any kind aboard rejects too.
	if _, _, _, ok := tvContractSampleFor(590e9, []tvPublicContractItem{item(11567, 1), item(2001, 5)}, thin, ships, priceOf); ok {
		t.Error("fitted contract with another thin type aboard sampled")
	}
	// Two units of the hull is not the fitted shape either.
	if _, _, _, ok := tvContractSampleFor(590e9, []tvPublicContractItem{item(11567, 2), item(3001, 1)}, thin, ships, priceOf); ok {
		t.Error("fitted contract with two hulls sampled")
	}

	// Ship types no longer price through this function at
	// all: they feed every tier into the learned correction
	// (truevalue_learn.go, pinned in truevalue_learn_test.go).
	// What remains here is the tiered estimate thin NON-ship
	// types still use, over the samples inside their anchor's
	// band: the strongest tier with two samples speaks --
	// recorded sales over likely sales over asks -- and a
	// lone sample at any tier cannot.
	tiered := func(p float64, source string) tvContractSample {
		return tvContractSample{price: p, at: time.Now(), source: source}
	}
	if est, source, n, ok := estimatedSellTiered([]tvContractSample{
		tiered(600e9, tvSampleFulfilled), tiered(620e9, tvSampleFulfilled),
		tiered(590e9, tvSampleListing), tiered(595e9, tvSampleListing), tiered(598e9, tvSampleListing),
	}); !ok || est != 610e9 || source != tvSampleFulfilled || n != 2 {
		t.Errorf("fulfilled over listings = (%v, %q, %d, %v), want (610e9, fulfilled, 2, true)", est, source, n, ok)
	}
	if est, source, n, ok := estimatedSellTiered([]tvContractSample{
		tiered(600e9, tvSampleFulfilled),
		tiered(500e9, tvSampleVanished), tiered(520e9, tvSampleVanished),
		tiered(590e9, tvSampleListing), tiered(610e9, tvSampleListing),
	}); !ok || est != 510e9 || source != tvSampleVanished || n != 2 {
		t.Errorf("vanished over listings = (%v, %q, %d, %v), want (510e9, vanished, 2, true)", est, source, n, ok)
	}
	if est, source, n, ok := estimatedSellTiered([]tvContractSample{
		tiered(600e9, tvSampleFulfilled), tiered(500e9, tvSampleVanished), tiered(590e9, tvSampleListing),
	}); ok || est != 0 || source != "" || n != 3 {
		t.Errorf("one sample per tier = (%v, %q, %d, %v), want no estimate (0, \"\", 3, false)", est, source, n, ok)
	}

	// The vanish margin: gone a day or more before expiry is
	// an exit expiry cannot explain; gone inside it is not
	// evidence.
	nowT := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if !tvVanishedBeforeExpiry(nowT, nowT.Add(25*time.Hour)) {
		t.Error("vanished 25h before expiry not counted")
	}
	if tvVanishedBeforeExpiry(nowT, nowT.Add(23*time.Hour)) {
		t.Error("vanished 23h before expiry counted")
	}
	if tvVanishedBeforeExpiry(nowT, nowT.Add(-time.Hour)) {
		t.Error("vanished after expiry counted")
	}

	// Thin: no average at all, or an average over a market
	// that barely trades.
	guide := map[int64]esi.MarketPrice{
		1: {AveragePrice: 0, AdjustedPrice: 10},
		2: {AveragePrice: 10, AdjustedPrice: 9},
		3: {AveragePrice: 10, AdjustedPrice: 9},
	}
	volume := map[int64]float64{2: 3, 3: 5000}
	if !isThinType(guide, volume, 1) || !isThinType(guide, volume, 2) || isThinType(guide, volume, 3) {
		t.Error("thin classification wrong (no-average thin, low-volume thin, healthy not)")
	}
	if !isThinType(guide, volume, 9999) {
		t.Error("a type the guide does not carry is thin")
	}
}

// TestTrueValueWorkerPass runs the pass over a fixture SDE and
// pins its arithmetic, its thin-market rule, and its honesty
// about what is not manufacturable.
//
// Fixture economics: mineral 34 sweeps The Forge at a typical
// sell of 5 (guide: average 5.5, adjusted 4, volume huge).
// Facility: Sotiyo role bonuses 0.99 mat / 0.95 cost, T2 rig
// -2.4% at the null multiplier 2.1 -> matFactor 0.940104;
// reference index 0.02 -> fee rate 0.02*0.95 + 0.0025 + 0.04 =
// 0.0615 of EIV.
func TestTrueValueWorkerPass(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (900, 'Fixture')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Titan', 6), (910, 'Fixture Goods', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(34, 'Tritanium', 910, 2, 1),
			(5001, 'Fixture Component', 910, 2, 1),
			(5003, 'Fixture Titan', 25, 2, 1),
			(6001, 'Fixture Widget', 910, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(7001, 5001, 1, 100, 300),
			(7003, 5003, 1, 1, 3600),
			(7006, 6001, 1, 100, 60)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(7001, 34, 100),
			(7003, 5001, 10),
			(7006, 34, 2)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(35827, 2600, 0.99), (35827, 2601, 0.95),
			(37147, 2594, -2.4), (37147, 2357, 2.1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	guide := []db.UpsertGuidePriceParams{
		{TypeID: 34, AveragePrice: 5.5, AdjustedPrice: 4},
		{TypeID: 5001, AveragePrice: 0, AdjustedPrice: 500},
		{TypeID: 5003, AveragePrice: 0, AdjustedPrice: 999},
		{TypeID: 6001, AveragePrice: 100, AdjustedPrice: 90},
	}
	for _, g := range guide {
		if err := q.UpsertGuidePrice(ctx, g); err != nil {
			t.Fatalf("seed guide: %v", err)
		}
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: now, CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	stats := []db.UpsertMarketRegionStatParams{
		{RegionID: 10000002, TypeID: 34, TypicalSell: 5, AvgDailyVolume: 1_000_000, UpdatedAt: now},
		{RegionID: 10000002, TypeID: 6001, TypicalSell: 95, AvgDailyVolume: 5000, UpdatedAt: now},
	}
	for _, s := range stats {
		if err := q.UpsertMarketRegionStat(ctx, s); err != nil {
			t.Fatalf("seed stats: %v", err)
		}
	}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: trueValueReferenceSystemID, Activity: "manufacturing", CostIndex: 0.02,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}

	app.refreshTrueValues(ctx)

	// Component: ceil(100 x .9 x .940104) = 85 of 34 at 5 =
	// 425, plus fee EIV 400 x .0615 = 24.6 -> 449.6. Thin
	// (no average): no contract evidence has spoken, so no
	// estimated sell is stored -- the build cost is not a
	// price and is never stored as one.
	comp, err := q.GetTrueValue(ctx, 5001)
	if err != nil {
		t.Fatalf("component true value: %v", err)
	}
	if math.Abs(comp.BuildCost-449.6) > 0.01 {
		t.Errorf("component build cost = %v, want 449.6", comp.BuildCost)
	}
	if comp.EstSell.Valid || comp.SellSamples != 0 || comp.SellSource != "" {
		t.Errorf("component est = %+v samples %d source %q, want no estimate stored", comp.EstSell, comp.SellSamples, comp.SellSource)
	}

	// Titan (hull ME 2): ceil(10 x .98 x .940104) = 10
	// components at 449.6 = 4496, plus fee EIV 5000 x .0615
	// = 307.5 -> 4803.5.
	titan, err := q.GetTrueValue(ctx, 5003)
	if err != nil {
		t.Fatalf("titan true value: %v", err)
	}
	if math.Abs(titan.BuildCost-4803.5) > 0.01 {
		t.Errorf("titan build cost = %v, want 4803.5", titan.BuildCost)
	}
	if titan.EstSell.Valid {
		t.Errorf("titan est = %+v, want no estimate stored (no contract evidence)", titan.EstSell)
	}

	// Healthy widget (average + real volume): a build cost
	// is stored, but no estimated sell -- the market rules.
	widget, err := q.GetTrueValue(ctx, 6001)
	if err != nil {
		t.Fatalf("widget true value: %v", err)
	}
	if widget.BuildCost <= 0 {
		t.Errorf("widget build cost = %v, want a stored cost", widget.BuildCost)
	}
	if widget.EstSell.Valid {
		t.Errorf("healthy type has an estimated sell (%v); the market price must rule alone", widget.EstSell.Float64)
	}

	// The mineral is not manufacturable: no row.
	if _, err := q.GetTrueValue(ctx, 34); err != sql.ErrNoRows {
		t.Errorf("mineral true value err = %v, want no row", err)
	}
}

// TestTrueValueCorpusPricing: the stored public-contract
// corpus is the listing tier's price of record, and ships
// price from it the abyssal way (truevalue_learn.go). Two
// bare asks inside the titan's band price it through its own
// bounded factor -- the shrunk weighted median of their log
// ratios to its build cost; a prize ship with NO blueprint
// at all (no build cost, no ladder) prices from the learned
// level of its own asks -- a fitted contract netted at
// stored prices, a 9x outlier trimmed -- and a contract
// carrying two thin hulls speaks for neither.
func TestTrueValueCorpusPricing(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (900, 'Fixture')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Titan', 6), (910, 'Fixture Goods', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(34, 'Tritanium', 910, 2, 1),
			(5001, 'Fixture Component', 910, 2, 1),
			(5003, 'Fixture Titan', 25, 2, 1),
			(5005, 'Fixture Prize Ship', 25, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(7001, 5001, 1, 100, 300),
			(7003, 5003, 1, 1, 3600)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(7001, 34, 100),
			(7003, 5001, 10)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(35827, 2600, 0.99), (35827, 2601, 0.95),
			(37147, 2594, -2.4), (37147, 2357, 2.1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	guide := []db.UpsertGuidePriceParams{
		{TypeID: 34, AveragePrice: 5.5e6, AdjustedPrice: 4e6},
		{TypeID: 5001, AveragePrice: 0, AdjustedPrice: 5e8},
		{TypeID: 5003, AveragePrice: 0, AdjustedPrice: 999},
	}
	for _, g := range guide {
		if err := q.UpsertGuidePrice(ctx, g); err != nil {
			t.Fatalf("seed guide: %v", err)
		}
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: now, CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 5e6, AvgDailyVolume: 1_000_000, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: trueValueReferenceSystemID, Activity: "manufacturing", CostIndex: 0.02,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}

	// The corpus: live item exchanges with their item lists
	// stored, as the contract sweep leaves them.
	contracts := []struct {
		id    int64
		price float64
		items []tvPublicContractItem
	}{
		{9001, 5.3e9, []tvPublicContractItem{{TypeID: 5003, Quantity: 1, IsIncluded: true}}},
		{9002, 6.1e9, []tvPublicContractItem{{TypeID: 5003, Quantity: 1, IsIncluded: true}}},
		{9003, 4e10, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}}},
		{9004, 5e10, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}}},
		{9005, 6e10, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}}},
		{9006, 9e11, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}}},
		{9007, 52e9, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}, {TypeID: 34, Quantity: 500, IsIncluded: true}}},
		{9008, 7e10, []tvPublicContractItem{{TypeID: 5005, Quantity: 1, IsIncluded: true}, {TypeID: 5003, Quantity: 1, IsIncluded: true}}},
	}
	recordID := int64(1)
	for _, c := range contracts {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO pub_contracts (contract_id, region_id, type, price, date_issued, date_expired, items_state, first_seen, last_seen)
			 VALUES ($1, 10000002, 'item_exchange', $2, $3, $4, 'stored', $3, $3)`,
			c.id, c.price, now.Add(-24*time.Hour), now.Add(7*24*time.Hour)); err != nil {
			t.Fatalf("seed contract %d: %v", c.id, err)
		}
		for _, it := range c.items {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included)
				 VALUES ($1, $2, $3, $4, $5)`,
				recordID, c.id, it.TypeID, it.Quantity, it.IsIncluded); err != nil {
				t.Fatalf("seed contract item: %v", err)
			}
			recordID++
		}
	}

	app.refreshTrueValues(ctx)

	// Titan: build cost at the evidence fixture's scale
	// (4803.5e6); its two in-band corpus asks price it
	// through its own factor: the weighted median log ratio
	// (the lower ask's, log(5.3e9 / 4803.5e6)) shrunk by its
	// effective count, 2/(2+6) -- 4803.5e6 x exp(0.25 x
	// 0.09836) = 4.9231e9. The two-hull contract 9008 is no
	// sample.
	titan, err := q.GetTrueValue(ctx, 5003)
	if err != nil {
		t.Fatalf("titan true value: %v", err)
	}
	if math.Abs(titan.BuildCost-4803.5e6) > 1 {
		t.Errorf("titan build cost = %v, want 4803.5e6", titan.BuildCost)
	}
	if !titan.EstSell.Valid || math.Abs(titan.EstSell.Float64-4.9231e9) > 1e6 || titan.SellSource != tvSampleListing || titan.SellSamples != 2 {
		t.Errorf("titan est = %+v source %q samples %d, want ~4.9231e9 from 2 corpus listings",
			titan.EstSell, titan.SellSource, titan.SellSamples)
	}

	// Prize ship: no blueprint, so build cost 0 and no
	// ladder; its asks set a learned level (the fitted one
	// netted to 49.5e9, the 9e11 outlier trimmed): the
	// survivors' weighted mean log price, exp(24.6226) =
	// 4.9373e10.
	prize, err := q.GetTrueValue(ctx, 5005)
	if err != nil {
		t.Fatalf("prize ship true value: %v", err)
	}
	if prize.BuildCost != 0 {
		t.Errorf("prize build cost = %v, want 0 (no blueprint)", prize.BuildCost)
	}
	if !prize.EstSell.Valid || math.Abs(prize.EstSell.Float64-4.9373e10) > 4e8 ||
		prize.SellSource != tvSampleListing || prize.SellSamples != 4 {
		t.Errorf("prize est = %+v source %q samples %d, want ~4.9373e10 from 4 contracts",
			prize.EstSell, prize.SellSource, prize.SellSamples)
	}
}

// TestTrueValueLeafPriceHouseOrder: a BOM leaf prices in the
// house order inside the pass itself — the stored estimated
// sell when the sweep has no typical for it — and a leaf only
// the guide knows (average and adjusted both present) prices
// nothing, leaving the product that builds from it unpriced
// rather than fossil-priced.
func TestTrueValueLeafPriceHouseOrder(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Goods', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(7001, 'Fixture Exotic', 910, 2, 1),
			(7002, 'Fixture Fossil', 910, 2, 1),
			(6002, 'Fixture Gadget', 910, 2, 1),
			(6003, 'Fixture Gizmo', 910, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(8002, 6002, 1, 100, 60),
			(8003, 6003, 1, 100, 60)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(8002, 7001, 10),
			(8003, 7002, 5)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(35827, 2600, 0.99), (35827, 2601, 0.95),
			(37147, 2594, -2.4), (37147, 2357, 2.1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	// The fossil leaf carries the guide's full hand — an
	// average and an adjusted — and nothing else.
	if err := q.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
		TypeID: 7002, AveragePrice: 50, AdjustedPrice: 40,
	}); err != nil {
		t.Fatalf("seed guide: %v", err)
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: now, CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: trueValueReferenceSystemID, Activity: "manufacturing", CostIndex: 0.02,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}
	// The exotic leaf's stored estimate from an earlier pass.
	seedTrueValue(t, q, 7001, 90, 123, 2)

	app.refreshTrueValues(ctx)

	// Gadget: ceil(10 x .9 x .940104) = 9 exotic at the stored
	// estimate 123 = 1107. No guide row for 7001, so no EIV
	// base exists and the job fee is left out entirely.
	gadget, err := q.GetTrueValue(ctx, 6002)
	if err != nil {
		t.Fatalf("gadget true value: %v", err)
	}
	if math.Abs(gadget.BuildCost-1107) > 0.01 {
		t.Errorf("gadget build cost = %v, want 1107 (leaves at the stored estimated sell)", gadget.BuildCost)
	}

	// Gizmo: its only leaf prices from nothing the house
	// trusts, so the gizmo stays unpriced — no row at all.
	if _, err := q.GetTrueValue(ctx, 6003); err != sql.ErrNoRows {
		t.Errorf("gizmo true value err = %v, want no row (guide-priced leaves price nothing)", err)
	}
}

// tvPricesTransport serves one price guide; every other call
// fails (the page fetches its book and finds none, which the
// market page renders honestly).
type tvPricesTransport struct{ calls int }

func (s *tvPricesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	header := make(http.Header)
	if req.URL.Path == "/markets/prices/" {
		header.Set("Expires", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
		body, _ := json.Marshal([]esi.MarketPrice{
			{TypeID: 11567, AveragePrice: 0, AdjustedPrice: 64840457150.95},
			{TypeID: 3001, AveragePrice: 250, AdjustedPrice: 200},
			{TypeID: 3002, AveragePrice: 0, AdjustedPrice: 40},
			{TypeID: 3005, AveragePrice: 777, AdjustedPrice: 700},
		})
		return &http.Response{StatusCode: http.StatusOK, Header: header,
			Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	}
	if strings.HasPrefix(req.URL.Path, "/markets/10000002/orders/") {
		// An empty book: the page renders its price cards
		// from the guide alone.
		return &http.Response{StatusCode: http.StatusOK, Header: header,
			Body: io.NopCloser(strings.NewReader(`[]`))}, nil
	}
	return &http.Response{StatusCode: http.StatusInternalServerError, Header: header,
		Body: io.NopCloser(strings.NewReader(`{"error":"fixture"}`))}, nil
}

func seedTrueValue(t *testing.T, q *db.Queries, typeID int64, build, est float64, samples int64) {
	t.Helper()
	estv := sql.NullFloat64{}
	if est > 0 {
		estv = sql.NullFloat64{Float64: est, Valid: true}
	}
	// A seeded estimate represents a tier-backed one (the
	// only kind the worker writes): two or more samples name
	// the listing tier behind it.
	source := ""
	if est > 0 && samples >= 2 {
		source = tvSampleListing
	}
	if err := q.UpsertTrueValue(context.Background(), db.UpsertTrueValueParams{
		TypeID: typeID, BuildCost: build, EstSell: estv, SellSamples: samples,
		ComputedAt: time.Now().UTC(), ProfileVersion: trueValueProfileVersion,
		SellSource: source,
	}); err != nil {
		t.Fatalf("seed true value %d: %v", typeID, err)
	}
}

// TestTrueValueFitPricing: an Avatar-hulled fit prices its
// hull at the estimated sell, not at CCP's 64.8B fossil --
// in the stored reprice and in the fitting tool's own sum.
// A line the sweep prices goes at its Forge typical; a line
// only the guide knows (3005: average 777, adjusted 700)
// prices nothing at all.
func TestTrueValueFitPricing(t *testing.T) {
	transport := &tvPricesTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedTrueValue(t, q, 11567, 550e9, 590e9, 4)
	seedTrueValue(t, q, 3002, 30, 45, 2) // a module tier too: avg 0, adj 40, est 45
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 3001, TypicalSell: 250, AvgDailyVolume: 100,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}

	saved := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Avatar fixture", ShipTypeID: 11567, IsPublic: true,
		CreatedAt: saved, UpdatedAt: saved,
		ItemsJson: `{"name":"Avatar fixture","shipTypeId":11567,"items":[{"typeId":3001,"qty":2},{"typeId":3002,"qty":3},{"typeId":3005,"qty":1}]}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}

	if stored, limited := app.refreshGuidePrices(ctx); limited || !stored {
		t.Fatalf("guide refresh = (stored %v, limited %v), want (true, false)", stored, limited)
	}
	got, err := q.GetPublicFitting(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := 590e9 + 2*250 + 3*45 // hull at est, 3001 at Forge typical, 3002 at est; 3005 unpriced
	if got.TotalPrice != want {
		t.Errorf("repriced fit = %v ISK, want %v (hull at estimated sell, not the 64.8B adjusted)", got.TotalPrice, want)
	}

	doc := &fitview.FitDoc{
		ShipTypeID: 11567,
		Items:      []fitview.FitDocItem{{TypeID: 3001, Qty: 2}, {TypeID: 3002, Qty: 3}, {TypeID: 3005, Qty: 1}},
	}
	cost := app.FitBuilder().FitCost(ctx, doc)
	if cost.HullRaw != 590e9 || cost.TotalRaw != want {
		t.Errorf("FitCost hull/total = %v / %v, want 590e9 / %v", cost.HullRaw, cost.TotalRaw, want)
	}
	if cost.Unpriced != 1 {
		t.Errorf("FitCost unpriced lines = %d, want 1 (the guide-average-only module)", cost.Unpriced)
	}
}

// TestTrueValueItemPage: the item page of a contract-traded
// type shows Build cost and Estimated sell with its basis,
// and CCP's adjusted price no longer poses as the price;
// a healthy type's page is untouched.
func TestTrueValueItemPage(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (900, 'Fixture')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Titan', 6), (910, 'Fixture Minerals', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(11567, 'Avatar', 25, 2, 1),
			(34, 'Tritanium', 910, 2, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	app.pricesMu.Lock()
	app.prices = map[int64]esi.MarketPrice{
		11567: {TypeID: 11567, AveragePrice: 0, AdjustedPrice: 64840457150.95},
		34:    {TypeID: 34, AveragePrice: 5.9, AdjustedPrice: 5},
	}
	app.pricesMu.Unlock()

	seedTrueValue(t, q, 11567, 550e9, 590e9, 4)
	// A healthy type's stored row carries no estimated sell:
	// it must not surface on the page at all.
	seedTrueValue(t, q, 34, 4.2, 0, 0)

	code, body := getPage(t, app, cookie, "/items/type/11567/")
	if code != http.StatusOK {
		t.Fatalf("avatar page: status %d", code)
	}
	mustContain(t, "/items/type/11567/", body,
		"Build cost", "550,000,000,000",
		"Estimated sell", "590,000,000,000", "from 4 public contracts",
	)
	if strings.Contains(body, "Adjusted price") || strings.Contains(body, "64,840,457,150.95") {
		t.Error("avatar page still presents CCP's adjusted price")
	}

	code, body = getPage(t, app, cookie, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("tritanium page: status %d", code)
	}
	mustContain(t, "/items/type/34/", body, "Average price", "5.90", "Adjusted price", "5.00")
	if strings.Contains(body, "Build cost") || strings.Contains(body, "Estimated sell") {
		t.Error("healthy type's page gained true-value rows")
	}
}

// TestTrueValueMarketPage: the market page's price card for a
// no-average type presents the estimated value, labelled as
// such, in place of the adjusted price.
func TestTrueValueMarketPage(t *testing.T) {
	transport := &tvPricesTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 11567, Name: "Avatar"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	seedTrueValue(t, q, 11567, 550e9, 590e9, 4)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/market/?type=11567&region=10000002")
	if code != http.StatusOK {
		t.Fatalf("market page: status %d", code)
	}
	mustContain(t, "/market/ avatar", body,
		`kpi-k">Estimated value`,
		"590,000,000,000",
		"from 4 public contracts",
	)
	if strings.Contains(body, `kpi-k">Adjusted price`) {
		t.Error("market page still cards the adjusted price for a no-average type")
	}
}

// TestTrueValueNetWorthFallback: net worth and the daily
// sampler price a hangar in the house order -- the Forge
// typical beats the guide average where both exist, and the
// estimated sell covers both a type the guide lacks entirely
// and one whose only guide price is the adjusted fossil.
func TestTrueValueNetWorthFallback(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 31, TypeID: 34, Quantity: 100}, // Forge typical 6 (guide average 5 must not win)
		{ItemID: 32, TypeID: 5001, Quantity: 1}, // guide adjusted 40 only; est 700
		{ItemID: 33, TypeID: 4242, Quantity: 2}, // no guide row at all; est 500
	})
	setHomeLayout(t, q, user.ID, `[{"id":"networth"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	if err := q.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
		TypeID: 34, AdjustedPrice: 4, AveragePrice: 5,
	}); err != nil {
		t.Fatalf("seed guide: %v", err)
	}
	if err := q.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
		TypeID: 5001, AdjustedPrice: 40, AveragePrice: 0,
	}); err != nil {
		t.Fatalf("seed guide: %v", err)
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: time.Now().UTC(), CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 6, AvgDailyVolume: 1_000_000,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	seedTrueValue(t, q, 5001, 650, 700, 2)
	seedTrueValue(t, q, 4242, 480, 500, 2)

	// Assets: 100x6 + 1x700 + 2x500 = 2300; total 3300.50,
	// every stack priced.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "2,300.00", "3,300.50")
	if strings.Contains(body, "items priced") {
		t.Error("net worth reports partial coverage when every stack priced via true values")
	}

	app.sampleWalletHistory(ctx, ch, time.Now())
	rows, err := q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("samples: %v (%d rows)", err, len(rows))
	}
	if !rows[0].NetWorth.Valid || rows[0].NetWorth.Float64 != 3300.5 {
		t.Errorf("sample net worth = %+v, want 3300.5 (same order as the widget)", rows[0].NetWorth)
	}
}

// TestFitPricingGlobalMarketFill: a global-market type (its only
// swept book is the global region's) prices in fit valuation at
// its global typical -- in the stored reprice and in the fitting
// tool's own sum -- and the own-swept global typical beats its
// stored estimated sell. A type holding both a Forge row and a
// global row prices from The Forge.
func TestFitPricingGlobalMarketFill(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	seed := func(regionID, typeID int64, typical float64) {
		t.Helper()
		if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
			RegionID: regionID, TypeID: typeID, TypicalSell: typical, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed region stat (%d, %d): %v", regionID, typeID, err)
		}
	}
	// 3010, the hull: the global book only.
	seed(globalMarketRegion, 3010, 7_500_000)
	// 3007, an item: the global book only, plus an estimated
	// sell far below it -- the swept global typical must win.
	seed(globalMarketRegion, 3007, 5_000_000)
	seedTrueValue(t, q, 3007, 300, 400, 3)
	// 3008, an item holding both books: The Forge wins.
	seed(defaultMarketRegion, 3008, 100)
	seed(globalMarketRegion, 3008, 999)

	saved := now.Add(-48 * time.Hour).Truncate(time.Second)
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Global fixture", ShipTypeID: 3010, IsPublic: true,
		CreatedAt: saved, UpdatedAt: saved,
		ItemsJson: `{"name":"Global fixture","shipTypeId":3010,"items":[{"typeId":3007,"qty":2},{"typeId":3008,"qty":3}]}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}

	app.repriceFittings(ctx)
	got, err := q.GetPublicFitting(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := float64(7_500_000 + 2*5_000_000 + 3*100)
	if got.TotalPrice != want {
		t.Errorf("repriced fit = %v ISK, want %v (hull and 3007 at their global typicals, 3008 at its Forge typical)", got.TotalPrice, want)
	}

	doc := &fitview.FitDoc{
		ShipTypeID: 3010,
		Items:      []fitview.FitDocItem{{TypeID: 3007, Qty: 2}, {TypeID: 3008, Qty: 3}},
	}
	cost := app.FitBuilder().FitCost(ctx, doc)
	if cost.HullRaw != 7_500_000 || cost.TotalRaw != want {
		t.Errorf("FitCost hull/total = %v / %v, want 7500000 / %v (the global fill, same as the reprice)", cost.HullRaw, cost.TotalRaw, want)
	}
	if cost.Unpriced != 0 {
		t.Errorf("FitCost unpriced lines = %d, want 0 (every line holds a swept typical)", cost.Unpriced)
	}
}

// TestFitPricingGlobalOnlyReprice: with no Forge typical and no
// estimated sell anywhere in the database, a fit whose types
// price only from the global book still reprices -- the stored
// reprice's source guard counts the global book as a price
// source, not just The Forge's.
func TestFitPricingGlobalOnlyReprice(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: globalMarketRegion, TypeID: 3010, TypicalSell: 7_500_000, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	saved := now.Add(-48 * time.Hour).Truncate(time.Second)
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Global only fixture", ShipTypeID: 3010, IsPublic: true,
		CreatedAt: saved, UpdatedAt: saved,
		ItemsJson: `{"name":"Global only fixture","shipTypeId":3010,"items":[]}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}
	if n := app.repriceFittings(ctx); n != 1 {
		t.Fatalf("repriced fits = %d, want 1 (the global book alone is a price source)", n)
	}
	got, err := q.GetPublicFitting(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalPrice != 7_500_000 {
		t.Errorf("repriced fit = %v ISK, want 7500000 (the hull at its global typical)", got.TotalPrice)
	}
}
