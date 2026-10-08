package app

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func nw(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

// TestNetWorthBreakdownDays: a day's total splits into the ISK on hand
// and the rest; characters with no computed net worth that day
// contribute neither part; the parts always add up to the total.
func TestNetWorthBreakdownDays(t *testing.T) {
	rows := []db.WalletHistory{
		{CharacterID: 1, Day: "2026-03-01", Balance: 100, NetWorth: nw(1000)},
		{CharacterID: 2, Day: "2026-03-01", Balance: 50, NetWorth: nw(250)},
		{CharacterID: 3, Day: "2026-03-01", Balance: 999, NetWorth: sql.NullFloat64{}}, // unknown: not counted at all
		{CharacterID: 1, Day: "2026-03-02", Balance: 5000, NetWorth: sql.NullFloat64{}},
		{CharacterID: 1, Day: "2026-03-03", Balance: 300, NetWorth: nw(200)}, // balance read after the net worth: assets clamp at 0
	}
	days := netWorthBreakdownDays(rows)
	if len(days) != 2 {
		t.Fatalf("%d days, want 2 (the day with no net worth stays off): %+v", len(days), days)
	}
	if d := days[0]; d.ISK != 150 || d.Assets != 1100 || d.Total() != 1250 {
		t.Errorf("day 1 = %+v, want ISK 150, assets 1100, total 1250", d)
	}
	if d := days[1]; d.ISK != 300 || d.Assets != 0 {
		t.Errorf("day 3 = %+v, want ISK 300, assets clamped to 0", d)
	}
}

// TestBuildNetWorthChart: two bands stacked under the total line, on a
// scale from zero; a legend and a per-day breakdown in the dots.
func TestBuildNetWorthChart(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, ok := buildNetWorthChart([]netWorthDay{{At: at("2026-03-01"), ISK: 1, Assets: 1}}); ok {
		t.Fatal("one day drew a chart")
	}
	chart, ok := buildNetWorthChart([]netWorthDay{
		{At: at("2026-03-01"), ISK: 100, Assets: 900},
		{At: at("2026-03-02"), ISK: 300, Assets: 700},
		{At: at("2026-03-03"), ISK: 200, Assets: 1800},
	})
	if !ok || !chart.Stacked {
		t.Fatalf("chart ok=%v stacked=%v, want a stacked chart", ok, chart.Stacked)
	}
	if len(chart.Bands) != 2 || chart.Bands[0].Label != "ISK" || chart.Bands[1].Label != "Assets and orders" {
		t.Fatalf("bands = %+v, want ISK then assets", chart.Bands)
	}
	// ISK band: 3 line points + 2 baseline corners; assets band: 3 along
	// the total + 3 back along the ISK line.
	if n := len(strings.Fields(chart.Bands[0].Points)); n != 5 {
		t.Errorf("ISK band has %d vertices, want 5", n)
	}
	if n := len(strings.Fields(chart.Bands[1].Points)); n != 6 {
		t.Errorf("assets band has %d vertices, want 6", n)
	}
	if len(chart.Dots) != 3 || chart.Dots[0].ISK == "" || chart.Dots[0].Assets == "" {
		t.Fatalf("dots = %+v, want 3 carrying the ISK/assets split", chart.Dots)
	}
	var names []string
	for _, l := range chart.Legend {
		names = append(names, l.Label)
	}
	if strings.Join(names, ",") != "ISK,Assets & orders,Total" {
		t.Errorf("legend = %v", names)
	}
	// The scale starts at zero: the bottom tick is 0, and the same total
	// at the same height means the line is higher where the total is.
	if last := chart.Ticks[len(chart.Ticks)-1]; !strings.HasPrefix(last.Label, "0 ") {
		t.Errorf("bottom tick = %q, want a zero baseline", last.Label)
	}
	if chart.Dots[2].Y >= chart.Dots[0].Y {
		t.Errorf("total 2000 (y=%d) should plot above total 1000 (y=%d)", chart.Dots[2].Y, chart.Dots[0].Y)
	}
}

// TestHomeNetWorthShowsTheBreakdown: the Home module draws the stacked
// chart with its legend and per-day ISK/assets figures.
func TestHomeNetWorthShowsTheBreakdown(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1234.5)
	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-01", 100, nw(1000))
	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-02", 120, nw(1400))
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/", body,
		`class="chart-band"`, `aria-label="Net worth over time, split into ISK and assets"`,
		`legend-isk`, `legend-assets`, `legend-total`, "Assets &amp; orders",
		`data-isk="`, `data-assets="`)
}
