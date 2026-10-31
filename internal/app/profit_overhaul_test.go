package app

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// parseISK reads a formatted figure ("1,234.50 ISK", signed or
// not) back into a float.
func parseISK(t *testing.T, s string) float64 {
	t.Helper()
	s = strings.TrimSuffix(strings.TrimSpace(s), " ISK")
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		t.Fatalf("figure %q does not parse: %v", s, err)
	}
	return v
}

// The waterfall is a partition of the By activity figures: its
// steps, read back out of the chart's own hover rows, sum to
// the page's Earned exactly, and its COGS step is the trade
// fold's matched cost over the span.
func TestProfitWaterfallPartition(t *testing.T) {
	app, _, queries := demoTestApp(t)
	ctx := context.Background()
	view, err := app.buildProfitView(ctx, app.demoUserID(ctx), "", "30", "", profitModes{})
	if err != nil {
		t.Fatal(err)
	}
	wf := view.Charts.Waterfall
	if wf == nil {
		t.Fatal("no waterfall on the demo view")
	}
	if len(wf.Hits) != 5 {
		t.Fatalf("waterfall steps = %d, want 5", len(wf.Hits))
	}
	steps := make([]float64, 5)
	for i, hit := range wf.Hits {
		var rows [][3]string
		if err := json.Unmarshal([]byte(hit.Rows), &rows); err != nil || len(rows) == 0 {
			t.Fatalf("step %d rows: %v", i, err)
		}
		steps[i] = parseISK(t, rows[0][1])
	}
	gross, cogs, industry, fees, net := steps[0], -steps[1], -steps[2], -steps[3], steps[4]
	if math.Abs(gross-cogs-industry-fees-net) > 0.05 {
		t.Errorf("waterfall steps do not sum to the net: %.2f - %.2f - %.2f - %.2f = %.2f, net %.2f",
			gross, cogs, industry, fees, gross-cogs-industry-fees, net)
	}
	if earned := parseISK(t, view.Earned); math.Abs(net-earned) > 0.05 {
		t.Errorf("waterfall net = %.2f, page Earned = %.2f", net, earned)
	}
	// The COGS step is the COGS buckets' matched cost, summed
	// straight from the query.
	now := time.Now().UTC()
	fromDay := now.AddDate(0, 0, -29).Format("2006-01-02")
	buckets, err := queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{
		Unit: "day", OwnerKind: "char", OwnerIds: []int64{demoCharacterID, demoCharacter2ID}, FromDay: fromDay,
	})
	if err != nil {
		t.Fatalf("ListTradeCOGSBuckets: %v", err)
	}
	var wantCost float64
	for _, b := range buckets {
		wantCost += b.CovCost
	}
	if math.Abs(cogs-wantCost) > 0.05 {
		t.Errorf("waterfall COGS step = %.2f, COGS buckets sum = %.2f", cogs, wantCost)
	}
	if wf.Cap == nil {
		t.Error("waterfall net bar has no unverified cap, but the demo has uncovered sales")
	}
	if view.CoveragePct == "" || view.CoverageUncovered == "" {
		t.Errorf("coverage card figures missing: pct %q, uncovered %q", view.CoveragePct, view.CoverageUncovered)
	}
}

// The uncovered day filter is the resolution queue for one
// bucket: it returns exactly the types that sold units with no
// known cost that day, no more and no fewer.
func TestProfitUncoveredDayFilter(t *testing.T) {
	app, conn, queries := demoTestApp(t)
	ctx := context.Background()
	owners := []int64{demoCharacterID, demoCharacter2ID}
	now := time.Now().UTC()
	fromDay := now.AddDate(0, 0, -29).Format("2006-01-02")
	cov, err := queries.ListTradeCoverageDaily(ctx, db.ListTradeCoverageDailyParams{
		OwnerKind: "char", OwnerIds: owners, FromDay: fromDay,
	})
	if err != nil {
		t.Fatalf("ListTradeCoverageDaily: %v", err)
	}
	day := ""
	for _, c := range cov {
		if c.Units > c.CovUnits {
			day = c.Day // the last partly covered day in the span
		}
	}
	if day == "" {
		t.Fatal("the demo span has no partly covered day to filter to")
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT type_id FROM trade_item_daily
		WHERE owner_kind = 'char' AND owner_id IN ($1, $2) AND day = $3
		GROUP BY type_id
		HAVING SUM(units) > SUM(cov_units)`,
		demoCharacterID, demoCharacter2ID, day)
	if err != nil {
		t.Fatalf("expected rows: %v", err)
	}
	want := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatalf("no uncovered types on %s by the daily fold, but coverage says the day is partly covered", day)
	}
	view, err := app.buildProfitView(ctx, app.demoUserID(ctx), "", "30", "", profitModes{Uncovered: day})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, item := range view.Items {
		got[item.TypeID] = true
		if item.Covered {
			t.Errorf("filtered item %d (%s) is fully covered; the queue holds uncovered rows only", item.TypeID, item.Name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("uncovered=%s returned %d types %v, want %d %v", day, len(got), got, len(want), want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("uncovered=%s is missing type %d", day, id)
		}
	}
	if view.UncoveredRaw != day {
		t.Errorf("view filter = %q, want %q", view.UncoveredRaw, day)
	}
}
