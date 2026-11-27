package app

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedMoverType(t *testing.T, app *Application, typeID int64, name string) {
	t.Helper()
	if _, err := app.db.ExecContext(context.Background(),
		`INSERT INTO sde_types (type_id, name, group_id, published) VALUES ($1, $2, 1, 1)
		 ON CONFLICT (type_id) DO UPDATE SET name = excluded.name`, typeID, name); err != nil {
		t.Fatalf("seed sde_types %d: %v", typeID, err)
	}
}

// TestTrendsIngestRankingAndWhatChangedFilter verifies the full ingest-to-UI
// path for volume-confirmed movers, rolling 7-day averages, ISK/trade-count
// floors (100M Forge / 20M other hubs), cross-hub confirmation, spread-profit
// and ISK-move sorts, raw % warning signs, and the filtered What changed feed.
func TestTrendsIngestRankingAndWhatChangedFilter(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	const (
		forge  int64 = 10000002
		domain int64 = 10000043

		typeConfirmedMulti int64 = 910001 // passes all gates in Forge + rises in Domain
		typeConfirmedOne   int64 = 910002 // passes all gates in Forge only
		typeManipulated    int64 = 910003 // thin spike, widening spread, low trades, 1 hub
		typeShortHold      int64 = 910004 // passes ISK + trades + vol, but only 3 of 7 days held
		typeFallingVol     int64 = 910005 // passes ISK + trades + hold, but 7d vol < 30d vol
		typeSpreadLeader   int64 = 910006 // passes gates with largest daily spread profit
		typeDomainOnly     int64 = 910007 // 35M ISK/day in Domain (clears 20M hub default)
	)
	seedMoverType(t, app, typeConfirmedMulti, "Alpha Confirmed Multi")
	seedMoverType(t, app, typeConfirmedOne, "Beta Confirmed Single")
	seedMoverType(t, app, typeManipulated, "Gamma Manipulated Spike")
	seedMoverType(t, app, typeShortHold, "Delta Short Hold")
	seedMoverType(t, app, typeFallingVol, "Epsilon Low Vol Ratio")
	seedMoverType(t, app, typeSpreadLeader, "Zeta Spread Leader")
	seedMoverType(t, app, typeDomainOnly, "Eta Domain Mover")

	// Mark other hub regions fresh so the sweep touches only The Forge and Domain.
	for _, region := range marketRegions {
		if region.ID != forge && region.ID != domain {
			markRegionFresh(t, q, region.ID)
		}
	}

	now := time.Now().UTC()
	dayStr := func(offset int) string { return now.AddDate(0, 0, -offset).Format("2006-01-02") }

	seedDaily := func(regionID, typeID int64, offset int, sell, buy float64) {
		t.Helper()
		if err := q.UpsertMarketRegionStatDaily(ctx, db.UpsertMarketRegionStatDailyParams{
			RegionID: regionID, TypeID: typeID, Day: dayStr(offset),
			TypicalSell: sell, BestSell: sell, TypicalBuy: buy, BestBuy: buy,
			SellOrders: 10, BuyOrders: 10, SellVolume: 10000, BuyVolume: 10000,
		}); err != nil {
			t.Fatalf("seed daily (%d,%d,-%d): %v", regionID, typeID, offset, err)
		}
	}
	seedHist := func(regionID, typeID int64, offset int, avg float64, vol, orders int64) {
		t.Helper()
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: regionID, TypeID: typeID, Date: dayStr(offset),
			Average: avg, Highest: avg, Lowest: avg, Volume: vol, OrderCount: orders,
		}); err != nil {
			t.Fatalf("seed history (%d,%d,-%d): %v", regionID, typeID, offset, err)
		}
	}

	// 1. Alpha Confirmed Multi (Forge + Domain): baseline 100, last 5 history days 140 (+40%),
	//    7d vol 2,000,000 * 140 = 280M ISK/day, 30 trades/day, 7d vol > 30d vol.
	for d := 29; d >= 1; d-- {
		p := 100.0
		vol := int64(1_000_000)
		if d <= 5 {
			p = 140.0
			vol = 2_000_000
		}
		seedDaily(forge, typeConfirmedMulti, d, p, p*0.95)
		seedHist(forge, typeConfirmedMulti, d, p, vol, 30)

		dp := 100.0
		if d <= 5 {
			dp = 125.0
		}
		seedDaily(domain, typeConfirmedMulti, d, dp, dp*0.95)
		seedHist(domain, typeConfirmedMulti, d, dp, 300_000, 15)
	}

	// 2. Beta Confirmed Single (Forge only): baseline 100, last 5 history days 120 (+20%),
	//    200M ISK/day, 20 trades/day; flat in Domain.
	for d := 29; d >= 1; d-- {
		p := 100.0
		vol := int64(1_500_000)
		if d <= 5 {
			p = 120.0
			vol = 1_700_000
		}
		seedDaily(forge, typeConfirmedOne, d, p, p*0.96)
		seedHist(forge, typeConfirmedOne, d, p, vol, 20)
		seedDaily(domain, typeConfirmedOne, d, 100.0, 95.0)
		seedHist(domain, typeConfirmedOne, d, 100.0, 100_000, 12)
	}

	// 3. Gamma Manipulated Spike (Forge only): baseline 100 (5% spread),
	//    spikes to 600 today (buy stays 100 -> 500% spread, widened > 2x),
	//    tiny history (1M ISK/day, 2 trades/day).
	for d := 29; d >= 1; d-- {
		p := 100.0
		if d <= 6 {
			p = 400.0
		}
		seedDaily(forge, typeManipulated, d, p, 95.0)
		seedHist(forge, typeManipulated, d, 100.0, 10_000, 2)
	}

	// 4. Delta Short Hold (Forge): baseline 100, only 3 days (-1, -2, -3) at 150
	//    (and missing days -4..-7 in history so median_7d = 150 but hold_days = 3 < 4).
	for d := 29; d >= 8; d-- {
		seedDaily(forge, typeShortHold, d, 100.0, 95.0)
		seedHist(forge, typeShortHold, d, 100.0, 1_500_000, 25)
	}
	for d := 3; d >= 1; d-- {
		seedDaily(forge, typeShortHold, d, 150.0, 140.0)
		seedHist(forge, typeShortHold, d, 150.0, 3_000_000, 40)
	}

	// 5. Epsilon Low Vol Ratio (Forge): baseline 100, last 5 days 135 (+35%),
	//    passes ISK (135M) and trades (20/d) and hold (5d), but 7d vol (1M) < 30d vol (3M).
	for d := 29; d >= 1; d-- {
		p := 100.0
		vol := int64(4_000_000)
		if d <= 5 {
			p = 135.0
		}
		if d <= 7 {
			vol = 1_000_000
		}
		seedDaily(forge, typeFallingVol, d, p, p*0.95)
		seedHist(forge, typeFallingVol, d, p, vol, 20)
	}

	// 6. Zeta Spread Leader (Forge): modest +10% move (100 -> 110) over 5 days,
	//    180M ISK/day, 25 trades/day, wide profitable spread (sell 110, buy 80) and deep book.
	for d := 29; d >= 1; d-- {
		p := 100.0
		if d <= 5 {
			p = 110.0
		}
		seedDaily(forge, typeSpreadLeader, d, p, 80.0)
		seedHist(forge, typeSpreadLeader, d, p, 1_700_000, 25)
	}

	// 7. Eta Domain Mover (Domain only): 35M ISK/day (clears Domain's 20M default),
	//    15 trades/day, baseline 100 -> 130 for 5 days.
	for d := 29; d >= 1; d-- {
		p := 100.0
		if d <= 5 {
			p = 130.0
		}
		seedDaily(domain, typeDomainOnly, d, p, p*0.95)
		seedHist(domain, typeDomainOnly, d, p, 300_000, 15)
	}

	makeOrders := func(baseID, typeID int64, sell, buy float64, vol int64) []esi.MarketOrder {
		var out []esi.MarketOrder
		for i := int64(0); i < 6; i++ {
			out = append(out,
				esi.MarketOrder{OrderID: baseID + i, TypeID: typeID, Price: sell, VolumeRemain: vol},
				esi.MarketOrder{OrderID: baseID + 50 + i, TypeID: typeID, IsBuyOrder: true, Price: buy, VolumeRemain: vol},
			)
		}
		return out
	}

	var forgeBook []esi.MarketOrder
	forgeBook = append(forgeBook, makeOrders(1000, typeConfirmedMulti, 140, 133, 500_000)...)
	forgeBook = append(forgeBook, makeOrders(2000, typeConfirmedOne, 120, 115, 500_000)...)
	forgeBook = append(forgeBook, makeOrders(3000, typeManipulated, 600, 100, 5_000)...)
	forgeBook = append(forgeBook, makeOrders(4000, typeShortHold, 150, 140, 500_000)...)
	forgeBook = append(forgeBook, makeOrders(5000, typeFallingVol, 135, 128, 500_000)...)
	forgeBook = append(forgeBook, makeOrders(6000, typeSpreadLeader, 110, 80, 1_000_000)...)
	transport.books[forge] = [][]esi.MarketOrder{forgeBook}

	var domainBook []esi.MarketOrder
	domainBook = append(domainBook, makeOrders(7000, typeConfirmedMulti, 125, 118, 200_000)...)
	domainBook = append(domainBook, makeOrders(8000, typeConfirmedOne, 100, 95, 200_000)...)
	domainBook = append(domainBook, makeOrders(9000, typeDomainOnly, 130, 123, 200_000)...)
	transport.books[domain] = [][]esi.MarketOrder{domainBook}

	sweepUntilIdle(t, app)

	// Verify stored ingest metrics directly.
	stAlpha, err := q.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{RegionID: forge, TypeID: typeConfirmedMulti})
	if err != nil {
		t.Fatalf("get alpha stat: %v", err)
	}
	if stAlpha.ConfirmedHubs < 1 {
		t.Errorf("alpha confirmed_hubs = %d, want >= 1", stAlpha.ConfirmedHubs)
	}
	if stAlpha.MoverHoldDays < 4 {
		t.Errorf("alpha mover_hold_days = %d, want >= 4", stAlpha.MoverHoldDays)
	}
	if stAlpha.IskTraded7d < 100_000_000 {
		t.Errorf("alpha isk_traded_7d = %.0f, want >= 100M", stAlpha.IskTraded7d)
	}
	if !stAlpha.MoverScore.Valid || stAlpha.MoverScore.Float64 <= 0 {
		t.Errorf("alpha mover_score = %+v, want > 0", stAlpha.MoverScore)
	}
	if stAlpha.SellMedian7d != 140 || stAlpha.SellMedian30d != 100 {
		t.Errorf("alpha history medians = (7d=%.2f, 30d=%.2f), want (140, 100)", stAlpha.SellMedian7d, stAlpha.SellMedian30d)
	}
	wantScore := math.Abs(stAlpha.MoverChg7v30.Float64) * math.Log1p(stAlpha.IskTraded7d)
	if math.Abs(stAlpha.MoverScore.Float64-wantScore) > 1e-6 {
		t.Errorf("alpha mover_score = %v, want %v", stAlpha.MoverScore.Float64, wantScore)
	}
	// Watchlist watchMove reads the same stored mover_chg_7v30 change.
	wm := app.watchMove(ctx, typeConfirmedMulti, forge)
	if !wm.Has7 || math.Abs(wm.Chg7-40.0) > 1e-6 {
		t.Errorf("watchMove(alpha) = %+v, want Has7=true Chg7=40.0", wm)
	}

	stGamma, err := q.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{RegionID: forge, TypeID: typeManipulated})
	if err != nil {
		t.Fatalf("get gamma stat: %v", err)
	}
	if !stGamma.SpreadWidened {
		t.Errorf("gamma spread_widened = false, want true (spread_pct=%+v)", stGamma.SpreadPct)
	}
	if stGamma.ConfirmedHubs != 0 {
		t.Errorf("gamma confirmed_hubs = %d, want 0", stGamma.ConfirmedHubs)
	}

	stDelta, err := q.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{RegionID: forge, TypeID: typeShortHold})
	if err != nil {
		t.Fatalf("get delta stat: %v", err)
	}
	if stDelta.MoverHoldDays != 3 {
		t.Errorf("delta mover_hold_days = %d, want 3", stDelta.MoverHoldDays)
	}

	// 1) Default Trends view in The Forge: volume-confirmed move, 100M ISK, 10 trades/day.
	rec := get(app, nil, "/market/trends/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/ = %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, "/market/trends/ (default)", body,
		"Alpha Confirmed Multi",
		"&#43;1 hub",
		"Beta Confirmed Single",
		"Zeta Spread Leader",
		"100.00M ISK",
	)
	for _, excluded := range []string{"Gamma Manipulated Spike", "Delta Short Hold", "Epsilon Low Vol Ratio"} {
		if strings.Contains(body, excluded) {
			t.Errorf("default volume-confirmed Trends unexpectedly included %q", excluded)
		}
	}

	// 2) Domain default uses the 20M ISK floor so Eta Domain Mover (35M ISK/day) appears.
	recDom := get(app, nil, "/market/trends/?region=10000043")
	if recDom.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/?region=10000043 = %d", recDom.Code)
	}
	mustContain(t, "Domain default", recDom.Body.String(), "Eta Domain Mover", "20.00M ISK")

	// 3) Spread profit sort ranks Zeta Spread Leader #1.
	recProfit := get(app, nil, "/market/trends/?sort=spread_profit")
	if recProfit.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/?sort=spread_profit = %d", recProfit.Code)
	}
	profitBody := recProfit.Body.String()
	idxZeta := strings.Index(profitBody, "Zeta Spread Leader")
	idxBeta := strings.Index(profitBody, "Beta Confirmed Single")
	if idxZeta < 0 || idxBeta < 0 || idxZeta > idxBeta {
		t.Errorf("spread_profit sort did not rank Zeta Spread Leader ahead of Beta Confirmed Single")
	}

	// 4) Raw % mode turns off all gates and shows manipulation warning columns.
	recRaw := get(app, nil, "/market/trends/?sort=raw_pct")
	if recRaw.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/?sort=raw_pct = %d", recRaw.Code)
	}
	rawBody := recRaw.Body.String()
	mustContain(t, "raw % mode", rawBody,
		"Gamma Manipulated Spike",
		"All gates off",
		"Low",
		"Widening",
		"1 hub only",
	)

	// 5) What changed enforces ISK/day and trades/day floors in SQL,
	//    and shows everything when raw_pct or 0 floors are selected.
	today := dayStr(0)
	if _, err := app.db.ExecContext(ctx, `
		INSERT INTO market_changes (region_id, type_id, day, kind, before, after, pct, detected_at) VALUES
		($1, $2, $3, 'price_up', 100, 140, 40, now()),
		($1, $4, $3, 'spread_wide', 5, 500, 495, now())
		ON CONFLICT (region_id, type_id, day, kind) DO UPDATE SET before = excluded.before, after = excluded.after, pct = excluded.pct`,
		forge, typeConfirmedMulti, today, typeManipulated); err != nil {
		t.Fatalf("seed market_changes: %v", err)
	}
	recChanges := get(app, nil, "/market/trends/?mode=changes")
	if recChanges.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/?mode=changes = %d", recChanges.Code)
	}
	changesBody := recChanges.Body.String()
	mustContain(t, "What changed (default floors)", changesBody,
		"Alpha Confirmed Multi",
		"100.00M ISK",
	)
	if strings.Contains(changesBody, "Gamma Manipulated Spike") {
		t.Errorf("What changed with default 100M ISK / 10 trades floor leaked Gamma Manipulated Spike")
	}

	recChangesRaw := get(app, nil, "/market/trends/?mode=changes&sort=raw_pct")
	if recChangesRaw.Code != http.StatusOK {
		t.Fatalf("GET /market/trends/?mode=changes&sort=raw_pct = %d", recChangesRaw.Code)
	}
	mustContain(t, "What changed (raw mode)", recChangesRaw.Body.String(),
		"Alpha Confirmed Multi",
		"Gamma Manipulated Spike",
	)

	// 6) Market Movers widget: confirmed default, Domain 20M default, spread_profit, isk_move, and raw_pct.
	def := homeWidgetRegistry[widgetMovers]
	if def == nil {
		t.Fatalf("widgetMovers not registered")
	}
	runWidget := func(opts map[string]string) *moversWidgetView {
		o := def.defaultOptions()
		for k, v := range opts {
			o[k] = v
		}
		in := &widgetInput{
			Opts:  o,
			Forms: []string{"small", "tall", "medium", "large"},
		}
		return buildMoversWidget(ctx, app, in).(*moversWidgetView)
	}

	wDefault := runWidget(nil)
	if len(wDefault.Rising) == 0 || wDefault.Rising[0].TypeID != typeConfirmedMulti {
		t.Fatalf("default movers rising = %+v, want Alpha Confirmed Multi first", wDefault.Rising)
	}
	if len(wDefault.Moves) == 0 || wDefault.Moves[0].TypeID != typeConfirmedMulti {
		t.Fatalf("default movers tall Moves = %+v, want Alpha Confirmed Multi first from stored table", wDefault.Moves)
	}
	if wDefault.Rising[0].HubBadge != "+1 hub" {
		t.Errorf("default movers top HubBadge = %q, want +1 hub", wDefault.Rising[0].HubBadge)
	}
	// Tall-only form reads directly from the stored combined bucket without needing Rising/Falling.
	tallIn := &widgetInput{Opts: def.defaultOptions(), Forms: []string{"tall"}}
	wTall := buildMoversWidget(ctx, app, tallIn).(*moversWidgetView)
	if !wTall.Any() || len(wTall.Moves) == 0 || wTall.Moves[0].TypeID != typeConfirmedMulti {
		t.Errorf("tall-only movers = %+v, want Moves populated with Alpha first", wTall)
	}
	for _, r := range wDefault.Rising {
		if r.TypeID == typeManipulated || r.TypeID == typeShortHold || r.TypeID == typeFallingVol {
			t.Errorf("default movers widget unexpectedly included gated item %s", r.Name)
		}
	}

	wDomain := runWidget(map[string]string{"region": "10000043"})
	foundEta := false
	for _, r := range wDomain.Rising {
		if r.TypeID == typeDomainOnly {
			foundEta = true
		}
	}
	if !foundEta {
		t.Errorf("Domain default movers widget missing Eta Domain Mover (35M ISK/d >= 20M hub default)")
	}

	wProfit := runWidget(map[string]string{"sort": moversSortSpreadProfit})
	if len(wProfit.Rising) == 0 || wProfit.Rising[0].TypeID != typeSpreadLeader {
		t.Errorf("spread_profit movers widget top = %+v, want Zeta Spread Leader", wProfit.Rising)
	}

	wRaw := runWidget(map[string]string{"sort": moversSortRawPct})
	if len(wRaw.Rising) == 0 || wRaw.Rising[0].TypeID != typeManipulated {
		t.Fatalf("raw_pct movers widget top = %+v, want Gamma Manipulated Spike", wRaw.Rising)
	}
	for _, wantWarn := range []string{"trades/d", "widening spread", "1 hub only"} {
		if !strings.Contains(wRaw.Rising[0].Warn, wantWarn) {
			t.Errorf("raw_pct movers widget warning %q missing %q", wRaw.Rising[0].Warn, wantWarn)
		}
	}

	// Render every form of movers.html to guarantee no template runtime errors.
	ts, err := homeTemplateSet()
	if err != nil {
		t.Fatalf("homeTemplateSet: %v", err)
	}
	for _, form := range []string{"small", "tall", "medium", "large"} {
		for _, view := range []*moversWidgetView{wDefault, wRaw} {
			var buf bytes.Buffer
			if err := ts.ExecuteTemplate(&buf, "w-movers-"+form, map[string]any{
				"BodyClass": "w-body-test",
				"Data":      view,
			}); err != nil {
				t.Errorf("render w-movers-%s: %v", form, err)
			}
		}
	}
}
