package app

// Tests for the Profit pulse home widget: the headline figures
// come from the stored profit_daily rows (the unattributed
// remainder excluded, as on the Profit page), the top item from
// the stored trade_item_window rows, and the waterfall from the
// stored trade_item_daily cost rows, drawn by the Profit page's
// own chart builder. How much shows follows the widget's stored
// size: figures only at the smallest size, the top item added at
// the default size, the waterfall added at a larger size. Same
// hermetic contract as the other home suites: seeded stored state,
// zero outbound calls.

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
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// Smallest size: the two headline figures only.
	setHomeLayout(t, q, user.ID, `[{"id":"profitpulse","w":1,"h":1}]`)
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (small): status %d", code)
	}
	mustContain(t, "/", body,
		`data-widget="profitpulse"`, "Profit pulse",
		"Last 30 days", "Gross in", "1,300,000.00", "Kept as profit", "850,000.00",
	)
	for _, absent := range []string{"Top item", "Where the ISK went"} {
		if strings.Contains(body, absent) {
			t.Errorf("small widget shows %q, want figures only", absent)
		}
	}

	// Default size (2x2): the top item joins the figures.
	setHomeLayout(t, q, user.ID, `[{"id":"profitpulse"}]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (default): status %d", code)
	}
	mustContain(t, "/", body,
		"Top item", "Tritanium", "185,000.00",
	)
	if strings.Contains(body, "Where the ISK went") {
		t.Errorf("default-size widget shows the waterfall, want it only at a larger size")
	}

	// Largest size: the waterfall joins, drawn from the same span
	// totals the Profit page walks (fees residual 100,000).
	setHomeLayout(t, q, user.ID, `[{"id":"profitpulse","w":3,"h":3}]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (large): status %d", code)
	}
	mustContain(t, "/", body,
		"Top item", "Where the ISK went", "was kept as profit",
		"Capital recycled (COGS)", "Industry costs", "Fees, taxes and other outflows", "Net earned",
		"Sales with no cost basis",
		// The chart labels position by pf-x/pf-y classes, as on
		// the Profit page; inline styles are blocked by the CSP.
		"pf-x-", "pf-y-",
	)

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
	if strings.Contains(body, "Gross in") {
		t.Errorf("empty widget shows figures, want the empty state only")
	}
}
