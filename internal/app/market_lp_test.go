package app

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// An offer's ISK per LP: product value at the typical price, less
// the ISK cost and the required items at their typical sell, over
// the LP; blueprint copies and offers needing an unpriced item stay
// unpriced.
func TestRefreshLPOfferValues(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := app.db.ExecContext(ctx, s, args...); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
	}
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (1, 'Charges', 8), (2, 'Blueprints', 9) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id) VALUES (100, 'Ammo', 1), (101, 'Ammo Blueprint', 2), (34, 'Tritanium', 1), (999, 'Rare', 1) ON CONFLICT DO NOTHING`)
	for _, s := range []struct {
		typeID    int64
		sell, buy float64
	}{{100, 50, 40}, {101, 1e9, 9e8}, {34, 5, 4}} {
		exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at) VALUES (10000002, $1, $2, $3, $4)`, s.typeID, s.sell, s.buy, now)
	}
	// 100 ammo for 1,000 LP and 1,000 ISK plus 200 Tritanium:
	// (100*50 - 1000 - 200*5) / 1000 = 3.0 on the sell side, 2.0 instant.
	exec(`INSERT INTO lp_offers VALUES (1, 1, 100, 100, 1000, 1000, 0), (1, 2, 101, 1, 1000, 0, 0), (1, 3, 100, 100, 1000, 0, 0)`)
	exec(`INSERT INTO lp_offer_requirements VALUES (1, 1, 34, 200), (1, 3, 999, 1)`)
	if err := app.refreshLPOfferValues(ctx, q, 10000002, now); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rows, err := q.ListLPOfferValues(ctx, db.ListLPOfferValuesParams{RegionID: 10000002, RowCap: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows without a store row = %d, want 0 (the store join)", len(rows))
	}
	exec(`INSERT INTO lp_store_corps (corporation_id, name, offer_count) VALUES (1, 'Store', 3)`)
	rows, err = q.ListLPOfferValues(ctx, db.ListLPOfferValuesParams{RegionID: 10000002, RowCap: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].OfferID != 1 {
		t.Fatalf("priced rows = %+v, want offer 1 only", rows)
	}
	if got := rows[0].IskPerLpSell.Float64; math.Abs(got-3) > 1e-9 {
		t.Errorf("ISK per LP (sell) = %v, want 3", got)
	}
	if got := rows[0].IskPerLpBuy.Float64; math.Abs(got-2) > 1e-9 {
		t.Errorf("ISK per LP (instant) = %v, want 2", got)
	}
	n, err := q.CountLPOfferValuesUnpriced(ctx, db.CountLPOfferValuesUnpricedParams{RegionID: 10000002})
	if err != nil || n != 2 {
		t.Errorf("unpriced = %d (%v), want 2 (blueprint copy, unpriced requirement)", n, err)
	}
}

// The browser's Fuzzwork-parity surface: ISK-per-LP and LP-cost
// range filters bind in SQL, an item name finds the stores selling
// it across corporations, and the CSV and JSON exports carry the
// same filters at the export cap.
func TestMarketLPFiltersAndExports(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := app.db.ExecContext(ctx, s, args...); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
	}
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (1, 'Charges', 8) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (100, 'Ammo', 1, 1, 1), (200, 'Implant', 1, 1, 1), (34, 'Tritanium', 1, 1, 1) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO lp_store_corps (corporation_id, name, offer_count) VALUES (1, 'Store One', 2), (2, 'Store Two', 1)`)
	// Store One's Ammo also needs 200 Tritanium, whose stored
	// averages (not its typicals) are what the icon tooltip shows.
	exec(`INSERT INTO lp_offer_requirements VALUES (1, 1, 34, 200)`)
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, sell_avg, buy_avg, updated_at) VALUES (10000002, 34, 5, 4, 6.5, 4.25, $1)`, time.Now().UTC())
	// Ammo at Store One: 3.0 ISK/LP for 1,000 LP. Implant at Store
	// One: 900 ISK/LP for 50,000 LP. Ammo also at Store Two: 2.5.
	now := time.Now().UTC()
	exec(`INSERT INTO lp_offer_values (region_id, corporation_id, offer_id, type_id, quantity, lp_cost, isk_cost, required_cost, unit_sell, unit_buy, isk_per_lp_sell, isk_per_lp_buy, daily_volume, prices_at, updated_at) VALUES
		(10000002, 1, 1, 100, 100, 1000, 1000, 1300, 50, 40, 3.0, 2.0, 500, $1, $1),
		(10000002, 1, 2, 200, 1, 50000, 0, 0, 45000000, 44000000, 900.0, 880.0, 30, $1, $1),
		(10000002, 2, 1, 100, 100, 1000, 1000, 0, 45, 38, 2.5, 1.8, 500, $1, $1)`, now)

	view := app.buildMarketLPView(ctx, map[string][]string{})
	if len(view.Rows) != 3 {
		t.Fatalf("unfiltered rows = %d, want 3", len(view.Rows))
	}
	if view.Rows[0].ISKPerLPDir != "up" {
		t.Fatalf("best row dir = %q, want up", view.Rows[0].ISKPerLPDir)
	}
	for _, row := range view.Rows {
		if row.TypeID != 100 || row.StoreID != 1 {
			continue
		}
		if len(row.Requirements) != 1 {
			t.Fatalf("ammo requirements = %+v, want the one Tritanium need", row.Requirements)
		}
		req := row.Requirements[0]
		if req.TipLines != "Needs|200;Avg sell|6.50;Avg buy|4.25;Items total|1,300.00" {
			t.Errorf("requirement tooltip = %q, want the stored averages", req.TipLines)
		}
		if !strings.Contains(req.IconURL, "/types/34/icon") || !strings.Contains(req.Href, "type=34") {
			t.Errorf("requirement icon/link = %q / %q, want type 34", req.IconURL, req.Href)
		}
	}
	view = app.buildMarketLPView(ctx, map[string][]string{"minisklp": {"100"}})
	if len(view.Rows) != 1 || view.Rows[0].TypeID != 200 {
		t.Fatalf("minisklp rows = %+v, want only the implant", view.Rows)
	}
	view = app.buildMarketLPView(ctx, map[string][]string{"maxlp": {"5000"}})
	if len(view.Rows) != 2 {
		t.Fatalf("maxlp rows = %d, want the two 1,000 LP offers", len(view.Rows))
	}
	view = app.buildMarketLPView(ctx, map[string][]string{"item": {"Ammo"}})
	if len(view.Rows) != 2 || view.Rows[0].StoreID != 1 || view.Rows[1].StoreID != 2 {
		t.Fatalf("find-stores rows = %+v, want Ammo at both stores", view.Rows)
	}
	view = app.buildMarketLPView(ctx, map[string][]string{"item": {"No Such Item"}})
	if len(view.Rows) != 0 {
		t.Fatalf("unknown item rows = %d, want 0", len(view.Rows))
	}

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_ = user
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Tester")
	code, body := getPage(t, app, cookie, "/market/lp/export.csv?minisklp=100")
	if code != 200 || !strings.Contains(body, "Implant") || strings.Contains(body, "Ammo") {
		t.Fatalf("csv export: code %d body %.200q", code, body)
	}
	code, body = getPage(t, app, cookie, "/market/lp/export.json?item=Ammo")
	if code != 200 || !strings.Contains(body, `"isk_per_lp": 3`) || !strings.Contains(body, "Store Two") {
		t.Fatalf("json export: code %d body %.300q", code, body)
	}
}
