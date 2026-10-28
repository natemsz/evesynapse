package app

import (
	"context"
	"math"
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
