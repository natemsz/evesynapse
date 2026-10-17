package tests

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

// The item page's price-history sparkline: the polyline must
// carry its own stroke. It once relied on a CSS rule that does
// not exist, so the chart rendered as an empty box under its
// heading while the page claimed recorded days.
func TestItemPageHistorySparklinePaints(t *testing.T) {
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
	if !strings.Contains(body, "Price history") {
		t.Fatal("item page has no price history section")
	}
	if !strings.Contains(body, `class="spark"`) {
		t.Fatal("item page renders no sparkline svg")
	}
	if !strings.Contains(body, `stroke="#ffb84d"`) {
		t.Error("sparkline polyline carries no stroke: the chart paints nothing")
	}
	if !strings.Contains(body, "3 recorded days") {
		t.Error("item page does not note the recorded days under the chart")
	}
	if !strings.Contains(body, "The Forge") {
		t.Error("item page does not show the hub prices")
	}
}
