package app

// P3 Tradefinder tests: region-pair margin math, the three-day
// freshness rule on either side, the volume-cap formula, filters,
// sort and cap, best-price hints, and page renders proven to stay
// on stored rows with zero outbound calls.

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

func tfRegionStat(regionID, typeID int64, typicalBuy, typicalSell float64, buyVolume, sellVolume int64, avgDailyVolume float64, stamp string) db.UpsertMarketRegionStatParams {
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
	now := nowT.Format(time.RFC3339)
	stale := nowT.Add(-4 * 24 * time.Hour).Format(time.RFC3339)

	// Type 34: margin 10-4=6, 150%; units min(20/day, origin buy
	// 50, dest sell 100) = 20 -> 120/day. The headline route.
	// Type 35: margin 100-90=10, 11.1%; units min(100, 5, 10) = 5
	// -> 50/day (origin buy volume is the cap that bites).
	// Type 36: healthy margin but the destination has no open
	// sell volume -- cannot move, drops out.
	// Type 37: origin figures 4 days old -- excluded even though
	// the destination side is fresh.
	// Type 38: destination figures 4 days old -- excluded even
	// though the origin side is fresh.
	// Type 39: margin 2% -- under the 5% default floor.
	originSeeds := []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfOrigin, 34, 4, 0, 50, 0, 0, now),
		tfRegionStat(tfOrigin, 35, 90, 0, 5, 0, 0, now),
		tfRegionStat(tfOrigin, 36, 4, 0, 50, 0, 0, now),
		tfRegionStat(tfOrigin, 37, 4, 0, 50, 0, 0, stale),
		tfRegionStat(tfOrigin, 38, 4, 0, 50, 0, 0, now),
		tfRegionStat(tfOrigin, 39, 100, 0, 50, 0, 0, now),
	}
	destSeeds := []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfDest, 34, 0, 10, 0, 100, 20, now),
		tfRegionStat(tfDest, 35, 0, 100, 0, 10, 100, now),
		tfRegionStat(tfDest, 36, 0, 10, 0, 0, 100, now),
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

	view := app.buildTradefinderView(ctx, url.Values{})
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
		t.Fatalf("second route: %+v, want type 35 capped by origin buy volume at %s/day", view.Rows[1], esi.FormatISK(50))
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
	view = app.buildTradefinderView(ctx, url.Values{"minmargin": {"200"}})
	if len(view.Rows) != 0 {
		t.Fatalf("200%% margin floor rows: %d, want 0", len(view.Rows))
	}
	view = app.buildTradefinderView(ctx, url.Values{"minvol": {"50"}})
	if len(view.Rows) != 1 || view.Rows[0].TypeID != 35 {
		t.Fatalf("minvol 50 rows: %+v, want only type 35", view.Rows)
	}
	view = app.buildTradefinderView(ctx, url.Values{"minmargin": {"1"}})
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
	view = app.buildTradefinderView(ctx, url.Values{"origin": {"10000002"}, "destination": {"10000002"}})
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
	now := time.Now().UTC().Format(time.RFC3339)
	conn := app.db
	for i := int64(0); i < 105; i++ {
		typeID := int64(1000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Cap Item %d", i)); err != nil {
			t.Fatalf("seed cap type: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, typeID, 10, 0, 1000, 0, 0, now)); err != nil {
			t.Fatalf("seed cap origin stat: %v", err)
		}
		if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfDest, typeID, 0, 20, 0, 1000, 10, now)); err != nil {
			t.Fatalf("seed cap dest stat: %v", err)
		}
	}
	view := app.buildTradefinderView(ctx, url.Values{})
	if len(view.Rows) != tradefinderRowCap {
		t.Fatalf("capped tradefinder rows: %d, want %d", len(view.Rows), tradefinderRowCap)
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
	now := time.Now().UTC().Format(time.RFC3339)
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 34, 4, 0, 50, 0, 0, now)); err != nil {
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
		"Tradefinder",
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
	now := time.Now().UTC().Format(time.RFC3339)

	// Type 40 is not in the SDE fixture; the stats are what the
	// view judges, but the name keeps the row readable.
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (40, 'Degenerate Coating', 18)`); err != nil {
		t.Fatalf("seed lowball type: %v", err)
	}

	// Type 34: a sane route -- origin typical buy 4 against a
	// typical sell of 5, destination typical sell 10.
	// Type 40: the degenerate one -- origin typical buy 1.5
	// against a typical sell of 240M (a buy book of lowball
	// orders), destination typical sell 250M.
	for _, s := range []db.UpsertMarketRegionStatParams{
		tfRegionStat(tfOrigin, 34, 4, 5, 50, 0, 0, now),
		tfRegionStat(tfDest, 34, 0, 10, 0, 100, 20, now),
		tfRegionStat(tfOrigin, 40, 1.5, 240_000_000, 100, 0, 0, now),
		tfRegionStat(tfDest, 40, 0, 250_000_000, 0, 100, 10, now),
	} {
		if err := q.UpsertMarketRegionStat(ctx, s); err != nil {
			t.Fatalf("seed region stat: %v", err)
		}
	}

	routeIDs := func(view *tradefinderView) map[int64]bool {
		ids := make(map[int64]bool, len(view.Rows))
		for _, r := range view.Rows {
			ids[r.TypeID] = true
		}
		return ids
	}

	// Default: the degenerate route is hidden, the sane one shows.
	view := app.buildTradefinderView(ctx, url.Values{})
	if view.Lowball {
		t.Error("default view has lowball routes on, want off")
	}
	if ids := routeIDs(view); !ids[34] || ids[40] {
		t.Fatalf("default routes: %v, want type 34 only", ids)
	}

	// Opted in: both routes list, and the flag echoes for the form.
	view = app.buildTradefinderView(ctx, url.Values{"lowball": {"1"}})
	if !view.Lowball {
		t.Error("lowball=1 view dropped the opt-in flag")
	}
	if ids := routeIDs(view); !ids[34] || !ids[40] {
		t.Fatalf("lowball routes: %v, want types 34 and 40", ids)
	}

	// An explicit 0 is off, like an unchecked box.
	view = app.buildTradefinderView(ctx, url.Values{"lowball": {"0"}})
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
		"Tradefinder",
		"Still gathering prices",
	)
	if strings.Contains(body, "Estimated profit per day") {
		t.Error("still-gathering page shows a results table before any sweep")
	}

	// Both regions swept, but nothing clears the filters: the
	// distinct no-routes state.
	seedTradefinderFixture(t, app)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := q.UpsertMarketRegionStat(ctx, tfRegionStat(tfOrigin, 34, 100, 0, 50, 0, 0, now)); err != nil {
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
		"Tradefinder",
		"No profitable routes between these regions right now",
	)
	if strings.Contains(body, "Still gathering prices") {
		t.Error("no-routes page falls back to the still-gathering state despite swept regions")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("tradefinder empty pages made %d outbound calls, want 0", got)
	}
}
