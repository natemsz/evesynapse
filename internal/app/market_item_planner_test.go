package app

// Hermetic tests for (market item depth + planner
// judging): chart day-data and recent-days geometry as pure
// functions, the trader snapshot's window math, the item page's
// interactive markup and snapshot strip rendered against a stub
// book, and planner "Judge as" scoping — skill gates, scoped
// inventory netting, and priced verdicts — through the real
// router with the transport pinned at zero outbound calls.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/markethistory"
)

// ---------------------------------------------------------------------------
// Chart data + trader math (pure)
// ---------------------------------------------------------------------------

func tradeRow(date string, avg, high, low float64, vol int64) db.MarketHistory {
	return db.MarketHistory{RegionID: 10000002, TypeID: 34, Date: date, Average: avg, Highest: high, Lowest: low, Volume: vol, OrderCount: 10}
}

func TestBuildPriceChartCarriesDayData(t *testing.T) {
	rows := []db.MarketHistory{
		tradeRow("2026-09-30", 10.0, 11.0, 9.0, 123456),
		tradeRow("2026-10-01", 10.5, 11.5, 9.5, 100000),
		tradeRow("2026-10-02", 10.2, 11.2, 9.2, 90000),
	}
	chart, ok := markethistory.BuildPriceChart(rows)
	if !ok {
		t.Fatal("chart: ok=false with rows")
	}
	if len(chart.Dots) != 3 {
		t.Fatalf("dots: got %d, want 3", len(chart.Dots))
	}
	last := chart.Dots[2]
	if last.Date != "2026-10-02" || last.Average != "10.20" || last.Highest != "11.20" || last.Lowest != "9.20" || last.Volume != "90,000" {
		t.Fatalf("dot day data: %+v", last.Day)
	}
	if !strings.Contains(last.Title, "2026-10-02") || !strings.Contains(last.Title, "10.20") {
		t.Fatalf("dot title: %q", last.Title)
	}
	if len(chart.Bars) != 3 || !strings.Contains(chart.Bars[0].Title, "volume 123,456") {
		t.Fatalf("bars: %+v", chart.Bars)
	}
	// Axes: price carries max/mid/min in compact ISK, volume
	// carries max/mid/zero on its own scale, and the x axis is
	// anchored at the first, middle, and last recorded days.
	if len(chart.PriceTicks) != 3 ||
		chart.PriceTicks[0].Label != "10.5 ISK" ||
		chart.PriceTicks[1].Label != "10.25 ISK" ||
		chart.PriceTicks[2].Label != "10 ISK" {
		t.Fatalf("price ticks: %+v", chart.PriceTicks)
	}
	if len(chart.VolumeTicks) != 3 ||
		chart.VolumeTicks[0].Label != "123K" ||
		chart.VolumeTicks[1].Label != "61.7K" ||
		chart.VolumeTicks[2].Label != "0" {
		t.Fatalf("volume ticks: %+v", chart.VolumeTicks)
	}
	if len(chart.DateTicks) != 3 ||
		chart.DateTicks[0].Label != "2026-09-30" || chart.DateTicks[0].Anchor != "start" ||
		chart.DateTicks[1].Label != "2026-10-01" || chart.DateTicks[1].Anchor != "middle" ||
		chart.DateTicks[2].Label != "2026-10-02" || chart.DateTicks[2].Anchor != "end" ||
		chart.DateTicks[1].X <= chart.DateTicks[0].X || chart.DateTicks[2].X <= chart.DateTicks[1].X {
		t.Fatalf("date ticks: %+v", chart.DateTicks)
	}
	// Recent days: newest first, same figures.
	if len(chart.Recent) != 3 || chart.Recent[0].Date != "2026-10-02" || chart.Recent[2].Date != "2026-09-30" {
		t.Fatalf("recent: %+v", chart.Recent)
	}

	// The recent table caps at markethistory.RecentChartDays.
	var many []db.MarketHistory
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		many = append(many, tradeRow(base.AddDate(0, 0, i).Format(markethistory.DateLayout), float64(100+i), 0, 0, 100))
	}
	chart, _ = markethistory.BuildPriceChart(many)
	if len(chart.Recent) != markethistory.RecentChartDays {
		t.Fatalf("recent cap: got %d, want %d", len(chart.Recent), markethistory.RecentChartDays)
	}
	if chart.Recent[0].Date != "2026-08-30" {
		t.Fatalf("recent newest: %q, want 2026-08-30", chart.Recent[0].Date)
	}
}

func TestTraderStatsWindowsAndMargin(t *testing.T) {
	var rows []db.MarketHistory
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		avg := float64(100 + i)
		rows = append(rows, tradeRow(base.AddDate(0, 0, i).Format(markethistory.DateLayout), avg, avg+1, avg-1, 2000))
	}
	stats := markethistory.BuildTraderStats(rows, 110, 100)
	if stats.Avg7 != "136.00" { // mean of 133..139
		t.Fatalf("Avg7: %q, want 136.00", stats.Avg7)
	}
	if stats.Avg30 != "124.50" { // mean of 110..139
		t.Fatalf("Avg30: %q, want 124.50", stats.Avg30)
	}
	if stats.AvgVol7 != "2,000" {
		t.Fatalf("AvgVol7: %q, want 2,000", stats.AvgVol7)
	}
	if stats.MarginPct != "9.1%" { // (110−100)/110
		t.Fatalf("MarginPct: %q, want 9.1%%", stats.MarginPct)
	}

	// Sparse rows: the window only counts recorded days inside it.
	sparse := []db.MarketHistory{
		tradeRow("2026-09-01", 50, 0, 0, 700),
		tradeRow("2026-10-01", 100, 0, 0, 300),
	}
	stats = markethistory.BuildTraderStats(sparse, 0, 0)
	if stats.Avg7 != "100.00" || stats.Avg30 != "100.00" || stats.AvgVol7 != "300" {
		t.Fatalf("sparse stats: %+v", stats)
	}
	if stats.MarginPct != "" {
		t.Fatalf("margin without a book: %q, want empty", stats.MarginPct)
	}

	// No rows: nothing invented; a one-sided book gives no margin.
	stats = markethistory.BuildTraderStats(nil, 110, 0)
	if stats.Avg7 != "" || stats.Avg30 != "" || stats.AvgVol7 != "" || stats.MarginPct != "" {
		t.Fatalf("empty stats invented figures: %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// Item page: interactive chart markup + trading snapshot
// ---------------------------------------------------------------------------

func TestMarketItemChartMarkupAndSnapshot(t *testing.T) {
	book := `[
		{"order_id":1,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":11.0,"volume_remain":5,"volume_total":5},
		{"order_id":2,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":true,"price":10.0,"volume_remain":7,"volume_total":7}
	]`
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}, bookJSON: book}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed34SDEType(t, app)
	cookie := seedItemPage(t, app, q)

	today := time.Now().UTC()
	for i := 0; i < 20; i++ {
		avg := float64(100 + i)
		day := today.AddDate(0, 0, i-19).Format(markethistory.DateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 34, Date: day, Average: avg,
			Highest: avg + 5, Lowest: avg - 5, Volume: 1000, OrderCount: 90,
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}

	_, body := getPage(t, app, cookie, "/market/?type=34")
	newest := today.Format(markethistory.DateLayout)
	mustContain(t, "/market/?type=34 (interactive chart)", body,
		`<circle class="cdot"`,
		`data-chart-scrub="true"`,
		`class="chart-legend"`,
		"Average price",
		"Price in ISK · volume in units traded",
		`class="chart-axis-label price-axis`,
		`class="chart-axis-label volume-axis`,
		`class="chart-axis-label chart-date-tick`,
		`>119 ISK<`,
		`>1K<`,
		`data-date="`+newest+`"`,
		`data-avg="119.00"`,
		`data-high="124.00"`,
		`data-vol="1,000"`,
		`<div class="ctip" hidden></div>`,
		"Last recorded days",
		"<td>"+newest+"</td>",
		"Trading snapshot",
		">116.00 ISK<", // 7-day average of 113..119
		"9.1% of the sell price",
	)

	// The chart spans the panel edge to edge — no fixed-width
	// cap left on the svg — and the settled snapshot is not a
	// live region (nothing left to fill in).
	if strings.Contains(body, "max-width:720px") {
		t.Fatal("/market/?type=34 chart svg still caps its width at 720px")
	}
	mustContain(t, "/market/?type=34 (full-width chart)", body,
		`<svg viewBox="0 0 720 240" width="100%"`,
		`<div class="trader-body" data-poll-state="chart">`)
	if strings.Contains(body, "/market/trader-fragment") {
		t.Fatal("settled market page still polls the trader fragment")
	}

	// The history fragment stays cache-only: polling it must not
	// move the transport, and it re-renders the same day data.
	calls := transport.calls.Load()
	_, frag := getPage(t, app, cookie, "/market/history-fragment?region=10000002&type=34")
	mustContain(t, "/market/history-fragment", frag, `data-date="`+newest+`"`)
	if got := transport.calls.Load(); got != calls {
		t.Fatalf("history fragment made %d outbound calls, want 0", got-calls)
	}
}

// ---------------------------------------------------------------------------
// Planner judging
// ---------------------------------------------------------------------------

// plannerScopeApp plants two characters on one account with
// the planner SDE fixture (tags are set per test).
func plannerScopeApp(t *testing.T) (*Application, *db.Queries, *http.Cookie) {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	seedPlannerSDE(t, app)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	return app, q, cookie
}

func setCharTags(t *testing.T, app *Application, characterID int64, tags string) {
	t.Helper()
	if _, err := app.db.ExecContext(context.Background(),
		`UPDATE characters SET tags = $1 WHERE character_id = $2`, tags, characterID); err != nil {
		t.Fatalf("set tags: %v", err)
	}
}

func seedCharSkills(t *testing.T, q *db.Queries, characterID int64, level int) {
	t.Helper()
	seedSnapshot(t, q, characterID, esi.SnapSkills, esi.Skills{
		TotalSP: 1000000,
		Skills:  []esi.Skill{{SkillID: 3380, SkillpointsInSkill: 256000, ActiveSkillLevel: level, TrainedSkillLevel: level}},
	})
}

func seedTritanium(t *testing.T, q *db.Queries, characterID, qty int64) {
	t.Helper()
	seedSnapshot(t, q, characterID, esi.SnapAssets, []esi.Asset{
		{ItemID: characterID*10 + qty, TypeID: 34, Quantity: qty, LocationID: 60003760, LocationType: "station"},
	})
}

func seedPlannerPrices(app *Application) {
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 100}
}

func TestPlannerJudgeCharacterBuildable(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedCharSkills(t, q, fixtureCharA, 5)
	seedTritanium(t, q, fixtureCharA, 15)
	seedTritanium(t, q, fixtureCharB, 1000) // must NOT count under char A's scope
	seedPlannerPrices(app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	code, body := getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("judge char page: status %d", code)
	}
	mustContain(t, path, body,
		`name="judge"`,
		fmt.Sprintf(`value="char:%d" selected`, fixtureCharA),
		"Can Fixture Ceo build this?",
		"Industry — needs IV · trained V — covered",
		"Every skill in the chain is covered.",
		"netted against what Fixture Ceo holds",
		// 20 Tritanium needed, 15 on hand → 5 to buy at 5 ISK.
		"Worth making — judged as Fixture Ceo",
		"25.00 ISK",
		"975.00 ISK", // profit = 1,000.00 value − 25.00 cost
		"97.5% of the sell value",
	)
}

func TestPlannerJudgeCharacterShortSkill(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedCharSkills(t, q, fixtureCharA, 3) // root wants I, the component wants IV
	seedPlannerPrices(app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		"Industry — needs IV · trained III",
		"Not yet — the short skills are listed above.",
	)
	if strings.Contains(body, "Industry — needs IV · trained III — covered") {
		t.Fatal("short skill marked covered")
	}
}

func TestPlannerJudgeCharacterNoSkillsYet(t *testing.T) {
	app, _, cookie := plannerScopeApp(t)
	seedPlannerPrices(app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		"Skills haven&#39;t synced for Fixture Ceo yet",
		"Fixture Ceo&#39;s hangars haven&#39;t synced yet",
	)
	if strings.Contains(body, "can build this?</strong>") && strings.Contains(body, "covered") {
		t.Fatal("unknown skills rendered a verdict")
	}
}

func TestPlannerJudgeTagBestOfAndCombinedStock(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	setCharTags(t, app, fixtureCharA, "indy")
	setCharTags(t, app, fixtureCharB, "indy")
	seedCharSkills(t, q, fixtureCharA, 3)
	seedCharSkills(t, q, fixtureCharB, 5) // the tag's best covers IV
	seedTritanium(t, q, fixtureCharA, 15)
	seedTritanium(t, q, fixtureCharB, 10) // combined 25 ≥ 20 needed
	seedPlannerPrices(app)

	path := "/planner/?product=1001&runs=1&judge=tag:indy"
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		`value="tag:indy" selected`,
		`Can your &#34;indy&#34; characters build this?`,
		"Industry — needs IV · best on the tag V — covered",
		"Every skill in the chain is covered.",
		`combined hangars of your &#34;indy&#34; characters (2 of them)`,
	)
}

func TestPlannerJudgeTagNobodyHasIt(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	setCharTags(t, app, fixtureCharA, "indy")
	setCharTags(t, app, fixtureCharB, "indy")
	seedCharSkills(t, q, fixtureCharA, 1) // B has no snapshot at all
	seedPlannerPrices(app)

	path := "/planner/?product=1001&runs=1&judge=tag:indy"
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		"Industry — needs IV · best on the tag I",
		"Skills are on file for 1 of 2 tagged characters",
		"Not yet — the short skills are listed above.",
	)
}

func TestPlannerJudgePricesIncomplete(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedCharSkills(t, q, fixtureCharA, 5)
	seedTritanium(t, q, fixtureCharA, 15)
	// No prices anywhere: the verdict must refuse to guess.

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		"Worth making — judged as Fixture Ceo",
		"Prices incomplete",
	)
	if strings.Contains(body, "Profit per run") {
		t.Fatal("verdict quoted a profit on unknown prices")
	}
}

func TestPlannerNoScopeRendersAsBefore(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedTritanium(t, q, fixtureCharA, 15)
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedPlannerPrices(app)

	_, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	mustContain(t, "/planner/ (general)", body,
		"ME 8% · TE 12% (owned blueprint)",
		"975.00 ISK",
	)
	for _, absent := range []string{"build this?", "Worth making — judged as"} {
		if strings.Contains(body, absent) {
			t.Fatalf("general plan grew a scoped block: %q present", absent)
		}
	}
}
