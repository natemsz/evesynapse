package app

// P3 Tradefinder tests: region-pair margin math, the three-day
// freshness rule on either side, the volume-cap formula, filters,
// sort and cap, best-price hints, and page renders proven to stay
// on stored rows with zero outbound calls.

import (
	"context"
	"evesynapse/internal/app/tradefinder"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	tfOrigin = int64(10000002) // The Forge
	tfDest   = int64(10000043) // Domain
)

// seedTradefinderFixture lays down the SDE names a tradefinder
// test judges by. Region stats (with the stored sold-per-day
// average) are seeded per test.
func seedTradefinderFixture(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	conn := app.db
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (35, 'Pyerite', 18), (36, 'Mexallon', 18), (37, 'Isogen', 18), (38, 'Nocxium', 18), (39, 'Zydrine', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142), (60003761, 'Jita 4 - Moon 10 - Wiyrkomi Peace Corps Assembly Plant', 30000142), (60008494, 'Amarr VIII (Oris) - Emperor Family Academy', 30002187)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9), (30002187, 'Amarr', 10000043, 1.0)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000043, 'Domain')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
}

func tfRegionStat(regionID, typeID int64, typicalBuy, typicalSell float64, buyVolume, sellVolume int64, avgDailyVolume float64, stamp time.Time) db.UpsertMarketRegionStatParams {
	return db.UpsertMarketRegionStatParams{
		RegionID: regionID, TypeID: typeID,
		BestSell: typicalSell, TypicalSell: typicalSell, SellBand: typicalSell,
		BestBuy: typicalBuy, TypicalBuy: typicalBuy, BuyBand: typicalBuy,
		SellOrders: 1, BuyOrders: 1, SellVolume: sellVolume, BuyVolume: buyVolume,
		AvgDailyVolume: avgDailyVolume, UpdatedAt: stamp,
	}
}

func TestTradefinderMathFreshnessFiltersAndHints(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedTradefinderFixture(t, app)

	nowT := time.Now().UTC()
	now := nowT
	stale := nowT.Add(-4 * 24 * time.Hour)

	// Type 34: margin 10-4=6, 150%; units min(20/day, origin
	// stock 50) = 20 -> 120/day. The headline route.
	// Type 35: margin 100-90=10, 11.1%; units min(100, 5) = 5
	// -> 50/day (the origin's stock on sale is the cap that bites).
	// Type 36: healthy margin but nothing on sale in the origin --
	// cannot move, drops out.
	// Type 37: origin figures 4 days old -- excluded even though
	// the destination side is fresh.
	// Type 38: destination figures 4 days old -- excluded even
	// though the origin side is fresh.
	// Type 39: margin 2% -- under the 5% default floor.
	originSeeds := []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfOrigin, 34, 0, 4, 0, 50, 0, now),
		tfRegionStat(tfOrigin, 35, 0, 90, 0, 5, 0, now),
		tfRegionStat(tfOrigin, 36, 0, 4, 0, 0, 0, now),
		tfRegionStat(tfOrigin, 37, 0, 4, 0, 50, 0, stale),
		tfRegionStat(tfOrigin, 38, 0, 4, 0, 50, 0, now),
		tfRegionStat(tfOrigin, 39, 0, 100, 0, 50, 0, now),
	}
	destSeeds := []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfDest, 34, 0, 10, 0, 100, 20, now),
		tfRegionStat(tfDest, 35, 0, 100, 0, 10, 100, now),
		tfRegionStat(tfDest, 36, 0, 10, 0, 100, 100, now),
		tfRegionStat(tfDest, 37, 0, 10, 0, 100, 100, now),
		tfRegionStat(tfDest, 38, 0, 10, 0, 100, 100, stale),
		tfRegionStat(tfDest, 39, 0, 102, 0, 100, 100, now),
	}
	for _, s := range append(originSeeds, destSeeds...) {
		if err := q.UpsertMarketRegionStat(ctx, s); err != nil {
			t.Fatalf("seed region stat: %v", err)
		}
	}

	// Hints: type 34 sells cheapest at the second Jita station in
	// the origin, and the best buyer in the destination sits at
	// the Amarr station. Type 35 has no station rows at all.
	for _, s := range []db.UpsertMarketStationStatParams{
		{LocationID: 60003760, RegionID: tfOrigin, TypeID: 34, BestSell: 5.5, BestBuy: 0, SellOrders: 1, SellVolume: 10, UpdatedAt: now},
		{LocationID: 60003761, RegionID: tfOrigin, TypeID: 34, BestSell: 5.0, BestBuy: 0, SellOrders: 1, SellVolume: 10, UpdatedAt: now},
		{LocationID: 60008494, RegionID: tfDest, TypeID: 34, BestSell: 0, BestBuy: 9.0, BuyOrders: 1, BuyVolume: 10, UpdatedAt: now},
	} {
		if err := q.UpsertMarketStationStat(ctx, s); err != nil {
			t.Fatalf("seed station stat: %v", err)
		}
	}

	view := tradefinder.BuildView(ctx, app, url.Values{})
	if !view.HasData() {
		t.Fatalf("tradefinder HasData false, want true (origin %v, dest %v)", view.OriginHasData, view.DestHasData)
	}
	if len(view.Rows) != 2 {
		t.Fatalf("tradefinder rows: %d, want 2 (34 and 35 only): %+v", len(view.Rows), view.Rows)
	}
	top := view.Rows[0]
	if top.TypeID != 34 || top.MarginPct != "150.0%" || top.SoldPerDay != "20" || top.ProfitDay != esi.FormatISK(120) || top.ProfitItem != esi.FormatISK(6) {
		t.Fatalf("top route: %+v, want type 34, 150%%, 20/day, %s/day, %s/item", top, esi.FormatISK(120), esi.FormatISK(6))
	}
	if view.Rows[1].TypeID != 35 || view.Rows[1].ProfitDay != esi.FormatISK(50) {
		t.Fatalf("second route: %+v, want type 35 capped by the origin's stock at %s/day", view.Rows[1], esi.FormatISK(50))
	}
	if top.OriginStation.Name == "" || top.OriginBest != esi.FormatISK(5.0) {
		t.Fatalf("cheapest hint: %+v %q, want the 5.0 Jita station", top.OriginStation, top.OriginBest)
	}
	if !strings.Contains(top.OriginStation.Name, "Moon 10") {
		t.Fatalf("cheapest hint station: %q, want the cheaper Moon 10 station", top.OriginStation.Name)
	}
	if top.DestStation.Name == "" || top.DestBest != esi.FormatISK(9.0) {
		t.Fatalf("best buyer hint: %+v %q, want the 9.0 Amarr station", top.DestStation, top.DestBest)
	}
	if view.Rows[1].OriginBest != "" || view.Rows[1].DestBest != "" {
		t.Fatalf("type 35 hints: %q/%q, want none seeded", view.Rows[1].OriginBest, view.Rows[1].DestBest)
	}
	if view.OriginAsOf == "" || view.DestAsOf == "" {
		t.Fatalf("freshness lines empty: %q / %q", view.OriginAsOf, view.DestAsOf)
	}

	// Filters: a 200% margin floor drops everything; a sold-per
	// -day floor of 50 keeps only Pyerite (100/day); a 1% margin
	// floor admits the 2% Zydrine route.
	view = tradefinder.BuildView(ctx, app, url.Values{"minmargin": {"200"}})
	if len(view.Rows) != 0 {
		t.Fatalf("200%% margin floor rows: %d, want 0", len(view.Rows))
	}
	view = tradefinder.BuildView(ctx, app, url.Values{"minvol": {"50"}})
	if len(view.Rows) != 1 || view.Rows[0].TypeID != 35 {
		t.Fatalf("minvol 50 rows: %+v, want only type 35", view.Rows)
	}
	view = tradefinder.BuildView(ctx, app, url.Values{"minmargin": {"1"}})
	found39 := false
	for _, r := range view.Rows {
		if r.TypeID == 39 {
			found39 = true
		}
	}
	if !found39 {
		t.Fatalf("1%% margin floor rows: %+v, want the 2%% type 39 route admitted", view.Rows)
	}

	// Same region on both ends: valid, but no routes by design.
	view = tradefinder.BuildView(ctx, app, url.Values{"origin": {"10000002"}, "destination": {"10000002"}})
	if !view.SameRegion || len(view.Rows) != 0 {
		t.Fatalf("same-region view: same %v, rows %d, want same with 0 rows", view.SameRegion, len(view.Rows))
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder build made %d outbound calls, want 0", got)
	}
}

func TestTradefinderCap(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	conn := app.db
	for i := int64(0); i < 105; i++ {
		typeID := int64(1000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Cap Item %d", i)); err != nil {
			t.Fatalf("seed cap type: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, typeID, 0, 10, 0, 1000, 0, now)); err != nil {
			t.Fatalf("seed cap origin stat: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, typeID, 0, 20, 0, 1000, 10, now)); err != nil {
			t.Fatalf("seed cap dest stat: %v", err)
		}
	}
	view := tradefinder.BuildView(ctx, app, url.Values{})
	if len(view.Rows) != tradefinder.RowCap {
		t.Fatalf("capped tradefinder rows: %d, want %d", len(view.Rows), tradefinder.RowCap)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder cap build made %d outbound calls, want 0", got)
	}
}

func TestTradefinderPageRendersStoredRowsZeroOutbound(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 34, 0, 4, 0, 50, 0, now)); err != nil {
		t.Fatalf("seed origin stat: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, 34, 0, 10, 0, 100, 20, now)); err != nil {
		t.Fatalf("seed dest stat: %v", err)
	}
	if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
		LocationID: 60003761, RegionID: tfOrigin, TypeID: 34, BestSell: 5.0, SellOrders: 1, SellVolume: 10, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed origin station stat: %v", err)
	}
	if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
		LocationID: 60008494, RegionID: tfDest, TypeID: 34, BestBuy: 9.0, BuyOrders: 1, BuyVolume: 10, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed dest station stat: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/tradefinder/")
	if code != http.StatusOK {
		t.Fatalf("tradefinder page: status %d", code)
	}
	mustContain(t, "/market/tradefinder/", body,
		"Trade Finder",
		"Buy in",
		"Sell in",
		"Tritanium",
		"Profit per item",
		"Sold per day in Domain",
		"Estimated profit per day",
		"Cheapest at",
		"Best buyer at",
		"figures last gathered",
		"older than 3 days are left out",
		"/market/?type=34",
		"region=10000043",
		"/station/?station=60003761",
		"/station/?station=60008494",
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder page made %d outbound calls, want 0", got)
	}
}

func TestTradefinderLowballRoutesOptIn(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()

	// Type 40 is not in the SDE fixture; the stats are what the
	// view judges, but the name keeps the row readable.
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (40, 'Degenerate Coating', 18)`); err != nil {
		t.Fatalf("seed lowball type: %v", err)
	}

	// Type 34: a sane route -- origin typical sell 5,
	// destination typical sell 10.
	// Type 40: the long shot -- origin typical sell 1.5 against a
	// destination typical sell of 250M, far past ten times.
	for _, s := range []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfOrigin, 34, 4, 5, 0, 50, 0, now),
		tfRegionStat(tfDest, 34, 0, 10, 0, 100, 20, now),
		tfRegionStat(tfOrigin, 40, 1, 1.5, 0, 100, 0, now),
		tfRegionStat(tfDest, 40, 0, 250_000_000, 0, 100, 10, now),
	} {
		if err := q.UpsertMarketRegionStat(ctx, s); err != nil {
			t.Fatalf("seed region stat: %v", err)
		}
	}

	routeIDs := func(view *tradefinder.View) map[int64]bool {
		ids := make(map[int64]bool, len(view.Rows))
		for _, r := range view.Rows {
			ids[r.TypeID] = true
		}
		return ids
	}

	// Default: the degenerate route is hidden, the sane one shows.
	view := tradefinder.BuildView(ctx, app, url.Values{})
	if view.Lowball {
		t.Error("default view has lowball routes on, want off")
	}
	if ids := routeIDs(view); !ids[34] || ids[40] {
		t.Fatalf("default routes: %v, want type 34 only", ids)
	}

	// Opted in: both routes list, and the flag echoes for the form.
	view = tradefinder.BuildView(ctx, app, url.Values{"lowball": {"1"}})
	if !view.Lowball {
		t.Error("lowball=1 view dropped the opt-in flag")
	}
	if ids := routeIDs(view); !ids[34] || !ids[40] {
		t.Fatalf("lowball routes: %v, want types 34 and 40", ids)
	}

	// An explicit 0 is off, like an unchecked box.
	view = tradefinder.BuildView(ctx, app, url.Values{"lowball": {"0"}})
	if view.Lowball {
		t.Error("lowball=0 view has lowball routes on, want off")
	}

	// The form carries the checkbox; the checked state survives a
	// submit with the option on.
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/tradefinder/")
	if code != http.StatusOK {
		t.Fatalf("tradefinder page: status %d", code)
	}
	if !strings.Contains(body, `name="lowball"`) {
		t.Error("tradefinder form has no lowball checkbox")
	}
	if strings.Contains(body, `name="lowball" value="1" checked`) {
		t.Error("lowball checkbox renders checked without the option")
	}
	code, body = getPage(t, app, cookie, "/market/tradefinder/?lowball=1")
	if code != http.StatusOK {
		t.Fatalf("tradefinder lowball page: status %d", code)
	}
	if !strings.Contains(body, `name="lowball" value="1" checked`) {
		t.Error("lowball checkbox loses its checked state after submit")
	}
	if !strings.Contains(body, "Degenerate Coating") {
		t.Error("lowball page omits the degenerate route")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("lowball views made %d outbound calls, want 0", got)
	}
}

func TestTradefinderPageEmptyStates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// No sweep anywhere yet: the still-gathering state.
	code, body := getPage(t, app, cookie, "/market/tradefinder/")
	if code != http.StatusOK {
		t.Fatalf("tradefinder empty page: status %d", code)
	}
	mustContain(t, "/market/tradefinder/ (no sweep)", body,
		"Trade Finder",
		"Still gathering prices",
	)
	if strings.Contains(body, "Estimated profit per day") {
		t.Error("still-gathering page shows a results table before any sweep")
	}

	// Both regions swept, but nothing clears the filters: the
	// distinct no-routes state.
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 34, 0, 100, 0, 50, 0, now)); err != nil {
		t.Fatalf("seed origin stat: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, 34, 0, 101, 0, 100, 20, now)); err != nil {
		t.Fatalf("seed dest stat: %v", err)
	}
	code, body = getPage(t, app, cookie, "/market/tradefinder/")
	if code != http.StatusOK {
		t.Fatalf("tradefinder no-routes page: status %d", code)
	}
	mustContain(t, "/market/tradefinder/ (no routes)", body,
		"Trade Finder",
		"No profitable routes between these regions right now",
	)
	if strings.Contains(body, "Still gathering prices") {
		t.Error("no-routes page falls back to the still-gathering state despite swept regions")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder empty pages made %d outbound calls, want 0", got)
	}
}

func TestTradefinderMarginRankingAndCapInSQL(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	conn := app.db

	// 105 qualifying pairs with strictly increasing margin:
	// origin typical sell 100, destination typical sell 200+i.
	// units/day = min(10, 1000) = 10, so profit grows
	// strictly with the type id: the single bounded query must
	// return exactly the top 100 in margin-descending order.
	for i := int64(0); i < 105; i++ {
		typeID := int64(2000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Rank Item %d", i)); err != nil {
			t.Fatalf("seed rank type: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, typeID, 0, 100, 0, 1000, 0, now)); err != nil {
			t.Fatalf("seed rank origin stat: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, typeID, 0, 200+float64(i), 0, 1000, 10, now)); err != nil {
			t.Fatalf("seed rank dest stat: %v", err)
		}
	}

	view := tradefinder.BuildView(ctx, app, url.Values{})
	if !view.HasData() {
		t.Fatal("tradefinder has no data after seeding 105 pairs")
	}
	if len(view.Rows) != tradefinder.RowCap {
		t.Fatalf("tradefinder rows: %d, want %d", len(view.Rows), tradefinder.RowCap)
	}
	for i, row := range view.Rows {
		if want := int64(2104 - i); row.TypeID != want {
			t.Fatalf("row %d: type %d, want %d (margin-desc order)", i, row.TypeID, want)
		}
	}
	// Top route: margin 204/item, 10/day -> 2040/day.
	if view.Rows[0].ProfitItem != esi.FormatISK(204) || view.Rows[0].ProfitDay != esi.FormatISK(2040) {
		t.Fatalf("top route: %s/item %s/day, want %s/item %s/day",
			view.Rows[0].ProfitItem, view.Rows[0].ProfitDay, esi.FormatISK(204), esi.FormatISK(2040))
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder build made %d outbound calls, want 0", got)
	}
}

// TestTradefinderPendingRoutesQueueHistory: a route whose destination
// has no recorded sales yet is not dropped. It lists as pending, a
// signed-in view queues the destination history for it, and once that
// history lands the route ranks into the table.
func TestTradefinderPendingRoutesQueueHistory(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	// Pyerite clears the price rules but Domain has no sold-per-day figure.
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 35, 0, 4, 0, 50, 0, now)); err != nil {
		t.Fatalf("seed origin stat: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, 35, 0, 10, 0, 100, 0, now)); err != nil {
		t.Fatalf("seed dest stat: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/market/tradefinder/")
	mustContain(t, "/market/tradefinder/ (pending)", body,
		"Waiting on sales figures", "Pyerite", "No route has sales figures in Domain yet")
	var wants int
	if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_history_wants WHERE region_id = $1 AND type_id = 35`, tfDest).Scan(&wants); err != nil {
		t.Fatalf("count wants: %v", err)
	}
	if wants != 1 {
		t.Fatalf("history wants for the pending route = %d, want 1", wants)
	}

	// The history arrives (what fetchAndStoreHistory stores and then
	// refreshes): the route moves into the ranked table.
	for i, vol := range []int64{30, 40, 50} {
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: tfDest, TypeID: 35, Date: now.AddDate(0, 0, -i).Format("2006-01-02"),
			Average: 10, Highest: 10, Lowest: 10, Volume: vol, OrderCount: 1,
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	if err := q.RefreshMarketRegionStatAvgVolume(ctx, db.RefreshMarketRegionStatAvgVolumeParams{RegionID: tfDest, TypeID: 35}); err != nil {
		t.Fatalf("refresh sold per day: %v", err)
	}
	_, body = getPage(t, app, cookie, "/market/tradefinder/")
	mustContain(t, "/market/tradefinder/ (ranked)", body, "Pyerite", "Estimated profit per day")
	if strings.Contains(body, "Waiting on sales figures") {
		t.Error("the route still lists as pending after its history landed")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder made %d outbound calls, want 0", got)
	}
}

// TestTradefinderHealsPendingWithStoredHistory: a pending route whose
// destination history is already stored -- it arrived by a path that
// did not update the sold-per-day figure (a swarm peer, or before the
// figure was kept current) -- ranks into the table on the next view,
// from stored rows alone.
func TestTradefinderHealsPendingWithStoredHistory(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedTradefinderFixture(t, app)
	now := time.Now().UTC()
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 36, 0, 4, 0, 50, 0, now)); err != nil {
		t.Fatalf("seed origin stat: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, 36, 0, 10, 0, 100, 0, now)); err != nil {
		t.Fatalf("seed dest stat: %v", err)
	}
	// History is stored, but the figure was never refreshed.
	for i, vol := range []int64{20, 30, 40} {
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: tfDest, TypeID: 36, Date: now.AddDate(0, 0, -i).Format("2006-01-02"),
			Average: 10, Highest: 10, Lowest: 10, Volume: vol, OrderCount: 1,
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/market/tradefinder/")
	mustContain(t, "/market/tradefinder/ (healed)", body, "Mexallon", "Estimated profit per day")
	if strings.Contains(body, "Waiting on sales figures") {
		t.Error("a route with stored history still lists as pending")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder made %d outbound calls, want 0", got)
	}
}

// TestTradefinderGlobalMarketFill: an endpoint region with no
// stored row for a type reads the type's global-market row
// instead (the global book is that type's own market), and a
// region row always wins where both exist. A type holding only
// a global row prices both ends from the same book, so it forms
// no route at all.
func TestTradefinderGlobalMarketFill(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedTradefinderFixture(t, app)
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (40, 'Globalium', 18), (41, 'Bothbooks', 18), (42, 'Globex', 18), (43, 'Pendium', 18)`); err != nil {
		t.Fatalf("seed sde types: %v", err)
	}
	now := time.Now().UTC()
	seeds := []db.UpsertMarketRegionStatParams{
		// Type 40: no Forge row -- the global book is its origin
		// book. Margin 150-90=60 (66.7%); units min(25/day, 40
		// on sale) = 25 -> 1500/day.
		tfRegionStat(globalMarketRegion, 40, 0, 90, 0, 40, 30, now),
		tfRegionStat(tfDest, 40, 0, 150, 0, 100, 25, now),
		// Type 41: both books at the origin -- The Forge row
		// (100, 10 on sale) wins over the global row (40, 999).
		// Margin 30 (30.0%); units min(10/day, 10) = 10 -> 300/day.
		tfRegionStat(tfOrigin, 41, 0, 100, 0, 10, 0, now),
		tfRegionStat(globalMarketRegion, 41, 0, 40, 0, 999, 0, now),
		tfRegionStat(tfDest, 41, 0, 130, 0, 100, 10, now),
		// Type 42: the global book only, at both ends -- the
		// same book prices both sides, so no route can form.
		tfRegionStat(globalMarketRegion, 42, 0, 90, 0, 40, 30, now),
		// Type 43: the origin fills from the global book and the
		// destination has a price but no sold-per-day figure
		// yet: a pending route priced from the global row.
		tfRegionStat(globalMarketRegion, 43, 0, 80, 0, 10, 0, now),
		tfRegionStat(tfDest, 43, 0, 120, 0, 100, 0, now),
	}
	for _, s := range seeds {
		if err := q.UpsertMarketRegionStat(ctx, s); err != nil {
			t.Fatalf("seed region stat: %v", err)
		}
	}

	view := tradefinder.BuildView(ctx, app, url.Values{})
	if !view.HasData() {
		t.Fatalf("tradefinder HasData false, want true (origin %v, dest %v)", view.OriginHasData, view.DestHasData)
	}
	if len(view.Rows) != 2 {
		t.Fatalf("tradefinder rows: %d, want 2 (40 and 41 only): %+v", len(view.Rows), view.Rows)
	}
	top := view.Rows[0]
	if top.TypeID != 40 || top.BuyTypical != esi.FormatISK(90) || top.SellTypical != esi.FormatISK(150) ||
		top.MarginPct != "66.7%" || top.SoldPerDay != "25" || top.ProfitDay != esi.FormatISK(1500) {
		t.Fatalf("global-fill route: %+v, want type 40 bought at the global %s, sold at %s, 66.7%%, 25/day, %s/day",
			top, esi.FormatISK(90), esi.FormatISK(150), esi.FormatISK(1500))
	}
	second := view.Rows[1]
	if second.TypeID != 41 || second.BuyTypical != esi.FormatISK(100) || second.ProfitItem != esi.FormatISK(30) ||
		second.ProfitDay != esi.FormatISK(300) {
		t.Fatalf("hub-wins route: %+v, want type 41 bought at The Forge's %s (not the global %s), %s/item, %s/day",
			second, esi.FormatISK(100), esi.FormatISK(40), esi.FormatISK(30), esi.FormatISK(300))
	}
	if len(view.Pending) != 1 || view.Pending[0].TypeID != 43 ||
		view.Pending[0].BuyTypical != esi.FormatISK(80) || view.Pending[0].SellTypical != esi.FormatISK(120) {
		t.Fatalf("pending routes: %+v, want only type 43 priced from the global book at %s -> %s",
			view.Pending, esi.FormatISK(80), esi.FormatISK(120))
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder build made %d outbound calls, want 0", got)
	}
}
