package tests

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

// The item browser's price history is the Market page's history
// section — same chart, same states, same warming — not the bare
// sparkline it used to draw. These tests pin both ends of that
// contract: stored rows render the full chart, and no rows render
// the live pending section that polls the shared fragment while
// the view leaves the same want the Market view leaves.

func TestItemPageHistoryChart(t *testing.T) {
	rig := apptest.Build(t, nil)
	conn := rig.DB()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 18, 1, 1)`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (18, 'Mineral', 4)`,
		`INSERT INTO sde_categories (category_id, name) VALUES (4, 'Material')`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
		`INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume) VALUES (10000002, 34, 6.5, 6.4, 7.0, 5.9, 6.0, 5.5, 40, 35, 1000000, 900000, NOW(), 500000)`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES
		   (10000002, 34, '2026-10-13', 5.50, 6, 5, 900000, 90),
		   (10000002, 34, '2026-10-14', 6.00, 6.5, 5.5, 950000, 95),
		   (10000002, 34, '2026-10-15', 6.40, 7, 6, 990000, 99)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	code, body := apptest.GetPage(t, rig, nil, "/items/type/34/")
	if code != 200 {
		t.Fatalf("item page status = %d", code)
	}
	apptest.MustContain(t, "/items/type/34/ (chart)", body,
		"Price history — The Forge",
		`class="pchart"`,
		`data-chart-scrub="true"`,
		"<polyline",
		`stroke="#ffb84d"`,
		"5-day average",
		"Donchian channel",
		"Daily wicks: lowest to highest trade",
		"Volume",
		"3 days of recorded trades in The Forge",
		"Last recorded days",
		"2026-10-15",
		"Hub prices",
	)
	if strings.Contains(body, `class="spark"`) {
		t.Error("item page still renders the old bare sparkline")
	}
	// Per the owner's mock, the price history sits on the page
	// glass below the item card, not inside it: the history
	// heading follows the card's close, while Hub prices stays
	// inside the card.
	cardAt := strings.Index(body, `<section class="card">`)
	cardEnd := cardAt + strings.Index(body[cardAt:], "</section>")
	hubAt := strings.Index(body, "<h3>Hub prices</h3>")
	histAt := strings.Index(body, "<h3>Price history")
	if cardAt < 0 || hubAt < cardAt || hubAt > cardEnd {
		t.Error("hub prices no longer inside the item card")
	}
	if histAt < cardEnd {
		t.Error("price history renders inside the item card; the mock puts it on the glass below the card")
	}
}

func TestItemPageHistoryPendingQueuesWant(t *testing.T) {
	rig := apptest.Build(t, nil)
	conn := rig.DB()
	q := rig.Queries()
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 18, 1, 1)`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (18, 'Mineral', 4)`,
		`INSERT INTO sde_categories (category_id, name) VALUES (4, 'Material')`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
		`INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume) VALUES (10000002, 34, 6.5, 6.4, 7.0, 5.9, 6.0, 5.5, 40, 35, 1000000, 900000, NOW(), 500000)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	code, body := apptest.GetPage(t, rig, cookie, "/items/type/34/")
	if code != 200 {
		t.Fatalf("item page status = %d", code)
	}
	// No stored history: the section is the market's live pending
	// state — it names the shared fragment it will poll until the
	// chart swaps in, without the reader going anywhere.
	apptest.MustContain(t, "/items/type/34/ (pending)", body,
		"Price history — The Forge",
		`data-poll-state="pending"`,
		`data-poll-url="/market/history-fragment?region=10000002&amp;type=34&amp;range=3m"`,
		"This item is queued",
	)
	// And the view leaves the worker the same want a Market view
	// leaves.
	var wants int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM market_history_wants WHERE region_id = 10000002 AND type_id = 34`,
	).Scan(&wants); err != nil {
		t.Fatalf("count wants: %v", err)
	}
	if wants != 1 {
		t.Fatalf("market_history_wants rows = %d, want 1", wants)
	}
}
