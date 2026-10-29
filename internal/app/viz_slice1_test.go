package app

// Visualization slice 1: the ranged chart's series math against a
// hand-computed fixture (moving averages, the Donchian channel and
// the wicks, with the warmup rows in front of the displayed
// window), the prefs defaults and parsing, and a save/load round
// trip through the real router.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/markethistory"
)

func vizFixtureRows() []db.MarketHistory {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	avgs := []float64{200, 204, 208, 212, 216}
	rows := make([]db.MarketHistory, 0, 25)
	for g := 0; g < 25; g++ {
		avg := avgs[g%5]
		if g == 24 {
			avg = 220
		}
		high := avg + 6
		if g == 17 {
			high = avg + 30 // a warmup spike the channel must see
		}
		rows = append(rows, db.MarketHistory{
			RegionID: 10000002, TypeID: 34,
			Date:    base.AddDate(0, 0, g).Format(markethistory.DateLayout),
			Average: avg, Highest: high, Lowest: avg - 6,
			Volume:     int64(1000 + 100*g),
			OrderCount: int64(10 + g),
		})
	}
	return rows
}

func vizPoints(t *testing.T, points string) [][2]int {
	t.Helper()
	var out [][2]int
	for _, p := range strings.Fields(points) {
		xy := strings.Split(p, ",")
		if len(xy) != 2 {
			t.Fatalf("point %q is not x,y", p)
		}
		x, err1 := strconv.Atoi(xy[0])
		y, err2 := strconv.Atoi(xy[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("point %q does not parse: %v %v", p, err1, err2)
		}
		out = append(out, [2]int{x, y})
	}
	return out
}

func TestVizChartMathAndPrefs(t *testing.T) {
	rows := vizFixtureRows()
	all := markethistory.ChartOptions{Wicks: true, MAChannel: true, VolumeStrip: true}
	chart, ok := markethistory.BuildPriceChartRange(rows, 5, all)
	if !ok {
		t.Fatal("ranged chart not built")
	}
	// Displayed days are rows 20..24; the price scale spans their
	// wick extents: lowest 194 (day 20), highest 226 (day 24).
	// Band: TopY 12, BaseY 131, so y(v) = 131 - (v-194)/32*119.
	if chart.TopY != 12 || chart.BaseY != 131 {
		t.Fatalf("band: TopY %d BaseY %d, want 12/131", chart.TopY, chart.BaseY)
	}
	if chart.From != rows[20].Date || chart.To != rows[24].Date {
		t.Fatalf("range labels: %s to %s, want %s to %s", chart.From, chart.To, rows[20].Date, rows[24].Date)
	}

	// Wicks: one per displayed day, from highest to lowest.
	if len(chart.Wicks) != 5 {
		t.Fatalf("wicks: got %d, want 5", len(chart.Wicks))
	}
	if w := chart.Wicks[4]; w.X != 719 || w.Top != 12 || w.Bottom != 57 {
		t.Fatalf("last wick: %+v, want x=719 top=12 bottom=57", w)
	}
	if w := chart.Wicks[0]; w.X != 0 || w.Top != 87 || w.Bottom != 131 {
		t.Fatalf("first wick: %+v, want x=0 top=87 bottom=131", w)
	}

	// MA5 at the last day averages days 20..24 (208.8 -> y 76);
	// MA20 exists from the first displayed day only because the
	// 20 warmup rows are read (day 20 averages days 1..20 = 208
	// -> y 79).
	ma5 := vizPoints(t, chart.MA5Points)
	if len(ma5) != 5 || ma5[4] != [2]int{719, 76} {
		t.Fatalf("MA5 points: %v, want 5 points ending 719,76", ma5)
	}
	ma20 := vizPoints(t, chart.MA20Points)
	if len(ma20) != 5 || ma20[0] != [2]int{0, 79} {
		t.Fatalf("MA20 points: %v, want 5 points starting 0,79 (warmup boundary)", ma20)
	}

	// Donchian: the first displayed day's upper edge is the
	// warmup spike on day 17 (highest 238 -> y -32), not anything
	// inside the displayed window.
	band := vizPoints(t, chart.ChannelPoints)
	if len(band) != 10 {
		t.Fatalf("channel polygon: got %d points, want 10", len(band))
	}
	if band[0] != [2]int{0, -32} || band[1] != [2]int{179, -32} || band[2] != [2]int{359, 27} {
		t.Fatalf("channel upper edge: %v..., want 0,-32 179,-32 359,27", band[:3])
	}

	// The strip: volume bars plus the orders line on its own
	// scale (max order count 34 on day 24 -> the band's top).
	if len(chart.Bars) != 5 {
		t.Fatalf("volume bars: got %d, want 5", len(chart.Bars))
	}
	orders := vizPoints(t, chart.OrdersPoints)
	if len(orders) != 5 || orders[0] != [2]int{0, 150} || orders[4] != [2]int{719, 141} {
		t.Fatalf("orders points: %v, want 5 points from 0,150 to 719,141", orders)
	}
	// The recent-days table keeps its full span: newest 14 of all
	// rows read, not just the 5 displayed.
	if len(chart.Recent) != 14 || chart.Recent[0].Date != rows[24].Date || chart.Recent[13].Date != rows[11].Date {
		t.Fatalf("recent table: %d rows from %v, want 14 ending at %s", len(chart.Recent), chart.Recent, rows[11].Date)
	}

	// Every layer off: only the average line remains, and the
	// price band takes the whole plot height.
	bare, ok := markethistory.BuildPriceChartRange(rows, 5, markethistory.ChartOptions{})
	if !ok || bare.Points == "" {
		t.Fatal("bare chart: not built or no average line")
	}
	if len(bare.Wicks) != 0 || bare.MA5Points != "" || bare.MA20Points != "" ||
		bare.ChannelPoints != "" || bare.OrdersPoints != "" || len(bare.Bars) != 0 {
		t.Fatal("bare chart: a switched-off layer still drew")
	}
	if bare.BaseY != 214 {
		t.Fatalf("bare chart BaseY: got %d, want 214 (full plot height)", bare.BaseY)
	}

	// Prefs: defaults, explicit offs, unknown keys, corrupt input.
	if p := parseVizPrefs(""); p != defaultVizPrefs() {
		t.Fatalf("empty prefs: got %+v, want defaults", p)
	}
	if p := parseVizPrefs(`{"wicks": false, "later_slice": true}`); p.Wicks || !p.MAChannel || !p.VolumeStrip {
		t.Fatalf("stored prefs: got %+v, want only wicks off", p)
	}
	if p := parseVizPrefs("{not json"); p != defaultVizPrefs() {
		t.Fatalf("corrupt prefs: got %+v, want defaults", p)
	}

	// Save/load round trip through the real router.
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture")
	code, _ := postForm(t, app, cookie, "/settings/visualizations", url.Values{
		"ma_channel": {"1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("save prefs: status %d, want 303", code)
	}
	if got := app.vizPrefsFor(ctx, user.ID); got != (vizPrefs{MAChannel: true}) {
		t.Fatalf("round trip: got %+v, want only ma_channel on", got)
	}
	code, _ = postForm(t, app, cookie, "/settings/visualizations", url.Values{
		"ma_channel": {"1"}, "wicks": {"1"}, "volume_strip": {"1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("save all-on prefs: status %d, want 303", code)
	}
	if got := app.vizPrefsFor(ctx, user.ID); got != defaultVizPrefs() {
		t.Fatalf("round trip all-on: got %+v, want defaults", got)
	}
}
