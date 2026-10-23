package app

// Blueprint copy pricing (schema 061): a copy prices only
// from the stored copy medians -- the median ask of
// single-copy contracts for its type and runs -- and never
// from the type-level order, whose every figure for a
// blueprint type is the original's price. The fixtures
// mirror the live diagnosis: a Procurer-class blueprint
// whose original's Forge typical is 1,558,500,000 while its
// 10-run copies list at a 1,290,000 median.

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// seedCopyCorpus stores public contracts directly: the
// median pass reads the stored corpus, so the sweep is not
// under test here.
func seedCopyCorpus(t *testing.T, conn *sql.DB) (contract func(id int64, typ string, price float64), copyItem func(rec, contractID, typeID int64, runs any, included, isCopy bool, qty int64)) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	contract = func(id int64, typ string, price float64) {
		t.Helper()
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, price,
			date_issued, date_expired, items_state, last_cycle, first_seen, last_seen)
			VALUES ($1, 10000002, $2, $3, $4, $5, 'stored', 1, $4, $4)`,
			id, typ, price, now.Add(-time.Hour), now.Add(7*24*time.Hour))
	}
	copyItem = func(rec, contractID, typeID int64, runs any, included, isCopy bool, qty int64) {
		t.Helper()
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity,
			is_included, is_blueprint_copy, runs, material_efficiency, time_efficiency)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 10, 20)`,
			rec, contractID, typeID, qty, included, isCopy, runs)
	}
	return contract, copyItem
}

func TestBlueprintCopyMedianPass(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	ctx := context.Background()
	contract, copyItem := seedCopyCorpus(t, conn)

	// (17481, 10 runs): five single-copy listings; the
	// median of 0.8/1.0/1.29/2.0/2.5M is 1.29M exactly --
	// the live corpus's Procurer figure.
	for i, ask := range []float64{800_000, 1_000_000, 1_290_000, 2_000_000, 2_500_000} {
		id := int64(8001 + i)
		contract(id, "item_exchange", ask)
		copyItem(int64(100+i), id, 17481, int64(10), true, true, 1)
	}
	// (17481, 5 runs): its own stratum, median 300,000.
	for i, ask := range []float64{100_000, 200_000, 300_000, 400_000, 500_000} {
		id := int64(8011 + i)
		contract(id, "item_exchange", ask)
		copyItem(int64(120+i), id, 17481, int64(5), true, true, 1)
	}
	// (17482, 10 runs): four listings -- under the minimum
	// sample, so no row may be stored.
	for i := 0; i < 4; i++ {
		id := int64(8021 + i)
		contract(id, "item_exchange", 900_000)
		copyItem(int64(140+i), id, 17482, int64(10), true, true, 1)
	}
	// Exclusions, each aimed at the (17481, 10) group with a
	// junk ask that would drag its median if it leaked in.
	contract(8031, "item_exchange", 999_000_000) // multi-line
	copyItem(160, 8031, 17481, int64(10), true, true, 1)
	copyItem(161, 8031, 34, nil, true, false, 100)
	contract(8032, "item_exchange", 888_000_000) // wanted side only
	copyItem(162, 8032, 17481, int64(10), false, true, 1)
	contract(8033, "auction", 777_000_000) // an auction's start price is no ask for its copy
	copyItem(163, 8033, 17481, int64(10), true, true, 1)
	contract(8034, "item_exchange", 666_000_000) // an original, not a copy
	copyItem(164, 8034, 17481, nil, true, false, 1)
	contract(8035, "item_exchange", 0) // asked at zero
	copyItem(165, 8035, 17481, int64(10), true, true, 1)
	contract(8036, "item_exchange", 555_000_000) // copy with unknown runs
	copyItem(166, 8036, 17481, nil, true, true, 1)
	contract(8037, "item_exchange", 444_000_000) // a stack, not a singleton copy
	copyItem(167, 8037, 17481, int64(10), true, true, 2)

	if n := app.refreshBlueprintCopyMedians(ctx); n != 2 {
		t.Fatalf("stored %d median rows, want 2", n)
	}
	rows, err := q.ListBlueprintCopyMediansForTypes(ctx, []int64{17481, 17482})
	if err != nil {
		t.Fatalf("read medians: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("median rows = %d, want 2: %+v", len(rows), rows)
	}
	byRuns := map[int64]db.BlueprintCopyMedian{}
	for _, r := range rows {
		byRuns[r.Runs] = r
	}
	if got := byRuns[10]; got.TypeID != 17481 || got.MedianPrice != 1_290_000 || got.SampleCount != 5 {
		t.Errorf("10-run median = %+v, want {17481, 1290000, n=5}", got)
	}
	if got := byRuns[5]; got.MedianPrice != 300_000 || got.SampleCount != 5 {
		t.Errorf("5-run median = %+v, want {300000, n=5}", got)
	}
	if byRuns[10].ComputedAt.IsZero() {
		t.Error("the stored median carries no computed-at stamp")
	}

	// The stamp gates the cadence: still fresh, no recompute.
	if n := app.refreshBlueprintCopyMedians(ctx); n != 0 {
		t.Errorf("fresh medians recomputed (%d rows), want the pass to stand down", n)
	}
	// An aged stamp lets it run again.
	if _, err := conn.ExecContext(ctx,
		`UPDATE blueprint_copy_medians SET computed_at = $1`, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("age medians: %v", err)
	}
	if n := app.refreshBlueprintCopyMedians(ctx); n != 2 {
		t.Errorf("aged medians: stored %d rows, want 2", n)
	}
}

// TestAppraiseLinesBlueprintCopyPricing runs the core
// directly: a copy with every type-level price present --
// fresh typical, estimate, live fallback -- still prices
// only from its median, and an original of the same type
// prices through the house order unchanged.
func TestAppraiseLinesBlueprintCopyPricing(t *testing.T) {
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
	// The trap, fully baited: 17481 has a fresh typical at
	// the original's 1,558,500,000, an estimate, and a live
	// fallback. 17482 has a fresh typical and nothing else.
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES
		(10000002, 17481, 1558500000, $1), (10000002, 17482, 5000, $1)`, now.Add(-time.Hour))
	exec(`INSERT INTO true_values (type_id, build_cost, est_sell, sell_samples, sell_source, computed_at) VALUES (17481, 0, 999999999, 2, 'listing', $1)`, now)
	if err := q.InsertMarketStatsFallback(ctx, db.InsertMarketStatsFallbackParams{
		RegionID: 10000002, TypeID: 17481, MedianSell: 888_888_888, FivepctSell: 888_888_888,
		Source: "fuzzwork", SourceAsof: now.Add(-time.Hour), FetchedAt: now,
	}); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}
	// Postgres keeps microseconds; seed the stamp at that
	// grain so the read-back compares exactly.
	medianAt := now.Add(-30 * time.Minute).Truncate(time.Microsecond)
	if err := q.InsertBlueprintCopyMedian(ctx, db.InsertBlueprintCopyMedianParams{
		TypeID: 17481, Runs: 10, MedianPrice: 1_290_000, SampleCount: 68, ComputedAt: medianAt,
	}); err != nil {
		t.Fatalf("seed copy median: %v", err)
	}

	copyLine := func(typeID, runs int64) module.AppraisalLine {
		return module.AppraisalLine{TypeID: typeID, Quantity: 1, Included: true,
			BlueprintCopy: true, Runs: runs, MaterialEfficiency: 10, TimeEfficiency: 20}
	}
	res, err := app.AppraiseLines(ctx, []module.AppraisalLine{
		copyLine(17481, 10),                          // prices from its median
		copyLine(17481, 7),                           // no median at 7 runs
		copyLine(17481, 0),                           // unknown runs key no median
		copyLine(17482, 10),                          // no median; its type has a typical it must not take
		{TypeID: 17481, Quantity: 1, Included: true}, // the original itself
	})
	if err != nil {
		t.Fatalf("appraise lines: %v", err)
	}
	if len(res.Lines) != 5 {
		t.Fatalf("lines = %d, want 5", len(res.Lines))
	}
	got := res.Lines[0]
	if !got.Priced || got.UnitPrice != 1_290_000 || got.Tier != module.AppraisalTierCopyMedian {
		t.Errorf("copy with a median = {%v %v %q}, want {1290000 copy_median}", got.Priced, got.UnitPrice, got.Tier)
	}
	if !got.AsOf.Equal(medianAt) {
		t.Errorf("copy as-of = %v, want the median's computed-at %v", got.AsOf, medianAt)
	}
	for i, why := range []string{"no median at its runs", "unknown runs", "type typical must not leak to a copy"} {
		if ln := res.Lines[i+1]; ln.Priced || ln.Tier != module.AppraisalTierUnpriced {
			t.Errorf("copy line %d (%s) = {%v %v %q}, want unpriced", i+1, why, ln.Priced, ln.UnitPrice, ln.Tier)
		}
	}
	if ln := res.Lines[4]; !ln.Priced || ln.UnitPrice != 1_558_500_000 || ln.Tier != module.AppraisalTierTypical {
		t.Errorf("original = {%v %v %q}, want {1558500000 typical}", ln.Priced, ln.UnitPrice, ln.Tier)
	}
	if res.OfferedValue != 1_290_000+1_558_500_000 || res.PricedOffered != 2 || res.OfferedLines != 5 {
		t.Errorf("offered totals = %v (%d/%d), want 1559790000 (2/5)",
			res.OfferedValue, res.PricedOffered, res.OfferedLines)
	}

	// The wanted side obeys the same rule: a wanted copy
	// values at its median, feeding WantValue.
	res, err = app.AppraiseLines(ctx, []module.AppraisalLine{
		{TypeID: 17481, Quantity: 1, Included: false, BlueprintCopy: true, Runs: 10},
	})
	if err != nil {
		t.Fatalf("appraise want line: %v", err)
	}
	if res.WantValue != 1_290_000 || res.PricedWanted != 1 || res.WantedLines != 1 {
		t.Errorf("want totals = %v (%d/%d), want 1290000 (1/1)",
			res.WantValue, res.PricedWanted, res.WantedLines)
	}
}

// TestAppraisePublicContractsBlueprintCopyEndToEnd runs the
// whole loop the worker runs: corpus -> median pass ->
// appraisal pass -> stored contract rows.
func TestAppraisePublicContractsBlueprintCopyEndToEnd(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	ctx := context.Background()
	now := time.Now().UTC()
	contract, copyItem := seedCopyCorpus(t, conn)

	// The original's typical sits in the stats table too, so
	// any leak of the type-level order into the copy's price
	// shows up as 1,558,500,000.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at)
		 VALUES (10000002, 17481, 1558500000, $1)`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed typical: %v", err)
	}
	// Five listings make the (17481, 10 runs) median 1.29M.
	for i, ask := range []float64{800_000, 1_000_000, 1_290_000, 2_000_000, 2_500_000} {
		id := int64(8101 + i)
		contract(id, "item_exchange", ask)
		copyItem(int64(200+i), id, 17481, int64(10), true, true, 1)
	}
	if n := app.refreshBlueprintCopyMedians(ctx); n != 1 {
		t.Fatalf("median pass stored %d rows, want 1", n)
	}

	// The target: one Procurer-class copy, ME 10 / TE 20 /
	// 10 runs, asked at 2,500,000 -- the diagnosis's row.
	contract(9201, "item_exchange", 2_500_000)
	copyItem(210, 9201, 17481, int64(10), true, true, 1)
	// A copy whose type has no median anywhere: unpriced.
	contract(9202, "item_exchange", 800_000)
	copyItem(211, 9202, 17483, int64(10), true, true, 1)

	app.appraisePublicContracts(ctx)

	items, err := q.ListPublicContractItems(ctx, 9201)
	if err != nil || len(items) != 1 {
		t.Fatalf("read 9201 items: %v (%d)", err, len(items))
	}
	if !items[0].UnitPrice.Valid || items[0].UnitPrice.Float64 != 1_290_000 ||
		items[0].PriceTier != module.AppraisalTierCopyMedian {
		t.Errorf("9201 line = %v/%q, want the 1290000 copy median",
			items[0].UnitPrice, items[0].PriceTier)
	}
	if !items[0].PriceAsof.Valid {
		t.Error("9201 line carries no as-of")
	}
	c1, err := q.GetPublicContract(ctx, 9201)
	if err != nil {
		t.Fatalf("read 9201: %v", err)
	}
	if c1.OfferedValue != 1_290_000 || c1.TierMix != "copy_median:1" {
		t.Errorf("9201 totals = %v mix %q, want 1290000 copy_median:1", c1.OfferedValue, c1.TierMix)
	}
	// 2,500,000 asked against 1,290,000 of value is no deal,
	// however the old BPO-priced math read it.
	wantRatio := 2_500_000.0 / 1_290_000.0
	if !c1.DealRatio.Valid || math.Abs(c1.DealRatio.Float64-wantRatio) > 1e-9 {
		t.Errorf("9201 ratio = %v, want %v", c1.DealRatio, wantRatio)
	}
	if appraisalDealFlag(c1.DealRatio) {
		t.Error("9201 flagged as a deal at 194% of its copy value")
	}

	items2, err := q.ListPublicContractItems(ctx, 9202)
	if err != nil || len(items2) != 1 {
		t.Fatalf("read 9202 items: %v (%d)", err, len(items2))
	}
	if items2[0].UnitPrice.Valid || items2[0].PriceTier != module.AppraisalTierUnpriced {
		t.Errorf("9202 line = %v/%q, want honestly unpriced", items2[0].UnitPrice, items2[0].PriceTier)
	}
	c2, err := q.GetPublicContract(ctx, 9202)
	if err != nil {
		t.Fatalf("read 9202: %v", err)
	}
	if c2.OfferedValue != 0 || c2.PricedLines != 0 || c2.DealRatio.Valid {
		t.Errorf("9202 totals = %v (%d priced, ratio %v), want 0, 0, none",
			c2.OfferedValue, c2.PricedLines, c2.DealRatio)
	}
}

// TestContractDetailCopyMedianLabel pins the rendered
// provenance of a copy-priced line: the tier words as "Our
// copy median" and the note names the sample the median
// stands on, on the detail page and the expansion panel
// alike (seedContractCorpus's 7006 is the copy listing).
func TestContractDetailCopyMedianLabel(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := q.InsertBlueprintCopyMedian(ctx, db.InsertBlueprintCopyMedianParams{
		TypeID: 9999, Runs: 5, MedianPrice: 42_000, SampleCount: 12, ComputedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed copy median: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`UPDATE pub_contract_items SET unit_price=42000, line_value=42000,
		price_tier='copy_median', price_asof=$1 WHERE record_id=6`, now.Add(-time.Hour))
	exec(`UPDATE pub_contracts SET offered_value=42000, priced_lines=1, total_lines=1,
		tier_mix='copy_median:1', priced_at=$1 WHERE contract_id=7006`, now)

	_, body := getAnon(t, app, "/contracts/search/7006/")
	// The detail page renders the drawer's cards, whose
	// figures use the short form (42,000 -> "42.0K ISK").
	mustContain(t, "detail copy median", body,
		"Our copy median", "median of 12 copy listings", "42.0K ISK")
	_, body = getAnon(t, app, "/contracts/search/7006/panel")
	mustContain(t, "panel copy median", body,
		"Our copy median", "median of 12 copy listings")
}

// TestAppraisalToolBlueprintCopyMedian covers the paste
// appraiser: a pasted copy prices from its stored median
// under the copy label with its sample in the note; the
// original beside it prices at its typical as before; a
// copy with no median stored is left unpriced rather than
// priced as an original.
func TestAppraisalToolBlueprintCopyMedian(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := q.InsertBlueprintCopyMedian(ctx, db.InsertBlueprintCopyMedianParams{
		TypeID: 7001, Runs: 5, MedianPrice: 42_000, SampleCount: 12, ComputedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed copy median: %v", err)
	}

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`Rifter Blueprint (BPC, ME 10, TE 20, 5 runs)
Rifter Blueprint`}})
	mustContain(t, "paste copy median", body,
		"Our copy median", "42,000.00",
		"BPC · ME 10 · TE 20 · 5 runs · median of 12 copy listings",
		"1,000,000.00", "Our Forge typical",
		"Blueprint — priced as an original",
		"Priced 2 of 2 lines", "1,042,000.00")

	// No median at 9 runs: the copy is not priced, and its
	// original's 1,000,000 typical appears nowhere.
	body, _, _ = postAnonAppraisal(t, app, url.Values{"items": {`Rifter Blueprint (BPC, ME 10, TE 20, 9 runs)`}})
	mustContain(t, "paste copy without a median", body,
		"Not priced", "Priced 0 of 1 lines")
	// The formatted figure, not the bare digits: the form's
	// placeholder legitimately names "Tritanium x 1,000,000".
	if strings.Contains(body, "1,000,000.00") {
		t.Error("a copy with no median was priced at its original's typical")
	}
}
