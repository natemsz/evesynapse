package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// True-value evidence tiers (schema 052): the crawl's fitted
// listings teach a titan an estimated sell, a watched listing
// that vanishes before expiry graduates to the probable
// tier, and a linked character's finished contracts are the
// certain tier -- recorded sales. The tiers are the evidence
// ships price from (truevalue_learn.go): every tier feeds
// the learned correction, confirmed sales weighted highest.
//
// Fixture economics are TestTrueValueWorkerPass scaled by
// 1e6 so contract prices clear the sampler's floor: mineral
// typical 5e6; component build 449.6e6; titan build
// 4,803.5e6; supercarrier ("hel") build 2,401.75e6; the
// netting module's swept typical is 1e8.
// ---------------------------------------------------------------------------

// tvCrawlTransport serves the public-contracts endpoints from
// fixtures: region header pages and per-contract item lists.
// A contract ID in gone answers 404, as the endpoint does
// once a listing leaves the public market.
type tvCrawlTransport struct {
	headers map[int64][]tvPublicContract
	items   map[int64][]tvPublicContractItem
	gone    map[int64]bool
}

func (s *tvCrawlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	header := make(http.Header)
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: header,
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	path := req.URL.Path
	if strings.HasPrefix(path, "/contracts/public/items/") {
		id, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(path, "/contracts/public/items/"), "/"), 10, 64)
		if err != nil {
			return respond(http.StatusBadRequest, `{}`)
		}
		if s.gone[id] {
			return respond(http.StatusNotFound, `{"error":"contract not found"}`)
		}
		body, _ := json.Marshal(s.items[id])
		return respond(http.StatusOK, string(body))
	}
	if strings.HasPrefix(path, "/contracts/public/") {
		region, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(path, "/contracts/public/"), "/"), 10, 64)
		if err != nil {
			return respond(http.StatusBadRequest, `{}`)
		}
		header.Set("X-Pages", "1")
		rows := s.headers[region]
		if rows == nil {
			rows = []tvPublicContract{}
		}
		body, _ := json.Marshal(rows)
		return respond(http.StatusOK, string(body))
	}
	return respond(http.StatusInternalServerError, `{"error":"fixture"}`)
}

// seedTVEvidenceFixture installs the SDE and price fixtures
// the evidence tests price from (see the file comment for
// the arithmetic).
func seedTVEvidenceFixture(t *testing.T, conn *sql.DB, q *db.Queries) time.Time {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (900, 'Fixture')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Titan', 6), (26, 'Supercarrier', 6), (910, 'Fixture Goods', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(34, 'Tritanium', 910, 2, 1),
			(5001, 'Fixture Component', 910, 2, 1),
			(5003, 'Fixture Titan', 25, 2, 1),
			(5004, 'Fixture Supercarrier', 26, 2, 1),
			(6001, 'Fixture Widget', 910, 2, 1),
			(6002, 'Fixture Module', 910, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(7001, 5001, 1, 100, 300),
			(7003, 5003, 1, 1, 3600),
			(7004, 5004, 1, 1, 3600),
			(7006, 6001, 1, 100, 60)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(7001, 34, 100),
			(7003, 5001, 10),
			(7004, 5001, 5),
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
		{TypeID: 34, AveragePrice: 5.5e6, AdjustedPrice: 4e6},
		{TypeID: 5001, AveragePrice: 0, AdjustedPrice: 5e8},
		{TypeID: 5003, AveragePrice: 0, AdjustedPrice: 999},
		{TypeID: 5004, AveragePrice: 0, AdjustedPrice: 999},
		{TypeID: 6001, AveragePrice: 100, AdjustedPrice: 90},
		{TypeID: 6002, AveragePrice: 1e8, AdjustedPrice: 9e7},
	}
	for _, g := range guide {
		if err := q.UpsertGuidePrice(ctx, g); err != nil {
			t.Fatalf("seed guide: %v", err)
		}
	}
	stats := []db.UpsertMarketRegionStatParams{
		{RegionID: 10000002, TypeID: 34, TypicalSell: 5e6, AvgDailyVolume: 1_000_000, UpdatedAt: now},
		{RegionID: 10000002, TypeID: 6001, TypicalSell: 95, AvgDailyVolume: 5000, UpdatedAt: now},
		// The netting module prices from its swept typical --
		// the sampler's first source -- not its guide average.
		{RegionID: 10000002, TypeID: 6002, TypicalSell: 1e8, AvgDailyVolume: 100, UpdatedAt: now},
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
	return now
}

// bumpTVGuideMeta moves the guide's vintage so the next pass
// recomputes every row under changed inputs.
func bumpTVGuideMeta(t *testing.T, q *db.Queries, at time.Time) {
	t.Helper()
	if err := q.UpsertGuidePricesMeta(context.Background(), db.UpsertGuidePricesMetaParams{
		FetchedAt: at, CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("bump guide meta: %v", err)
	}
}

func tvHeader(id int64, price float64, expires time.Time) tvPublicContract {
	return tvPublicContract{
		ContractID: id, Price: price, Status: "outstanding", Type: "item_exchange",
		DateExpired: expires.UTC().Format(time.RFC3339),
	}
}

// TestTrueValueFittedListings: titans trade fitted, so the
// crawl's fitted samples (residuals after netting the fit at
// stored prices) give the fixture titan a listing-tier
// estimated sell; the supercarrier's bare listings price it
// the same way. Both hulls price through their own bounded
// factor (truevalue_learn.go): the weighted median log ratio
// to build cost -- the lower sample's, for two -- shrunk by
// the effective count, 2/(2+6).
func TestTrueValueFittedListings(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(10 * 24 * time.Hour)
	transport := &tvCrawlTransport{
		headers: map[int64][]tvPublicContract{10000002: {
			tvHeader(9001, 5.6e9, expires),
			tvHeader(9002, 6.1e9, expires),
			tvHeader(9003, 2.6e9, expires),
			tvHeader(9004, 2.5e9, expires),
		}},
		items: map[int64][]tvPublicContractItem{
			9001: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 3, IsIncluded: true}},
			9002: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 1, IsIncluded: true}},
			9003: {{TypeID: 5004, Quantity: 1, IsIncluded: true}},
			9004: {{TypeID: 5004, Quantity: 1, IsIncluded: true}},
		},
		gone: map[int64]bool{},
	}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed := seedTVEvidenceFixture(t, conn, q)
	bumpTVGuideMeta(t, q, seed)

	app.refreshTrueValues(ctx)

	// The crawl watched all four listings, shapes recorded.
	obs, err := q.ListTVContractObservationsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(obs) != 4 {
		t.Fatalf("observations = %d, want 4", len(obs))
	}
	byID := make(map[int64]db.TrueValueContractObservation, len(obs))
	for _, o := range obs {
		byID[o.ContractID] = o
	}
	if o := byID[9001]; o.Shape != tvContractShapeFitted || o.SamplePrice != 5.3e9 || o.TypeID != 5003 {
		t.Errorf("observation 9001 = %+v, want fitted titan sample 5.3e9", o)
	}
	if o := byID[9003]; o.Shape != tvContractShapeSingle || o.SamplePrice != 2.6e9 || o.TypeID != 5004 {
		t.Errorf("observation 9003 = %+v, want single supercarrier sample 2.6e9", o)
	}

	bumpTVGuideMeta(t, q, seed.Add(time.Hour))
	app.refreshTrueValues(ctx)

	// Titan: its own factor over the two fitted residuals
	// (5.3e9, 6.0e9): 4803.5e6 x exp(0.25 x log(5.3/4.8035))
	// = 4.9231e9.
	titan, err := q.GetTrueValue(ctx, 5003)
	if err != nil {
		t.Fatalf("titan true value: %v", err)
	}
	if math.Abs(titan.BuildCost-4803.5e6) > 1 {
		t.Errorf("titan build cost = %v, want 4803.5e6 (the fix must not touch costing)", titan.BuildCost)
	}
	if !titan.EstSell.Valid || math.Abs(titan.EstSell.Float64-4.9231e9) > 1e6 || titan.SellSamples != 2 || titan.SellSource != tvSampleListing {
		t.Errorf("titan est = %+v samples %d source %q, want ~4.9231e9 from 2 listing samples",
			titan.EstSell, titan.SellSamples, titan.SellSource)
	}
	// The control: bare supercarrier listings through the
	// same factor: 2401.75e6 x exp(0.25 x log(2.5/2.40175))
	// = 2.4259e9.
	hel, err := q.GetTrueValue(ctx, 5004)
	if err != nil {
		t.Fatalf("supercarrier true value: %v", err)
	}
	if !hel.EstSell.Valid || math.Abs(hel.EstSell.Float64-2.4259e9) > 1e6 || hel.SellSamples != 2 || hel.SellSource != tvSampleListing {
		t.Errorf("supercarrier est = %+v samples %d source %q, want ~2.4259e9 from 2 listing samples",
			hel.EstSell, hel.SellSamples, hel.SellSource)
	}
	if got := trueValueBasis(hel); got != "from 2 public contracts" {
		t.Errorf("supercarrier basis = %q, want %q", got, "from 2 public contracts")
	}
}

// TestTrueValueVanishedListings: both watched titan listings
// leave the public market long before expiry, so they
// graduate to the probable tier and the estimate rests on
// likely sales; a third listing that vanishes inside the
// expiry margin is forgotten, not counted.
func TestTrueValueVanishedListings(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(10 * 24 * time.Hour)
	transport := &tvCrawlTransport{
		headers: map[int64][]tvPublicContract{10000002: {
			tvHeader(9001, 5.6e9, expires),
			tvHeader(9002, 6.1e9, expires),
		}},
		items: map[int64][]tvPublicContractItem{
			9001: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 3, IsIncluded: true}},
			9002: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 1, IsIncluded: true}},
		},
		gone: map[int64]bool{},
	}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed := seedTVEvidenceFixture(t, conn, q)
	bumpTVGuideMeta(t, q, seed)

	app.refreshTrueValues(ctx)
	if got := len(app.trueValuePass.samples[5003]); got != 2 {
		t.Fatalf("listing samples after crawl = %d, want 2", got)
	}
	// A third watched listing, expiring inside the margin.
	if err := q.UpsertTVContractObservation(ctx, db.UpsertTVContractObservationParams{
		ContractID: 9009, RegionID: 10000002, TypeID: 5003, Shape: tvContractShapeFitted,
		ListedPrice: 5.5e9, SamplePrice: 5.4e9, FirstSeenAt: now,
		ExpiresAt: now.Add(12 * time.Hour),
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	// All three listings leave the public market; The Forge
	// comes due again.
	transport.gone[9001] = true
	transport.gone[9002] = true
	transport.gone[9009] = true
	app.trueValuePass.lastCrawl[10000002] = now.Add(-13 * time.Hour)
	app.trueValuePass.crawlNext = 0
	app.refreshTrueValues(ctx)

	vanished, err := q.ListTVVanishedSamplesSince(ctx, sql.NullTime{Time: now.Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("list vanished: %v", err)
	}
	if len(vanished) != 2 {
		t.Fatalf("vanished samples = %d, want 2 (the near-expiry vanishing must not count)", len(vanished))
	}
	live, err := q.ListTVContractObservationsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list live observations: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("live observations = %d, want 0 (9009 forgotten, 9001/9002 vanished)", len(live))
	}
	if got := len(app.trueValuePass.samples[5003]); got != 0 {
		t.Errorf("listing samples after vanish = %d, want 0 (the contracts now speak as likely sales)", got)
	}

	bumpTVGuideMeta(t, q, seed.Add(2*time.Hour))
	app.refreshTrueValues(ctx)
	titan, err := q.GetTrueValue(ctx, 5003)
	if err != nil {
		t.Fatalf("titan true value: %v", err)
	}
	if !titan.EstSell.Valid || math.Abs(titan.EstSell.Float64-4.9231e9) > 1e6 || titan.SellSamples != 2 || titan.SellSource != tvSampleVanished {
		t.Errorf("titan est = %+v samples %d source %q, want ~4.9231e9 from 2 vanished samples",
			titan.EstSell, titan.SellSamples, titan.SellSource)
	}
	if got := trueValueBasis(titan); got != "from 2 likely sales" {
		t.Errorf("titan basis = %q, want %q", got, "from 2 likely sales")
	}
}

// TestTrueValueFulfilledSales: finished contracts from a
// linked character's own sync are recorded sales. Two of them
// (one as issuer, one as acceptor, both fitted) set the
// titan's estimated sell ahead of its live listings; a
// finished contract between strangers, a deleted one, and one
// whose item list was never warmed contribute nothing. (The
// unwarmed one completed 20 days out: age alone no longer
// excludes a sale -- ship fits weight it -- but a sale whose
// shape cannot be judged is no sample at any age.)
func TestTrueValueFulfilledSales(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(10 * 24 * time.Hour)
	transport := &tvCrawlTransport{
		headers: map[int64][]tvPublicContract{10000002: {
			tvHeader(9001, 5.6e9, expires),
			tvHeader(9002, 6.1e9, expires),
		}},
		items: map[int64][]tvPublicContractItem{
			9001: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 3, IsIncluded: true}},
			9002: {{TypeID: 5003, Quantity: 1, IsIncluded: true}, {TypeID: 6002, Quantity: 1, IsIncluded: true}},
		},
		gone: map[int64]bool{},
	}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed := seedTVEvidenceFixture(t, conn, q)
	bumpTVGuideMeta(t, q, seed)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	rfc := func(ts time.Time) string { return ts.UTC().Format(time.RFC3339) }
	contracts := []esi.Contract{
		{ContractID: 8001, IssuerID: fixtureCharA, AcceptorID: 424242, Type: "item_exchange",
			Status: "finished", Price: 5.9e9, DateIssued: rfc(now.Add(-72 * time.Hour)), DateCompleted: rfc(now.Add(-24 * time.Hour))},
		{ContractID: 8002, IssuerID: 434343, AcceptorID: fixtureCharA, Type: "item_exchange",
			Status: "finished_contractor", Price: 6.4e9, DateIssued: rfc(now.Add(-48 * time.Hour)), DateCompleted: rfc(now.Add(-12 * time.Hour))},
		{ContractID: 8003, IssuerID: 434343, AcceptorID: 454545, Type: "item_exchange",
			Status: "finished", Price: 6.2e9, DateIssued: rfc(now.Add(-48 * time.Hour)), DateCompleted: rfc(now.Add(-6 * time.Hour))},
		{ContractID: 8004, IssuerID: fixtureCharA, Type: "item_exchange",
			Status: "deleted", Price: 6.2e9, DateIssued: rfc(now.Add(-48 * time.Hour))},
		{ContractID: 8005, IssuerID: fixtureCharA, Type: "item_exchange",
			Status: "finished", Price: 6.2e9, DateIssued: rfc(now.Add(-30 * 24 * time.Hour)), DateCompleted: rfc(now.Add(-20 * 24 * time.Hour))},
	}
	payload, _ := json.Marshal(contracts)
	if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: fixtureCharA, Kind: esi.SnapContracts, Payload: string(payload),
		FetchedAt: now, CachedUntil: sql.NullTime{Time: now.Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("seed contracts snapshot: %v", err)
	}
	detail := func(id int64, items []esi.ContractItem) {
		body, _ := json.Marshal(items)
		if err := q.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
			ContractID: id, CharacterID: fixtureCharA, Payload: string(body), FetchedAt: now,
		}); err != nil {
			t.Fatalf("seed contract detail %d: %v", id, err)
		}
	}
	inc := func(typeID, qty int64) esi.ContractItem {
		return esi.ContractItem{TypeID: typeID, Quantity: qty, IsIncluded: true}
	}
	detail(8001, []esi.ContractItem{inc(5003, 1), inc(6002, 2)}) // residual 5.9e9 - 2e8 = 5.7e9
	detail(8002, []esi.ContractItem{inc(5003, 1), inc(6002, 1)}) // residual 6.4e9 - 1e8 = 6.3e9

	app.refreshTrueValues(ctx)
	if got := len(app.trueValuePass.fulfilled[5003]); got != 2 {
		t.Fatalf("fulfilled samples after harvest = %d, want 2", got)
	}

	bumpTVGuideMeta(t, q, seed.Add(time.Hour))
	app.refreshTrueValues(ctx)
	titan, err := q.GetTrueValue(ctx, 5003)
	if err != nil {
		t.Fatalf("titan true value: %v", err)
	}
	// The family factor over all four samples, the two
	// recorded sales (5.7e9, 6.3e9) weighted 2.5 against the
	// listings' 1: the weighted median log ratio lands on
	// the 6.0e9 listing's, shrunk by the effective count --
	// 4803.5e6 x exp(0.3615 x 0.22248) = 5.2059e9 -- and the
	// provenance names the tier that carried the weight.
	if !titan.EstSell.Valid || math.Abs(titan.EstSell.Float64-5.2059e9) > 2e7 || titan.SellSamples != 2 || titan.SellSource != tvSampleFulfilled {
		t.Errorf("titan est = %+v samples %d source %q, want ~5.2059e9 resting on 2 fulfilled samples",
			titan.EstSell, titan.SellSamples, titan.SellSource)
	}
	if got := trueValueBasis(titan); got != "from 2 recorded sales" {
		t.Errorf("titan basis = %q, want %q", got, "from 2 recorded sales")
	}
}

// TestTVFulfilledSaleGate pins the participation and outcome
// rules of the certain tier directly.
func TestTVFulfilledSaleGate(t *testing.T) {
	base := esi.Contract{
		ContractID: 1, IssuerID: 111, Type: "item_exchange", Status: "finished", Price: 2e9,
	}
	if !tvFulfilledSale(base, 111) {
		t.Error("issuer's finished sale rejected")
	}
	asAcceptor := base
	asAcceptor.IssuerID, asAcceptor.AcceptorID = 222, 111
	if !tvFulfilledSale(asAcceptor, 111) {
		t.Error("acceptor's finished sale rejected")
	}
	strangers := base
	strangers.IssuerID, strangers.AcceptorID = 222, 333
	if tvFulfilledSale(strangers, 111) {
		t.Error("a sale between strangers counted (assignee visibility is not participation)")
	}
	for _, status := range []string{"deleted", "cancelled", "rejected", "failed", "outstanding", "in_progress"} {
		c := base
		c.Status = status
		if tvFulfilledSale(c, 111) {
			t.Errorf("status %q counted as a sale", status)
		}
	}
	for _, status := range []string{"finished_issuer", "finished_contractor"} {
		c := base
		c.Status = status
		if !tvFulfilledSale(c, 111) {
			t.Errorf("status %q not counted as a sale", status)
		}
	}
	auction := base
	auction.Type = "auction"
	if tvFulfilledSale(auction, 111) {
		t.Error("a finished auction counted (its price is a bid history, not a sale price)")
	}
	cheap := base
	cheap.Price = 500_000
	if tvFulfilledSale(cheap, 111) {
		t.Error("a sub-floor contract counted")
	}
}
