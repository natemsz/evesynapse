package app

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"evesynapse/internal/esi"
)

func nw(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

// TestNetWorthDaysSplitTheTotal: a day's total splits into the ISK on
// hand and the rest; characters with no computed net worth that day
// contribute neither part; the parts always add up to the total.
func TestNetWorthDaysSplitTheTotal(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedWalletSample(t, q, user.ID, 1, "2026-03-01", 100, nw(1000))
	seedWalletSample(t, q, user.ID, 2, "2026-03-01", 50, nw(250))
	seedWalletSample(t, q, user.ID, 3, "2026-03-01", 999, sql.NullFloat64{}) // unknown: not counted at all
	seedWalletSample(t, q, user.ID, 1, "2026-03-02", 5000, sql.NullFloat64{})
	seedWalletSample(t, q, user.ID, 1, "2026-03-03", 300, nw(200)) // balance read after the net worth: assets clamp at 0
	days, err := app.netWorthDays(ctx, user.ID, 30)
	if err != nil {
		t.Fatalf("netWorthDays: %v", err)
	}
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

// TestHomeNetWorthShowsTheBreakdown: the home Net Worth widget draws
// the stacked chart, wallets under the rest, with its legend, per-day
// tooltip figures and the table twin.
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
	setHomeLayout(t, q, user.ID, `[{"id":"networth","w":2,"h":2}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/", body,
		`class="w-area w-s1"`, `class="w-area w-s2"`, `aria-label="Net worth over 30 days, wallets and the rest stacked"`,
		`<span class="w-key"><span class="w-sw w-s1" aria-hidden="true"></span>Wallets</span>`, "Assets and orders",
		`data-tip-rows="`, `<th scope="row">2026-03-02</th><td>120.00</td><td>1,280.00</td><td>1,400.00</td>`)
}
