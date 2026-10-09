package app

// Spread scanner tests: station-grain aggregation from a
// fixture book, scanner query math, and page renders proven to
// stay on stored rows with zero outbound calls.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedScannerVolume stores the sweep-precomputed 7-day average
// daily volume for one (region, type) the way a completed sweep
// leaves it in market_region_stats (schema 035): the figure the
// scanner now reads instead of re-averaging market_history per
// render.
func seedScannerVolume(t *testing.T, q *db.Queries, regionID, typeID int64, avgDailyVolume float64) {
	t.Helper()
	ctx := context.Background()
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: regionID, TypeID: typeID, AvgDailyVolume: avgDailyVolume,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region volume type %d: %v", typeID, err)
	}
}

func TestStationSweepStoresAndCleansVanished(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	const stationA = int64(60003760)
	const stationB = int64(60003761)
	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 1, TypeID: 34, LocationID: stationA, IsBuyOrder: false, Price: 6.0, VolumeRemain: 100},
			{OrderID: 2, TypeID: 34, LocationID: stationB, IsBuyOrder: false, Price: 5.0, VolumeRemain: 200},
			{OrderID: 3, TypeID: 34, LocationID: stationA, IsBuyOrder: false, Price: 7.0, VolumeRemain: 50},
			{OrderID: 4, TypeID: 34, LocationID: stationA, IsBuyOrder: true, Price: 4.0, VolumeRemain: 40},
			{OrderID: 5, TypeID: 35, LocationID: stationA, IsBuyOrder: false, Price: 100.0, VolumeRemain: 10},
		},
		{
			{OrderID: 6, TypeID: 34, LocationID: stationB, IsBuyOrder: true, Price: 4.5, VolumeRemain: 60},
			{OrderID: 7, TypeID: 34, LocationID: stationB, IsBuyOrder: true, Price: 3.0, VolumeRemain: 70},
			{OrderID: 8, TypeID: 35, LocationID: stationA, IsBuyOrder: true, Price: 90.0, VolumeRemain: 5},
		},
	}
	sweepUntilIdle(t, app)

	rows, err := q.ListMarketStationStatsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list station stats: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("station rows: %d, want 3 (A/34, B/34, A/35): %+v", len(rows), rows)
	}
	byKey := map[stationKey]db.MarketStationStat{}
	for _, r := range rows {
		byKey[stationKey{LocationID: r.LocationID, TypeID: r.TypeID}] = r
	}
	a34 := byKey[stationKey{LocationID: stationA, TypeID: 34}]
	if a34.BestSell != 6.0 || a34.BestBuy != 4.0 || a34.SellOrders != 2 || a34.BuyOrders != 1 || a34.SellVolume != 150 || a34.BuyVolume != 40 {
		t.Fatalf("station A type 34: %+v, want best 6/4, 2/1 orders, 150/40 volume", a34)
	}
	b34 := byKey[stationKey{LocationID: stationB, TypeID: 34}]
	if b34.BestSell != 5.0 || b34.BestBuy != 4.5 || b34.SellOrders != 1 || b34.BuyOrders != 2 || b34.SellVolume != 200 || b34.BuyVolume != 130 {
		t.Fatalf("station B type 34: %+v, want best 5/4.5, 1/2 orders, 200/130 volume", b34)
	}
	a35 := byKey[stationKey{LocationID: stationA, TypeID: 35}]
	if a35.BestSell != 100.0 || a35.BestBuy != 90.0 {
		t.Fatalf("station A type 35: %+v, want best 100/90", a35)
	}

	// Station B leaves the book; a re-sweep drops only its rows.
	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 9, TypeID: 34, LocationID: stationA, IsBuyOrder: false, Price: 6.5, VolumeRemain: 10},
			{OrderID: 10, TypeID: 34, LocationID: stationA, IsBuyOrder: true, Price: 4.0, VolumeRemain: 10},
		},
	}
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: regionSweepKind(10000002), State: fetchStateOK,
		AttemptedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("age sweep record: %v", err)
	}
	sweepUntilIdle(t, app)
	rows, err = q.ListMarketStationStatsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list station stats after re-sweep: %v", err)
	}
	if len(rows) != 1 || rows[0].LocationID != stationA || rows[0].TypeID != 34 {
		t.Fatalf("station rows after station B vanished: %+v, want only A/34", rows)
	}
}

func TestScannerMathFiltersSortAndCap(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	const stationA = int64(60003760)

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (35, 'Pyerite', 18), (36, 'Mexallon', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	// Type 34: spread (10-4)/4 = 150%, profit (10-4)*min(20,100,50)=120.
	// Type 35: spread (100-90)/90 = 11.1%, profit 10*min(100,10,5)=50.
	// Type 36: no buy side -- never a row.
	for _, seed := range []db.UpsertMarketStationStatParams{
		{LocationID: stationA, RegionID: 10000002, TypeID: 34, BestSell: 10, BestBuy: 4, SellOrders: 1, BuyOrders: 1, SellVolume: 100, BuyVolume: 50, UpdatedAt: now},
		{LocationID: stationA, RegionID: 10000002, TypeID: 35, BestSell: 100, BestBuy: 90, SellOrders: 1, BuyOrders: 1, SellVolume: 10, BuyVolume: 5, UpdatedAt: now},
		{LocationID: stationA, RegionID: 10000002, TypeID: 36, BestSell: 50, BestBuy: 0, SellOrders: 1, BuyOrders: 0, SellVolume: 10, BuyVolume: 0, UpdatedAt: now},
	} {
		if err := q.UpsertMarketStationStat(ctx, seed); err != nil {
			t.Fatalf("seed station stat: %v", err)
		}
	}
	seedScannerVolume(t, q, 10000002, 34, 20)
	seedScannerVolume(t, q, 10000002, 35, 100)
	seedScannerVolume(t, q, 10000002, 36, 100)

	view := app.buildScannerView(ctx, url.Values{})
	if !view.HasData || len(view.Rows) != 2 {
		t.Fatalf("scanner rows: %d (hasData %v), want 2", len(view.Rows), view.HasData)
	}
	if view.Rows[0].TypeID != 34 || view.Rows[0].SpreadPct != "150.0%" || view.Rows[0].DailyVolume != "20" || view.Rows[0].DailyProfit != esi.FormatISK(120) {
		t.Fatalf("top scanner row: %+v, want type 34, 150%% spread, 20/day, %s profit", view.Rows[0], esi.FormatISK(120))
	}
	if view.Rows[1].TypeID != 35 || view.Rows[1].DailyProfit != esi.FormatISK(50) {
		t.Fatalf("second scanner row: %+v, want type 35 at %s profit", view.Rows[1], esi.FormatISK(50))
	}
	if view.Rows[0].Station.Name != "Jita 4 - Moon 4 - Caldari Navy Assembly Plant" || view.Rows[0].Station.StationID != stationA {
		t.Fatalf("scanner station: %+v, want the seeded Jita station linked", view.Rows[0].Station)
	}

	// Filters: a 200% spread floor drops everything; a sold-per-day
	// floor of 50 keeps only Pyerite (100/day).
	view = app.buildScannerView(ctx, url.Values{"minspread": {"200"}})
	if len(view.Rows) != 0 {
		t.Fatalf("200%% spread floor rows: %d, want 0", len(view.Rows))
	}
	view = app.buildScannerView(ctx, url.Values{"minvol": {"50"}})
	if len(view.Rows) != 1 || view.Rows[0].TypeID != 35 {
		t.Fatalf("minvol 50 rows: %+v, want only type 35", view.Rows)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("scanner build made %d outbound calls, want 0", got)
	}

	// Cap: 105 more qualifying types still list at most 100 rows.
	for i := int64(0); i < 105; i++ {
		typeID := int64(1000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Cap Item %d", i)); err != nil {
			t.Fatalf("seed cap type: %v", err)
		}
		if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: stationA, RegionID: 10000002, TypeID: typeID,
			BestSell: 20, BestBuy: 10, SellOrders: 1, BuyOrders: 1,
			SellVolume: 1000, BuyVolume: 1000, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed cap stat: %v", err)
		}
		seedScannerVolume(t, q, 10000002, typeID, 10)
	}
	view = app.buildScannerView(ctx, url.Values{})
	if len(view.Rows) != scannerRowCap {
		t.Fatalf("capped scanner rows: %d, want %d", len(view.Rows), scannerRowCap)
	}
}

func TestScannerPageRendersStoredRowsZeroOutbound(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
		LocationID: 60003760, RegionID: 10000002, TypeID: 34,
		BestSell: 10, BestBuy: 4, SellOrders: 1, BuyOrders: 1,
		SellVolume: 100, BuyVolume: 50, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed station stat: %v", err)
	}
	// An unnamed player structure with a qualifying spread still
	// lists, named plainly -- never a bare number.
	if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
		LocationID: 1022734985671, RegionID: 10000002, TypeID: 34,
		BestSell: 12, BestBuy: 4, SellOrders: 1, BuyOrders: 1,
		SellVolume: 100, BuyVolume: 50, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed structure stat: %v", err)
	}
	seedScannerVolume(t, q, 10000002, 34, 20)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/scanner/?region=10000002")
	if code != http.StatusOK {
		t.Fatalf("scanner page: status %d", code)
	}
	mustContain(t, "/market/scanner/", body,
		"Spread scanner",
		"Tritanium",
		"Jita 4 - Moon 4 - Caldari Navy Assembly Plant",
		"Player structure",
		"Sold per day in The Forge",
		"Prices as of",
		"/market/?type=34",
		"/station/?station=60003760",
	)
	if strings.Contains(body, "1022734985671") || strings.Contains(body, "Structure #") {
		t.Error("scanner page leaks a bare structure id or placeholder")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("scanner page made %d outbound calls, want 0", got)
	}
}

func TestScannerPageEmptyBeforeAnySweep(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/scanner/")
	if code != http.StatusOK {
		t.Fatalf("scanner empty page: status %d", code)
	}
	mustContain(t, "/market/scanner/ (empty)", body,
		"Spread scanner",
		"Still gathering prices",
	)
	if strings.Contains(body, "Estimated daily profit") {
		t.Error("empty scanner page shows a results table before any sweep")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("empty scanner page made %d outbound calls, want 0", got)
	}
}

func TestScannerProfitRankingAndCapInSQL(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	const stationA = int64(60003760)

	for _, stmt := range []string{
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	// 105 qualifying rows with strictly increasing profit
	// (1000+10i ISK): the single bounded query must return
	// exactly the top 100 in profit-descending order.
	for i := int64(0); i < 105; i++ {
		typeID := int64(5000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Rank Item %d", i)); err != nil {
			t.Fatalf("seed rank type: %v", err)
		}
		if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: stationA, RegionID: 10000002, TypeID: typeID,
			BestSell: 200 + float64(i), BestBuy: 100, SellOrders: 1, BuyOrders: 1,
			SellVolume: 1000, BuyVolume: 1000, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed rank stat: %v", err)
		}
		seedScannerVolume(t, q, 10000002, typeID, 10)
	}

	view := app.buildScannerView(ctx, url.Values{})
	if !view.HasData {
		t.Fatal("scanner has no data after seeding 105 rows")
	}
	if len(view.Rows) != scannerRowCap {
		t.Fatalf("scanner rows: %d, want %d", len(view.Rows), scannerRowCap)
	}
	for i, row := range view.Rows {
		if want := int64(5104 - i); row.TypeID != want {
			t.Fatalf("row %d: type %d, want %d (profit-desc order)", i, row.TypeID, want)
		}
	}
	if view.Rows[0].DailyProfit != esi.FormatISK(2040) {
		t.Fatalf("top row profit: %s, want %s", view.Rows[0].DailyProfit, esi.FormatISK(2040))
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("scanner build made %d outbound calls, want 0", got)
	}
}
