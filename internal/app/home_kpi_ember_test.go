package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestNetWorthSparkTotals: the sparkline series is each day's total
// net worth summed across the account's characters, ascending,
// capped at the last 31 days; days with no computed net worth stay
// off, exactly like the chart series.
func TestNetWorthSparkTotals(t *testing.T) {
	rows := []db.WalletHistory{
		{CharacterID: 1, Day: "2026-03-01", NetWorth: nw(1000)},
		{CharacterID: 2, Day: "2026-03-01", NetWorth: nw(250)},
		{CharacterID: 1, Day: "2026-03-02", NetWorth: sql.NullFloat64{}},
		{CharacterID: 2, Day: "2026-03-02", NetWorth: nw(300)},
		{CharacterID: 1, Day: "2026-03-03", NetWorth: nw(200)},
	}
	if got := netWorthSparkTotals(rows); fmt.Sprint(got) != "[1250 300 200]" {
		t.Errorf("totals = %v, want [1250 300 200]", got)
	}

	many := make([]db.WalletHistory, 0, 40)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 40; d++ {
		many = append(many, db.WalletHistory{CharacterID: 1,
			Day: base.AddDate(0, 0, d).Format("2006-01-02"), NetWorth: nw(float64(d + 1))})
	}
	got := netWorthSparkTotals(many)
	if len(got) != 31 || got[0] != 10 || got[30] != 40 {
		t.Errorf("capped totals = %v, want the last 31 days (10..40)", got)
	}
}

// TestSparklineSVG: the shaped SVG — and no SVG at all under two
// days, so the KPI never draws a placeholder line.
func TestSparklineSVG(t *testing.T) {
	line, area, ok := sparklineSVG([]float64{100, 150, 125})
	if !ok {
		t.Fatal("three days drew no sparkline")
	}
	if line != "2.0,26.0 50.0,2.0 98.0,14.0" {
		t.Errorf("line points = %q", line)
	}
	if !strings.HasSuffix(area, "98.0,28.0 2.0,28.0") || !strings.HasPrefix(area, line) {
		t.Errorf("area points = %q, want the line closed along the bottom", area)
	}
	if _, _, ok := sparklineSVG([]float64{100}); ok {
		t.Error("one day drew a sparkline; want none")
	}
	if _, _, ok := sparklineSVG(nil); ok {
		t.Error("no history drew a sparkline; want none")
	}
}

// TestHomeKPISparkAndPricedBar: with two sampled days the Fleet net
// worth KPI carries the sparkline and the Hangar KPI its priced-share
// bar; with one day there is no SVG anywhere.
func TestHomeKPISparkAndPricedBar(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 31, TypeID: 34, Quantity: 100},
		{ItemID: 32, TypeID: 999999, Quantity: 7},
	})
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 5, AvgDailyVolume: 1_000_000,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	setHomeLayout(t, q, user.ID, `[{"id":"glance"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-01", 100, nw(1000))
	_, body := getPage(t, app, cookie, "/")
	if strings.Contains(body, "kpi-spark") {
		t.Error("one sampled day rendered a sparkline; want none")
	}

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-02", 120, nw(1400))
	code, body := getPage(t, app, cookie, "/")
	if code != 200 {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ (spark + priced bar)", body,
		`class="kpi-spark"`, `class="kpi-spark-line"`,
		`<span class="kpi-bar"><span data-w="50.0"></span></span>`) // 1 of 2 items priced
}

// TestHomeKPIGlyphs: each of the four glance KPI tiles carries
// its mockup glyph span (wallet, coins, package, book) — the
// briefing-card icon treatment at tile scale — and no other tile
// on the page grows one.
func TestHomeKPIGlyphs(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)
	setHomeLayout(t, q, user.ID, `[{"id":"glance"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/")
	if code != 200 {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ (kpi glyphs)", body,
		`<span class="kpi-icon kpi-icon-wallet" aria-hidden="true">`,
		`<span class="kpi-icon kpi-icon-coins" aria-hidden="true">`,
		`<span class="kpi-icon kpi-icon-assets" aria-hidden="true">`,
		`<span class="kpi-icon kpi-icon-skills" aria-hidden="true">`,
		// One distinctive path fragment per glyph, so a swapped
		// or dropped path fails here, not on the owner's screen.
		`M19 7V4a1 1 0 0 0-1-1H5`,                   // wallet body
		`<circle cx="16" cy="8" r="6"/>`,            // coins top coin
		`<polyline points="3.29 7 12 12 20.71 7"/>`, // package lid seam
		`M22 4h-6a4 4 0 0 0-4 4v12`)                 // book-open right cover
	if got := strings.Count(body, `class="kpi-icon `); got != 4 {
		t.Errorf("kpi glyph spans = %d, want exactly the four glance tiles", got)
	}
}

// TestFleetSecurityBadge: a pilot's location cell carries the
// security pill from the stored systems row (tier and one-decimal
// value by the shared rounding, negatives included); a pilot with
// no known location has no badge.
func TestFleetSecurityBadge(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo")
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, -0.32)`); err != nil {
		t.Fatalf("seed system: %v", err)
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142})
	setHomeLayout(t, q, user.ID, `[{"id":"fleet"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/")
	if code != 200 {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ (fleet badge)", body, `<span class="sec-pill sec-0">-0.3</span>`)
	if got := strings.Count(body, "sec-pill"); got != 1 {
		t.Errorf("security pills = %d, want 1 (only the pilot with a known location)", got)
	}
}
