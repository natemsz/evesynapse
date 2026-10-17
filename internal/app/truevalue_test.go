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
// consumer's resolution order -- average, then the estimated
// sell, then CCP's adjusted price.
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
	// then adjusted). The guide's average is never a source:
	// 3005 carries one and nothing else, and must come back
	// unpriced. 3010 carries all three real sources and the
	// typical must win; 3002 only an estimated sell; 3003 an
	// estimate and an adjusted (estimate first); 3004 only
	// an adjusted.
	priceOf := tvStoredPricerFrom(
		map[int64]float64{34: 5.5, 3001: 250, 3010: 100, 3011: 1e10},
		map[int64]db.TrueValue{
			3002: {TypeID: 3002, EstSell: sql.NullFloat64{Float64: 45, Valid: true}},
			3003: {TypeID: 3003, EstSell: sql.NullFloat64{Float64: 50, Valid: true}},
			3010: {TypeID: 3010, EstSell: sql.NullFloat64{Float64: 90, Valid: true}},
		},
		map[int64]esi.MarketPrice{
			34:   {AveragePrice: 6, AdjustedPrice: 4},
			3001: {AveragePrice: 260, AdjustedPrice: 200},
			3003: {AdjustedPrice: 40},
			3004: {AdjustedPrice: 40},
			3005: {AveragePrice: 777},
			3010: {AveragePrice: 110, AdjustedPrice: 80},
		})
	for id, want := range map[int64]float64{34: 5.5, 3001: 250, 3002: 45, 3003: 50, 3004: 40, 3010: 100} {
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

	// Estimated sell: the median once two samples agree,
	// the build cost itself before that.
	sample := func(p float64) tvContractSample { return tvContractSample{price: p, at: time.Now()} }
	if got := estimatedSell(550e9, nil); got != 550e9 {
		t.Errorf("no samples: est = %v, want build cost 550e9", got)
	}
	if got := estimatedSell(550e9, []tvContractSample{sample(590e9)}); got != 550e9 {
		t.Errorf("one sample: est = %v, want build cost 550e9", got)
	}
	if got := estimatedSell(550e9, []tvContractSample{sample(610e9), sample(590e9)}); got != 600e9 {
		t.Errorf("two samples: est = %v, want median 600e9", got)
	}
	if got := estimatedSell(550e9, []tvContractSample{sample(700e9), sample(590e9), sample(610e9)}); got != 610e9 {
		t.Errorf("three samples: est = %v, want median 610e9", got)
	}

	// Tiered estimates: the strongest tier with two samples
	// speaks -- recorded sales over likely sales over asks --
	// and a lone sample at any tier cannot.
	tiered := func(p float64, source string) tvContractSample {
		return tvContractSample{price: p, at: time.Now(), source: source}
	}
	if est, source, n := estimatedSellTiered(550e9, []tvContractSample{
		tiered(600e9, tvSampleFulfilled), tiered(620e9, tvSampleFulfilled),
		tiered(590e9, tvSampleListing), tiered(595e9, tvSampleListing), tiered(598e9, tvSampleListing),
	}); est != 610e9 || source != tvSampleFulfilled || n != 2 {
		t.Errorf("fulfilled over listings = (%v, %q, %d), want (610e9, fulfilled, 2)", est, source, n)
	}
	if est, source, n := estimatedSellTiered(550e9, []tvContractSample{
		tiered(600e9, tvSampleFulfilled),
		tiered(500e9, tvSampleVanished), tiered(520e9, tvSampleVanished),
		tiered(590e9, tvSampleListing), tiered(610e9, tvSampleListing),
	}); est != 510e9 || source != tvSampleVanished || n != 2 {
		t.Errorf("vanished over listings = (%v, %q, %d), want (510e9, vanished, 2)", est, source, n)
	}
	if est, source, n := estimatedSellTiered(550e9, []tvContractSample{
		tiered(600e9, tvSampleFulfilled), tiered(500e9, tvSampleVanished), tiered(590e9, tvSampleListing),
	}); est != 550e9 || source != "" || n != 3 {
		t.Errorf("one sample per tier = (%v, %q, %d), want (550e9 anchor, \"\", 3)", est, source, n)
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
	// (no average): estimated sell anchors to the build.
	comp, err := q.GetTrueValue(ctx, 5001)
	if err != nil {
		t.Fatalf("component true value: %v", err)
	}
	if math.Abs(comp.BuildCost-449.6) > 0.01 {
		t.Errorf("component build cost = %v, want 449.6", comp.BuildCost)
	}
	if !comp.EstSell.Valid || comp.EstSell.Float64 != comp.BuildCost || comp.SellSamples != 0 {
		t.Errorf("component est = %+v samples %d, want anchored at build cost with 0 samples", comp.EstSell, comp.SellSamples)
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
	if !titan.EstSell.Valid || titan.EstSell.Float64 != titan.BuildCost {
		t.Errorf("titan est = %+v, want anchored at build cost", titan.EstSell)
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
	if err := q.UpsertTrueValue(context.Background(), db.UpsertTrueValueParams{
		TypeID: typeID, BuildCost: build, EstSell: estv, SellSamples: samples,
		ComputedAt: time.Now().UTC(), ProfileVersion: trueValueProfileVersion,
	}); err != nil {
		t.Fatalf("seed true value %d: %v", typeID, err)
	}
}

// TestTrueValueFitPricing: an Avatar-hulled fit prices its
// hull at the estimated sell, not at CCP's 64.8B fossil --
// in the stored reprice and in the fitting tool's own sum.
func TestTrueValueFitPricing(t *testing.T) {
	transport := &tvPricesTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedTrueValue(t, q, 11567, 550e9, 590e9, 4)
	seedTrueValue(t, q, 3002, 30, 45, 0) // a module tier too: avg 0, adj 40, est 45

	saved := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Avatar fixture", ShipTypeID: 11567, IsPublic: true,
		CreatedAt: saved, UpdatedAt: saved,
		ItemsJson: `{"name":"Avatar fixture","shipTypeId":11567,"items":[{"typeId":3001,"qty":2},{"typeId":3002,"qty":3}]}`,
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
	want := 590e9 + 2*250 + 3*45 // hull at est, 3001 at average, 3002 at est
	if got.TotalPrice != want {
		t.Errorf("repriced fit = %v ISK, want %v (hull at estimated sell, not the 64.8B adjusted)", got.TotalPrice, want)
	}

	doc := &fitview.FitDoc{
		ShipTypeID: 11567,
		Items:      []fitview.FitDocItem{{TypeID: 3001, Qty: 2}, {TypeID: 3002, Qty: 3}},
	}
	cost := app.FitBuilder().FitCost(ctx, doc)
	if cost.HullRaw != 590e9 || cost.TotalRaw != want {
		t.Errorf("FitCost hull/total = %v / %v, want 590e9 / %v", cost.HullRaw, cost.TotalRaw, want)
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
// sampler price a hangar in the house order -- the estimated
// sell covers both a type the guide lacks entirely and one
// whose only guide price is the adjusted fossil.
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
		{ItemID: 31, TypeID: 34, Quantity: 100}, // guide average 5
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
	seedTrueValue(t, q, 5001, 650, 700, 2)
	seedTrueValue(t, q, 4242, 480, 500, 0)

	// Assets: 100x5 + 1x700 + 2x500 = 2200; total 3200.50,
	// every stack priced.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "2,200.00", "3,200.50")
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
	if !rows[0].NetWorth.Valid || rows[0].NetWorth.Float64 != 3200.5 {
		t.Errorf("sample net worth = %+v, want 3200.5 (same order as the widget)", rows[0].NetWorth)
	}
}
