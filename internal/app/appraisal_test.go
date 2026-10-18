package app

// The appraisal core's order, floor rule and deal gating,
// plus the worker pass that stamps them onto stored
// contracts (appraisal.go, schema 056).

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

func TestResolveAppraisalPriceOrder(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	freshOwn := &db.MarketRegionStat{TypeID: 1, TypicalSell: 100, UpdatedAt: now.Add(-time.Hour)}
	staleOwn := &db.MarketRegionStat{TypeID: 1, TypicalSell: 100, UpdatedAt: now.Add(-10 * 24 * time.Hour)}
	zeroOwn := &db.MarketRegionStat{TypeID: 1, TypicalSell: 0, UpdatedAt: now.Add(-time.Hour)}
	tv := &db.TrueValue{TypeID: 1, EstSell: sql.NullFloat64{Float64: 80, Valid: true}, ComputedAt: now.Add(-2 * time.Hour)}
	tvNone := &db.TrueValue{TypeID: 1, ComputedAt: now}
	freshFB := &db.MarketStatsFallback{TypeID: 1, MedianSell: 60, SourceAsof: now.Add(-time.Hour)}
	expiredFB := &db.MarketStatsFallback{TypeID: 1, MedianSell: 60, SourceAsof: now.Add(-72 * time.Hour)}

	cases := []struct {
		name     string
		own      *db.MarketRegionStat
		tv       *db.TrueValue
		fb       *db.MarketStatsFallback
		wantTier string
		wantUnit float64
		wantOK   bool
	}{
		{"fresh own beats everything", freshOwn, tv, freshFB, module.AppraisalTierTypical, 100, true},
		{"stale own yields to the estimate", staleOwn, tv, freshFB, module.AppraisalTierEstimate, 80, true},
		{"estimate beats a live fallback", nil, tv, freshFB, module.AppraisalTierEstimate, 80, true},
		{"fallback speaks when nothing else does", staleOwn, tvNone, freshFB, module.AppraisalTierFallback, 60, true},
		{"floor rule: stale own over an expired fallback", staleOwn, nil, expiredFB, module.AppraisalTierTypical, 100, true},
		{"floor rule: stale own alone", staleOwn, nil, nil, module.AppraisalTierTypical, 100, true},
		{"expired fallback alone is silence", nil, nil, expiredFB, module.AppraisalTierUnpriced, 0, false},
		{"nothing stored is unpriced", nil, nil, nil, module.AppraisalTierUnpriced, 0, false},
		{"a zero typical is no typical", zeroOwn, nil, nil, module.AppraisalTierUnpriced, 0, false},
	}
	for _, tc := range cases {
		got := resolveAppraisalPrice(tc.own, tc.tv, tc.fb, now)
		if got.tier != tc.wantTier || got.unit != tc.wantUnit || got.ok != tc.wantOK {
			t.Errorf("%s: got {%s %v %v}, want {%s %v %v}",
				tc.name, got.tier, got.unit, got.ok, tc.wantTier, tc.wantUnit, tc.wantOK)
		}
		if tc.wantOK && got.asOf.IsZero() {
			t.Errorf("%s: a priced line must carry its tier's as-of", tc.name)
		}
	}
	// The floor rule's as-of is the stale row's own stamp: the
	// reader sees the age, not a freshened date.
	got := resolveAppraisalPrice(staleOwn, nil, expiredFB, now)
	if !got.asOf.Equal(staleOwn.UpdatedAt) {
		t.Errorf("floor rule as-of = %v, want the stale row's %v", got.asOf, staleOwn.UpdatedAt)
	}
}

func TestAppraisalDealRatioGating(t *testing.T) {
	full := module.Appraisal{OfferedValue: 1000, PricedOffered: 2, OfferedLines: 2}
	partial := module.Appraisal{OfferedValue: 1000, PricedOffered: 1, OfferedLines: 2}

	exchange := db.PubContract{Type: "item_exchange", Price: 850}
	if ratio, ok := appraisalDealRatio(exchange, full); !ok || ratio != 0.85 {
		t.Errorf("full-coverage exchange: got %v/%v, want 0.85/true", ratio, ok)
	}
	if !appraisalDealFlag(sql.NullFloat64{Float64: 0.85, Valid: true}) {
		t.Error("a ratio of exactly 0.85 must flag (the line is at or under)")
	}
	if _, ok := appraisalDealRatio(exchange, partial); ok {
		t.Error("partial coverage must store no ratio")
	}
	if _, ok := appraisalDealRatio(db.PubContract{Type: "auction", Buyout: 0}, full); ok {
		t.Error("an auction without a buyout has no ask to judge")
	}
	if ratio, ok := appraisalDealRatio(db.PubContract{Type: "auction", Buyout: 500}, full); !ok || ratio != 0.5 {
		t.Errorf("auction judges its buyout: got %v/%v, want 0.5/true", ratio, ok)
	}
	if _, ok := appraisalDealRatio(db.PubContract{Type: "courier"}, full); ok {
		t.Error("couriers carry no contents value to judge")
	}
	if _, ok := appraisalDealRatio(exchange, module.Appraisal{}); ok {
		t.Error("no offered lines, no ratio")
	}

	// The wanted side is cost, not decoration: an exchange's
	// ratio runs over price plus the valued wants, gated on
	// full wanted coverage exactly like the offered side.
	withWants := module.Appraisal{OfferedValue: 1000, PricedOffered: 2, OfferedLines: 2,
		WantValue: 500, PricedWanted: 1, WantedLines: 1}
	if ratio, ok := appraisalDealRatio(db.PubContract{Type: "item_exchange", Price: 350}, withWants); !ok || ratio != 0.85 {
		t.Errorf("exchange with priced wants: got %v/%v, want 0.85/true ((350+500)/1000)", ratio, ok)
	}
	if ratio, ok := appraisalDealRatio(db.PubContract{Type: "item_exchange", Price: 0}, withWants); !ok || ratio != 0.5 {
		t.Errorf("wants can carry the whole cost: got %v/%v, want 0.5/true", ratio, ok)
	}
	// The owner's case: 1 ISK plus wants worth two and a half
	// times the contents is no deal at any threshold.
	barter := module.Appraisal{OfferedValue: 1000, PricedOffered: 1, OfferedLines: 1,
		WantValue: 2499, PricedWanted: 1, WantedLines: 1}
	ratio, ok := appraisalDealRatio(db.PubContract{Type: "item_exchange", Price: 1}, barter)
	if !ok || ratio != 2.5 {
		t.Errorf("1 ISK + 2499 of wants over 1000 offered: got %v/%v, want 2.5/true", ratio, ok)
	}
	if appraisalDealFlag(sql.NullFloat64{Float64: ratio, Valid: ok}) {
		t.Error("a barter costing 250% of value must never flag as a deal")
	}
	// An unpriced wanted line leaves the cost unknowable: no
	// ratio, however cheap the ISK ask looks.
	unpricedWant := module.Appraisal{OfferedValue: 1000, PricedOffered: 1, OfferedLines: 1,
		WantedLines: 1}
	if _, ok := appraisalDealRatio(db.PubContract{Type: "item_exchange", Price: 1}, unpricedWant); ok {
		t.Error("an unpriced wanted line must store no ratio")
	}
	// A free exchange with no wants still has no cost to judge.
	if _, ok := appraisalDealRatio(db.PubContract{Type: "item_exchange", Price: 0}, full); ok {
		t.Error("a zero-cost exchange has no ratio")
	}
}

// TestAppraisePublicContractsEndToEnd runs the worker pass
// over seeded pricing tables and checks the stored results
// line by line.
func TestAppraisePublicContractsEndToEnd(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	ctx := context.Background()
	now := time.Now().UTC()

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	// Pricing inputs: 1001 fresh own typical 100; 1002 stale
	// own typical with an estimate of 80; 1003 estimate only;
	// 1004 live fallback median 25; 1005 expired fallback and
	// a stale own typical of 10 (the floor rule); 1006 nothing.
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES (10000002, 1001, 100, $1)`, now.Add(-time.Hour))
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES (10000002, 1002, 50, $1)`, now.Add(-10*24*time.Hour))
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES (10000002, 1005, 10, $1)`, now.Add(-10*24*time.Hour))
	exec(`INSERT INTO true_values (type_id, build_cost, est_sell, computed_at) VALUES (1002, 0, 80, $1)`, now)
	exec(`INSERT INTO true_values (type_id, build_cost, est_sell, computed_at) VALUES (1003, 0, 30, $1)`, now)
	fb := func(typeID int64, median float64, asof time.Time) db.InsertMarketStatsFallbackParams {
		return db.InsertMarketStatsFallbackParams{
			RegionID: 10000002, TypeID: typeID, MedianSell: median, FivepctSell: median,
			Source: "fuzzwork", SourceAsof: asof, FetchedAt: asof,
		}
	}
	if err := q.InsertMarketStatsFallback(ctx, fb(1004, 25, now.Add(-time.Hour))); err != nil {
		t.Fatalf("seed fallback 1004: %v", err)
	}
	if err := q.InsertMarketStatsFallback(ctx, fb(1005, 99, now.Add(-72*time.Hour))); err != nil {
		t.Fatalf("seed fallback 1005: %v", err)
	}

	contract := func(id int64, typ string, price, buyout float64) {
		t.Helper()
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, price, buyout,
			date_issued, date_expired, items_state, last_cycle, first_seen, last_seen)
			VALUES ($1, 10000002, $2, $3, $4, $5, $6, 'stored', 1, $5, $5)`,
			id, typ, price, buyout, now.Add(-time.Hour), now.Add(7*24*time.Hour))
	}
	item := func(rec, contractID, typeID, qty int64, included bool) {
		t.Helper()
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included)
			VALUES ($1, $2, $3, $4, $5)`, rec, contractID, typeID, qty, included)
	}
	contract(9101, "item_exchange", 850, 0) // asks exactly 85% of 1000
	item(1, 9101, 1001, 10, true)
	contract(9102, "item_exchange", 100, 0) // partial coverage
	item(2, 9102, 1002, 2, true)
	item(3, 9102, 1006, 1, true)
	item(4, 9102, 1003, 3, false)    // wanted side prices too
	contract(9103, "auction", 0, 20) // buyout 20 against a fallback 25
	item(5, 9103, 1004, 1, true)
	contract(9104, "item_exchange", 5, 0) // floor-rule line
	item(6, 9104, 1005, 1, true)
	contract(9105, "item_exchange", 1, 0) // 1 ISK + priced wants: a real, small deal
	item(7, 9105, 1001, 10, true)
	item(8, 9105, 1003, 3, false)
	contract(9106, "item_exchange", 1, 0) // 1 ISK + an unpriceable want: no ratio
	item(9, 9106, 1001, 10, true)
	item(10, 9106, 1006, 1, false)

	if n := app.appraisePublicContracts(ctx); n != 6 {
		t.Fatalf("appraised %d contracts, want 6", n)
	}

	c1, err := q.GetPublicContract(ctx, 9101)
	if err != nil {
		t.Fatalf("read 9101: %v", err)
	}
	if c1.OfferedValue != 1000 || c1.PricedLines != 1 || c1.TotalLines != 1 {
		t.Errorf("9101 totals = %v (%d/%d), want 1000 (1/1)", c1.OfferedValue, c1.PricedLines, c1.TotalLines)
	}
	if !c1.DealRatio.Valid || c1.DealRatio.Float64 != 0.85 || !appraisalDealFlag(c1.DealRatio) {
		t.Errorf("9101 ratio = %v, want a flagged 0.85", c1.DealRatio)
	}
	if c1.TierMix != "typical:1" || !c1.PricedAt.Valid {
		t.Errorf("9101 mix/stamp = %q/%v, want typical:1 and a stamp", c1.TierMix, c1.PricedAt)
	}

	c2, err := q.GetPublicContract(ctx, 9102)
	if err != nil {
		t.Fatalf("read 9102: %v", err)
	}
	if c2.OfferedValue != 160 || c2.WantValue != 90 {
		t.Errorf("9102 values = %v/%v, want 160 offered / 90 wanted", c2.OfferedValue, c2.WantValue)
	}
	if c2.PricedLines != 1 || c2.TotalLines != 2 || c2.DealRatio.Valid {
		t.Errorf("9102 coverage = %d/%d ratio %v, want 1/2 and no ratio",
			c2.PricedLines, c2.TotalLines, c2.DealRatio)
	}
	if c2.TierMix != "estimate:2" {
		t.Errorf("9102 mix = %q, want estimate:2", c2.TierMix)
	}

	c3, err := q.GetPublicContract(ctx, 9103)
	if err != nil {
		t.Fatalf("read 9103: %v", err)
	}
	if !c3.DealRatio.Valid || c3.DealRatio.Float64 != 0.8 {
		t.Errorf("9103 ratio = %v, want 0.8 (buyout over fallback value)", c3.DealRatio)
	}

	// 9105: the cost side is 1 ISK plus 3 x 30 of wanted
	// estimate -- 91 against 1000 offered, a genuine deal the
	// old ISK-only math would have read as 0% of value.
	c5, err := q.GetPublicContract(ctx, 9105)
	if err != nil {
		t.Fatalf("read 9105: %v", err)
	}
	if c5.OfferedValue != 1000 || c5.WantValue != 90 {
		t.Errorf("9105 values = %v/%v, want 1000 offered / 90 wanted", c5.OfferedValue, c5.WantValue)
	}
	if !c5.DealRatio.Valid || c5.DealRatio.Float64 != 0.091 || !appraisalDealFlag(c5.DealRatio) {
		t.Errorf("9105 ratio = %v, want a flagged 0.091 ((1+90)/1000)", c5.DealRatio)
	}

	// 9106: the wanted line prices nowhere, so the contract
	// stores no ratio -- a 1-ISK ask must not pose as a deal
	// when part of its price is an item we cannot value.
	c6, err := q.GetPublicContract(ctx, 9106)
	if err != nil {
		t.Fatalf("read 9106: %v", err)
	}
	if c6.WantValue != 0 || c6.DealRatio.Valid {
		t.Errorf("9106 want/ratio = %v/%v, want 0 and no ratio", c6.WantValue, c6.DealRatio)
	}

	items4, err := q.ListPublicContractItems(ctx, 9104)
	if err != nil || len(items4) != 1 {
		t.Fatalf("read 9104 items: %v (%d)", err, len(items4))
	}
	if !items4[0].UnitPrice.Valid || items4[0].UnitPrice.Float64 != 10 ||
		items4[0].PriceTier != module.AppraisalTierTypical {
		t.Errorf("9104 line = %v/%q, want the stale own 10 via the floor rule",
			items4[0].UnitPrice, items4[0].PriceTier)
	}

	// A second pass inside the refresh age reprices nothing.
	if n := app.appraisePublicContracts(ctx); n != 0 {
		t.Errorf("second pass appraised %d, want 0 (nothing due)", n)
	}
}
