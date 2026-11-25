package app

import (
	"context"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestSparklineSVG: the shaped SVG, and no SVG at all under two
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

// TestHomeGlanceLinesAndPricedShare: with two sampled days the medium
// At a Glance draws the net worth tile's line and the asset tile says
// how much of the hangar is priced; with one day there is no line.
func TestHomeGlanceLinesAndPricedShare(t *testing.T) {
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
	setHomeLayout(t, q, user.ID, `[{"id":"glance","w":2,"h":2}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-01", 100, nw(1000))
	_, body := getPage(t, app, cookie, "/")
	if strings.Contains(body, `class="w-spark pchart"`) {
		t.Error("one sampled day drew a line; want none")
	}

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-02", 120, nw(1400))
	code, body := getPage(t, app, cookie, "/")
	if code != 200 {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ (line + priced share)", body,
		`data-tile="networth"`, `class="w-spark pchart"`,
		"1 of 2 items priced")
}

// wideCard is one card of a home page, the widget's body as a desktop
// reader sees it, without the one-column copy a phone draws beside it.
func wideCard(body, id string) string {
	i := strings.Index(body, `data-widget="`+id+`"`)
	if i < 0 {
		return ""
	}
	card := body[i:]
	if j := strings.Index(card, `<section class="card`); j >= 0 {
		card = card[:j]
	}
	if j := strings.Index(card, `w-narrow"`); j >= 0 {
		card = card[:j]
	}
	return card
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
	if got := strings.Count(wideCard(body, "fleet"), "sec-pill"); got != 1 {
		t.Errorf("security pills = %d, want 1 (only the pilot with a known location)", got)
	}
}
