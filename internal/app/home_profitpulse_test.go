package app

// Tests for the Profit pulse home widget: the figures come from
// the stored profit_daily rows (the unattributed remainder excluded,
// as on the Profit page), the change from the same rows over the
// window before, the items from the stored trade_item_window rows,
// the daily bars from profit_daily bucketed per day, and the
// waterfall from the stored trade_item_daily cost rows, drawn by the
// Profit page's own chart builder. Each form builds only what it
// draws. Same hermetic contract as the other home suites: seeded
// stored state, zero outbound calls.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// seedProfitPulse plants the stored profit rows for one character:
// trading 1,000,000 in / 400,000 out, industry 200,000 / 50,000,
// bounties 100,000 / 0, plus an unattributed 999,999,999 that the
// headline must leave out (gross 1,300,000, kept 850,000). The top
// item is Tritanium (margin 185,000 with the default prefs' broker
// fees in the margin) over Pyerite (39,000); the daily trade rows
// carry 300,000 of covered cost and a 50,000 uncovered sale, the
// waterfall's anchors.
func seedProfitPulse(t *testing.T, app *Application, q *db.Queries, characterID int64) {
	t.Helper()
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")
	daily := []struct {
		activity      string
		income, costs float64
	}{
		{"trading", 1000000, 400000},
		{"industry", 200000, 50000},
		{"bounties", 100000, 0},
		{"unattributed", 999999999, 0},
	}
	for _, r := range daily {
		if _, err := app.db.ExecContext(ctx,
			`INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs) VALUES ('char', $1, $2, $3, $4, $5)`,
			characterID, day, r.activity, r.income, r.costs); err != nil {
			t.Fatalf("seed profit_daily %s: %v", r.activity, err)
		}
	}
	for _, n := range []struct {
		id   int64
		name string
	}{{34, "Tritanium"}, {35, "Pyerite"}} {
		if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: n.id, Name: n.name}); err != nil {
			t.Fatalf("seed type name %d: %v", n.id, err)
		}
	}
	window := []struct {
		typeID                       int64
		units, covUnits              int64
		revenue, covRevenue, covCost float64
		covTax, broker               float64
	}{
		{34, 100, 100, 500000, 500000, 300000, 10000, 5000},
		{35, 10, 10, 60000, 60000, 20000, 1000, 0},
	}
	for _, r := range window {
		if _, err := app.db.ExecContext(ctx,
			`INSERT INTO trade_item_window (owner_kind, owner_id, win, type_id, as_of_day, units, revenue, cov_units, cov_revenue, cov_cost, cov_tax, broker)
			 VALUES ('char', $1, 30, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			characterID, r.typeID, day, r.units, r.revenue, r.covUnits, r.covRevenue, r.covCost, r.covTax, r.broker); err != nil {
			t.Fatalf("seed trade_item_window %d: %v", r.typeID, err)
		}
	}
	if _, err := app.db.ExecContext(ctx,
		`INSERT INTO trade_item_daily (owner_kind, owner_id, day, type_id, units, revenue, cov_units, cov_revenue, cov_cost, cov_tax, broker)
		 VALUES ('char', $1, $2, 34, 100, 500000, 100, 500000, 300000, 10000, 5000),
		        ('char', $1, $2, 999, 5, 50000, 0, 0, 0, 0, 0)`,
		characterID, day); err != nil {
		t.Fatalf("seed trade_item_daily: %v", err)
	}
}

func TestProfitPulseWidgetSizes(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "")
	seedProfitPulse(t, app, q, fixtureCharA)
	// The 30 days before the window kept 100,000: the change is +750,000.
	if _, err := app.db.ExecContext(ctx,
		`INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs) VALUES ('char', $1, $2, 'trading', 100000, 0)`,
		fixtureCharA, time.Now().UTC().AddDate(0, 0, -40).Format("2006-01-02")); err != nil {
		t.Fatalf("seed previous window: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	chars, err := q.ListCharactersByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list characters: %v", err)
	}
	def, _ := widgetDefFor(widgetProfitPulse)
	build := func(form string) *profitPulseView {
		in := &widgetInput{UserID: user.ID, Forms: []string{form}, Opts: def.defaultOptions(), Chars: chars, Now: time.Now().UTC()}
		return buildProfitPulseWidget(ctx, app, in).(*profitPulseView)
	}

	// Kept is the labeled activities only, and its change is against
	// the window before, read from the same stored rows.
	small := build("small")
	if small.Lead.Value != "+850.00K" || small.Lead.Change != "+750.00K" || small.Gross != "1.30M ISK" {
		t.Errorf("small figures kept=%q change=%q gross=%q, want +850.00K, +750.00K, 1.30M ISK", small.Lead.Value, small.Lead.Change, small.Gross)
	}
	// Every seeded row sits on today: one bar, above zero, on the last day.
	if small.Strip == nil || len(small.Strip.Bars) != 1 || small.Strip.Bars[0].Down || small.Strip.Bars[0].X < 950 {
		t.Errorf("small strip = %+v, want one bar above zero on the last day", small.Strip)
	}
	if small.Days != nil || small.Waterfall != nil {
		t.Errorf("small form built the day chart or the waterfall")
	}
	// The medium form ranks the items by margin, broker fees in.
	if mid := build("medium"); mid.TopItems == nil || len(mid.TopItems.Rows) != 2 ||
		mid.TopItems.Rows[0].Name != "Tritanium" || mid.TopItems.Rows[0].Text != "+185.00K" || mid.Waterfall != nil {
		t.Errorf("medium top items = %+v, want Tritanium +185.00K then Pyerite, and no waterfall", mid.TopItems)
	}
	// in.Draws("large") decides the waterfall and the activity split.
	if large := build("large"); large.Waterfall == nil || large.Activities == nil || large.Activities.Rows[0].Name != "Trading" ||
		large.Activities.Rows[0].Text != "+600.00K" {
		t.Errorf("large waterfall=%t activities=%+v, want the waterfall and Trading first at +600.00K", large.Waterfall != nil, large.Activities)
	}

	for _, c := range []struct {
		layout string
		want   []string
	}{
		{`[{"id":"profitpulse","w":1,"h":1}]`, []string{"Kept as profit", "850.00K", "w-barchart"}},
		{`[{"id":"profitpulse"}]`, []string{"Gross in", "Best margins", "Tritanium", "w-barchart"}},
		{`[{"id":"profitpulse","w":3,"h":3}]`, []string{"Where the ISK went", "was kept as profit", "COGS", "Net earned", "By activity",
			// The chart labels position by pf-x/pf-y classes, as on
			// the Profit page; inline styles are blocked by the CSP.
			"pf-x-", "pf-y-"}},
	} {
		setHomeLayout(t, q, user.ID, c.layout)
		code, body := getPage(t, app, cookie, "/")
		if code != http.StatusOK {
			t.Fatalf("GET / (%s): status %d", c.layout, code)
		}
		mustContain(t, c.layout, body, append([]string{`data-widget="profitpulse"`}, c.want...)...)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Errorf("home render made %d outbound calls, want 0", got)
	}
}

func TestProfitPulseWidgetEmpty(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "")
	setHomeLayout(t, q, user.ID, `[{"id":"profitpulse"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, `data-widget="profitpulse"`, "No profit yet")
	if at := strings.Index(body, `class="card-body"`); at < 0 || strings.Contains(body[at:], "Gross in") {
		t.Errorf("empty widget shows figures, want the empty state only")
	}
}
