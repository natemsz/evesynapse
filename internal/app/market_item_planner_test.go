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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/buildplanner"
	"evesynapse/internal/buildplan"
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
		"5-day average",
		"Daily wicks: lowest to highest trade",
		`class="chart-axis-label price-axis`,
		`class="chart-axis-label volume-axis`,
		`class="chart-axis-label chart-date-tick`,
		`>124 ISK<`,
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

func seedPlannerPrices(t *testing.T, app *Application) {
	t.Helper()
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 100}
	// The guide still bases job fees; plan prices resolve from
	// the stored house sources, so the same figures are planted
	// as Forge typical sells.
	seedPlannerTypicals(t, app, map[int64]float64{34: 5, 1001: 1000, 1002: 100})
}

// seedPlannerTypicals plants Forge region-stat rows whose
// typical sell carries the fixture price: the planner's first
// price source.
func seedPlannerTypicals(t *testing.T, app *Application, typicals map[int64]float64) {
	t.Helper()
	ctx := context.Background()
	for id, typical := range typicals {
		if _, err := app.db.ExecContext(ctx,
			`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES (10000002, $1, $2, '2026-10-16T00:00:00Z')`,
			id, typical); err != nil {
			t.Fatalf("seed typical %d: %v", id, err)
		}
	}
}

func TestPlannerJudgeCharacterBuildable(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedCharSkills(t, q, fixtureCharA, 5)
	seedTritanium(t, q, fixtureCharA, 15)
	seedTritanium(t, q, fixtureCharB, 1000) // must NOT count under char A's scope
	seedPlannerPrices(t, app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	code, body := getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("judge char page: status %d", code)
	}
	mustContain(t, path, body,
		`name="judge"`,
		fmt.Sprintf(`value="char:%d"`, fixtureCharA),
		`aria-current="true"`,
		`id="judge-q"`,
		`value="Fixture Ceo"`,
		`plan-skillglyph ok`,
		`aria-label="Can Fixture Ceo build this? All skills covered."`,
		`class="plan-tip"`,
		`plan-skillbadge" aria-hidden="true">✓`,
		// 20 Tritanium needed, 15 on hand → 5 to buy at 5 ISK.
		"Profit Analysis",
		"25.00 ISK",
		"975.00 ISK", // profit = 1,000.00 value − 25.00 cost
		"97.5% of the sell value",
	)
}

func TestPlannerJudgeCharacterShortSkill(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedCharSkills(t, q, fixtureCharA, 3) // root wants I, the component wants IV
	seedPlannerPrices(t, app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		`plan-skillglyph bad`,
		`<li>Industry — needs IV · trained III</li>`,
		`plan-skillbadge" aria-hidden="true">✕`,
	)
	if strings.Contains(body, "Industry — needs IV · trained III — covered") {
		t.Fatal("short skill marked covered")
	}
}

func TestPlannerJudgeCharacterNoSkillsYet(t *testing.T) {
	app, _, cookie := plannerScopeApp(t)
	seedPlannerPrices(t, app)

	path := fmt.Sprintf("/planner/?product=1001&runs=1&judge=char:%d", fixtureCharA)
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		`plan-skillglyph bad`,
		"Skills haven&#39;t synced for Fixture Ceo yet — once they land, this says what they can build.",
		`aria-label="Can Fixture Ceo build this? Some skills missing."`,
		"Fixture Ceo&#39;s hangars haven&#39;t synced yet",
	)
	if strings.Contains(body, "plan-skillglyph ok") {
		t.Fatal("unknown skills rendered a covered verdict")
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
	seedPlannerPrices(t, app)

	path := "/planner/?product=1001&runs=1&judge=tag:indy"
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		`value="tag:indy"`,
		`aria-current="true"`,
		`plan-skillglyph ok`,
		`aria-label="Can your &#34;indy&#34; characters build this? All skills covered."`,
		`class="plan-tip"`,
		`plan-skillbadge" aria-hidden="true">✓`,
		"Every skill in the chain is covered.",
	)
}

func TestPlannerJudgeTagNobodyHasIt(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	setCharTags(t, app, fixtureCharA, "indy")
	setCharTags(t, app, fixtureCharB, "indy")
	seedCharSkills(t, q, fixtureCharA, 1) // B has no snapshot at all
	seedPlannerPrices(t, app)

	path := "/planner/?product=1001&runs=1&judge=tag:indy"
	_, body := getPage(t, app, cookie, path)
	mustContain(t, path, body,
		`plan-skillglyph bad`,
		`aria-label="Can your &#34;indy&#34; characters build this? Some skills missing."`,
		"Skills are on file for 1 of 2 tagged characters; the rest count as untrained until they sync.",
		`<li>Industry — needs IV · best on the tag I</li>`,
		`plan-skillbadge" aria-hidden="true">✕`,
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
		"Profit Analysis",
		"Prices incomplete",
	)
	if strings.Contains(body, "Profit per run") {
		t.Fatal("verdict quoted a profit on unknown prices")
	}
}

// TestPlannerMESummaryCollapsed: the ME wall ships inside a
// hidden centered modal behind a static button plus a summary
// line, with the set-all action and the tree tab's global
// expand/collapse controls beside it.
func TestPlannerMESummaryCollapsed(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedTritanium(t, q, fixtureCharA, 15)
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedPlannerPrices(t, app)

	_, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	mustContain(t, "/planner/ (me summary)", body,
		`id="plan-form"`,
		`id="plan-wrap"`,
		`id="me-toggle"`,
		`aria-haspopup="dialog"`,
		`>Blueprint ME</button>`,
		`Fixture Component at ME8 (owned), 1 at ME0 (assumed)`,
		`id="me-modal" hidden`,
		`id="me-fields"`,
		`class="me-summary"`,
		`class="me-grid"`,
		`class="me-cell"`,
		`class="me-name"`,
		`id="me-setall"`,
		`id="me-apply"`,
		// The ME inputs name the plan form as their owner, like
		// the settings dialog's controls: association by attribute
		// survives the dialog being lifted out of the form.
		`name="me_2002" form="plan-form"`,
		`data-me-src="owned"`,
		`data-plan-expand-all`,
		`data-plan-collapse-all`,
		"Fixture Component", // labels still render inside the collapsed block
	)
}

// TestPlannerSettingsModal: Runs, Market, Build system, Slots,
// Time factor and Recalculate live inside a modal behind a glyph
// toggle in the planner header; the build-system face shows the
// resolved system name, never a raw ID.
func TestPlannerSettingsModal(t *testing.T) {
	app, _, cookie := plannerScopeApp(t)
	seedPlannerPrices(t, app)
	if _, err := app.db.ExecContext(context.Background(),
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatalf("seed system: %v", err)
	}

	_, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	mustContain(t, "/planner/ (settings)", body,
		`id="plan-settings-toggle"`,
		`aria-label="Build settings"`,
		`aria-haspopup="dialog"`,
		`id="plan-settings-modal" hidden`,
		`id="plan-settings-close"`,
		`class="plan-settings-fields plan-setgrid"`,
		`name="runs"`,
		`name="market"`,
		`id="system-q" name="system" form="plan-form" value="Jita"`,
		`name="slots"`,
		`name="timef"`,
		`>Recalculate</button>`,
		// Every control in the dialog names the plan form as its
		// owner: the shell lifts dialogs out of the form's subtree,
		// and the explicit association is what keeps the control a
		// working part of the form wherever the dialog sits.
		`name="runs" form="plan-form"`,
		`name="judge" form="plan-form"`,
		`name="market" form="plan-form"`,
		`id="system-q" name="system" form="plan-form"`,
		`<select name="structure" form="plan-form">`,
		`<select name="rig" form="plan-form">`,
		`name="ftax" form="plan-form"`,
		`<select name="alpha" form="plan-form">`,
		`name="slots" form="plan-form"`,
		`name="timef" form="plan-form"`,
		`<button type="submit" form="plan-form">Recalculate</button>`,
	)

	// A system name in the query resolves to the same face, and a
	// numeric ID renders as the name too: the raw ID is never the
	// visible value.
	_, named := getPage(t, app, cookie, "/planner/?product=1001&runs=1&system=Jita")
	mustContain(t, "/planner/ (system name)", named, `id="system-q" name="system" form="plan-form" value="Jita"`)
	_, numbered := getPage(t, app, cookie, "/planner/?product=1001&runs=1&system=30000142")
	mustContain(t, "/planner/ (system id)", numbered, `id="system-q" name="system" form="plan-form" value="Jita"`)
}

// TestIndustryBlueprintPlannerLink: an owned blueprint whose SDE
// product maps carries a hammer-glyph link into the build planner.
func TestIndustryBlueprintPlannerLink(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2001, LocationID: 60003760, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, `[]`)

	_, body := getPage(t, app, cookie, "/industry/")
	mustContain(t, "/industry/", body,
		`/planner/?product=1001`,
		`title="open in build planner"`,
		`aria-label="Fixture Widget — open in build planner"`,
		`plan-glyph-sm`,
	)
}

// seedCapSDE plants a capital-scale blueprint tree: a hull with
// 40 components of 15 sub-assemblies of 8 materials (5441 nodes
// expanded). Leaf types are distinct rows so the shape mirrors
// shared minerals without tripping unique constraints.
func seedCapSDE(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	var bps, mats, skills, types []string
	types = append(types, "(10001, 'Cap Hull', 0, 0, 1)")
	for i := 34; i < 42; i++ {
		types = append(types, fmt.Sprintf("(%d, 'Cap Leaf %d', 0, 0, 1)", 9000+i, i))
	}
	bps = append(bps, "(20001, 10001, 1, 0, 3600)")
	var rootMats []string
	for i := 0; i < 40; i++ {
		compID := 11000 + i
		bps = append(bps, fmt.Sprintf("(%d, %d, 1, 0, 600)", 21000+i, compID))
		rootMats = append(rootMats, fmt.Sprintf("(20001, %d, 1)", compID))
		skills = append(skills, fmt.Sprintf("(%d, 3380, 1)", 21000+i))
		for j := 0; j < 15; j++ {
			subID := 12000 + i*15 + j
			bps = append(bps, fmt.Sprintf("(%d, %d, 1, 0, 60)", 22000+i*15+j, subID))
			mats = append(mats, fmt.Sprintf("(%d, %d, 1)", 21000+i, subID))
			skills = append(skills, fmt.Sprintf("(%d, 3380, 1)", 22000+i*15+j))
			for k := 0; k < 8; k++ {
				mats = append(mats, fmt.Sprintf("(%d, %d, 10)", 22000+i*15+j, 9034+k))
			}
		}
	}
	mats = append(mats, rootMats...)
	chunk := func(rows []string, stmt string) {
		for i := 0; i < len(rows); i += 400 {
			end := i + 400
			if end > len(rows) {
				end = len(rows)
			}
			if _, err := app.db.ExecContext(ctx, stmt+strings.Join(rows[i:end], ",")); err != nil {
				t.Fatalf("seed cap sde: %v", err)
			}
		}
	}
	chunk(types, `INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES `)
	chunk(bps, `INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES `)
	chunk(mats, `INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES `)
	chunk(skills, `INSERT INTO sde_blueprint_skills (blueprint_type_id, skill_type_id, level) VALUES `)
}

func capNodeCount(root *buildplan.Node) int {
	nodes := 0
	buildplan.Walk(root, func(n *buildplan.Node) { nodes++ })
	return nodes
}

// TestPlannerPrefetchMatchesLazy: the batched closure warm covers
// the whole capital tree (every blueprint memoized, leaves marked
// misses), and expanding over it matches a lazy expansion exactly
// — same nodes, same shopping, same money. Prefetch changes the
// read pattern, never the plan.
func TestPlannerPrefetchMatchesLazy(t *testing.T) {
	app, _, _ := plannerScopeApp(t)
	seedCapSDE(t, app)
	ctx := context.Background()

	lazy := buildplanner.NewSDESource(app, ctx)
	resLazy, err := buildplan.Build(lazy, 10001, buildplan.Input{Runs: 1})
	if err != nil {
		t.Fatalf("lazy build: %v", err)
	}
	warm := buildplanner.NewSDESource(app, ctx)
	warm.Prefetch(10001)
	if warm.Memoized() != 641 {
		t.Fatalf("prefetch memoized %d blueprints, want 641 (hull + 40 + 600)", warm.Memoized())
	}
	resWarm, err := buildplan.Build(warm, 10001, buildplan.Input{Runs: 1})
	if err != nil {
		t.Fatalf("warm build: %v", err)
	}
	if resWarm.TooLarge || resLazy.TooLarge {
		t.Fatalf("TooLarge warm=%v lazy=%v, want neither", resWarm.TooLarge, resLazy.TooLarge)
	}
	if got, want := capNodeCount(resWarm.Root), capNodeCount(resLazy.Root); got != want || got != 5441 {
		t.Fatalf("nodes warm=%d lazy=%d, want both 5441", got, want)
	}
	if len(resWarm.Shopping) != len(resLazy.Shopping) {
		t.Fatalf("shopping lines warm=%d lazy=%d", len(resWarm.Shopping), len(resLazy.Shopping))
	}
	for i := range resWarm.Shopping {
		if resWarm.Shopping[i] != resLazy.Shopping[i] {
			t.Fatalf("shopping line %d differs: %+v vs %+v", i, resWarm.Shopping[i], resLazy.Shopping[i])
		}
	}
	if resWarm.Root.LineCost != resLazy.Root.LineCost || resWarm.Root.CostComplete != resLazy.Root.CostComplete {
		t.Fatalf("root money differs: warm %v/%v lazy %v/%v",
			resWarm.Root.LineCost, resWarm.Root.CostComplete, resLazy.Root.LineCost, resLazy.Root.CostComplete)
	}
}

// TestPlannerCapitalJSONFirstLoad: a capital plan's JSON — what
// plan.js fetches on first load — serves the whole tree in one
// response: the root is present and the finished product is not
// in its own shopping list.
func TestPlannerCapitalJSONFirstLoad(t *testing.T) {
	app, _, cookie := plannerScopeApp(t)
	seedCapSDE(t, app)
	seedPlannerPrices(t, app)

	code, body := getPage(t, app, cookie, "/planner/?product=10001&judge=&format=json")
	if code != http.StatusOK {
		t.Fatalf("capital JSON: status %d", code)
	}
	var payload struct {
		Product int64 `json:"product"`
		Root    *struct {
			ID int64 `json:"id"`
		} `json:"root"`
		Optimized struct {
			Shopping []struct {
				ID int64 `json:"id"`
			} `json:"shopping"`
		} `json:"optimized"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("capital JSON does not parse: %v", err)
	}
	if payload.Product != 10001 || payload.Root == nil || payload.Root.ID != 10001 {
		t.Fatalf("capital JSON root = %+v, want product/root 10001", payload)
	}
	if len(payload.Optimized.Shopping) == 0 {
		t.Fatal("capital JSON aggregated no shopping")
	}
	for _, line := range payload.Optimized.Shopping {
		if line.ID == 10001 {
			t.Fatal("capital JSON shopping contains the finished product")
		}
	}
}

// TestPlannerDefaultScopeIsActiveCharacter: with no judge pick,
// the plan scopes to the acting character (the session's own),
// not to everyone together.
func TestPlannerDefaultScopeIsActiveCharacter(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedTritanium(t, q, fixtureCharA, 15)
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedPlannerPrices(t, app)

	_, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	mustContain(t, "/planner/ (default scope)", body,
		"Fixture Component",        // ME input keeps the owned blueprint visible
		"25.00 ISK",                // 20 needed, 15 on hand: 5 to buy at 5 ISK
		"materials (fees unknown)", // no cost-index row: fees stay unknown
		"Landed cost",
		`id="judge-q"`,
		`value="Fixture Ceo"`, // the picker shows the acting character
		fmt.Sprintf(`value="char:%d"`, fixtureCharA),
		`aria-current="true"`,
		"Can Fixture Ceo build this?",
		"Profit Analysis",
	)
	// One character: nothing to say about spanning.
	if strings.Contains(body, "Jobs timed and slotted as") {
		t.Error("a one-character plan claims to span characters")
	}
}

// TestPlannerExplicitGeneralScope: an explicit empty judge pick
// is the way back to everyone together (a missing pick defaults
// to the acting character instead).
func TestPlannerExplicitGeneralScope(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	seedTritanium(t, q, fixtureCharA, 15)
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedPlannerPrices(t, app)

	_, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1&judge=")
	mustContain(t, "/planner/ (general)", body,
		"Fixture Component",
		"25.00 ISK",
		`value="Everyone together"`,
		// The plan spans both characters and says whose jobs these
		// are: the acting character.
		"Jobs timed and slotted as Fixture Ceo; stock and blueprints pooled from 2 characters",
	)
	for _, absent := range []string{"build this?", "Profit Analysis"} {
		if strings.Contains(body, absent) {
			t.Fatalf("general plan grew a scoped block: %q present", absent)
		}
	}
}
