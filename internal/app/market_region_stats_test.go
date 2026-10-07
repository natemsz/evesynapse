package app

// P1 region stats platform tests: the whole-region sweep
// (aggregation from paged fixtures, incremental page budget,
// cadence gate, vanished-type cleanup, daily upsert), and the
// item page regions ribbon proven to render from stored rows
// with zero outbound calls.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/markethistory"
)

// regionBookTransport serves a mutable whole-region book the way
// ESI does: /markets/<region>/orders/ paged, X-Pages on every
// response. books maps region ID to its pages; tests rewrite the
// book between sweeps to watch rows appear and vanish.
type regionBookTransport struct {
	calls    atomic.Int64
	books    map[int64][][]esi.MarketOrder
	failWith int // when >0, every page answers with this status
}

func (s *regionBookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	header := http.Header{"Content-Type": []string{"application/json"}}
	if s.failWith > 0 {
		return &http.Response{
			StatusCode: s.failWith,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"error":"error limited"}`)),
		}, nil
	}
	var regionID int64
	if _, err := fmt.Sscanf(req.URL.Path, "/markets/%d/orders/", &regionID); err != nil {
		return &http.Response{
			StatusCode: http.StatusNotFound, Header: header,
			Body: io.NopCloser(strings.NewReader(`{"error":"unexpected path"}`)),
		}, nil
	}
	pages := s.books[regionID]
	page := 1
	fmt.Sscanf(req.URL.Query().Get("page"), "%d", &page)
	header.Set("X-Pages", fmt.Sprintf("%d", len(pages)))
	orders := []esi.MarketOrder{}
	if page >= 1 && page <= len(pages) {
		orders = pages[page-1]
	}
	raw, _ := json.Marshal(orders)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}, nil
}

// markRegionFresh settles a region's sweep record to just now,
// so the sweep picker skips it -- tests use it to aim the sweep
// at one region at a time (and to prove a fresh region is never
// re-fetched).
func markRegionFresh(t *testing.T, q *db.Queries, regionID int64) {
	t.Helper()
	if err := q.UpsertMarketFetchState(context.Background(), db.UpsertMarketFetchStateParams{
		Kind: regionSweepKind(regionID), State: fetchStateOK,
		AttemptedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("mark region %d fresh: %v", regionID, err)
	}
}

// sweepUntilIdle drives sweepRegionStats until no region has a
// sweep in progress. Sweep progress lives in market_sweep_state
// now (schema 034), so idleness is a database question, not a
// memory one.
func sweepUntilIdle(t *testing.T, app *Application) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if _, limited := app.sweepRegionStats(context.Background(), &fetchBudget{left: 1000}); limited {
			t.Fatal("sweep hit the error limit on a healthy stub")
		}
		var open int
		if err := app.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM market_sweep_state`).Scan(&open); err != nil {
			t.Fatalf("count in-progress sweeps: %v", err)
		}
		if open == 0 {
			return
		}
	}
	t.Fatal("sweep never completed")
}

func TestRegionSweepStoresStatsAndCleansVanished(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// Only The Forge is due; the other hubs are freshly swept.
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 100},
			{OrderID: 2, TypeID: 34, IsBuyOrder: false, Price: 6.0, VolumeRemain: 200},
			{OrderID: 3, TypeID: 34, IsBuyOrder: true, Price: 4.0, VolumeRemain: 50},
			{OrderID: 4, TypeID: 34, IsBuyOrder: true, Price: 3.0, VolumeRemain: 60},
		},
		{
			{OrderID: 5, TypeID: 34, IsBuyOrder: false, Price: 7.0, VolumeRemain: 300},
			{OrderID: 6, TypeID: 35, IsBuyOrder: false, Price: 100.0, VolumeRemain: 10},
			{OrderID: 7, TypeID: 35, IsBuyOrder: true, Price: 90.0, VolumeRemain: 5},
		},
	}

	sweepUntilIdle(t, app)

	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats for type 34: rows=%d err=%v, want 1", len(rows), err)
	}
	row := rows[0]
	if row.RegionID != 10000002 {
		t.Fatalf("type 34 stats in region %d, want The Forge", row.RegionID)
	}
	// Sells [5,6,7]: best 5, median 6, 9-in-10 band 7.
	if row.BestSell != 5.0 || row.TypicalSell != 6.0 || row.SellBand != 7.0 {
		t.Fatalf("type 34 sells: best %v typical %v band %v, want 5/6/7", row.BestSell, row.TypicalSell, row.SellBand)
	}
	if row.SellOrders != 3 || row.SellVolume != 600 {
		t.Fatalf("type 34 sell depth: %d orders / %d volume, want 3/600", row.SellOrders, row.SellVolume)
	}
	// Buys [4,3]: best 4, median 3.5, 9-in-10 band 3.
	if row.BestBuy != 4.0 || row.TypicalBuy != 3.5 || row.BuyBand != 3.0 {
		t.Fatalf("type 34 buys: best %v typical %v band %v, want 4/3.5/3", row.BestBuy, row.TypicalBuy, row.BuyBand)
	}
	if row.BuyOrders != 2 || row.BuyVolume != 110 {
		t.Fatalf("type 34 buy depth: %d orders / %d volume, want 2/110", row.BuyOrders, row.BuyVolume)
	}
	if row.UpdatedAt.IsZero() {
		t.Fatal("type 34 stats carry no updated_at")
	}

	// Daily snapshot written for today.
	daily, err := q.ListMarketRegionStatsDaily(ctx, db.ListMarketRegionStatsDailyParams{RegionID: 10000002, TypeID: 34})
	if err != nil || len(daily) != 1 {
		t.Fatalf("daily for type 34: rows=%d err=%v, want 1", len(daily), err)
	}
	if daily[0].TypicalSell != 6.0 {
		t.Fatalf("daily typical sell %v, want 6", daily[0].TypicalSell)
	}

	// A second cycle does not re-fetch a fresh region.
	callsBefore := transport.calls.Load()
	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 120})
	if limited || stored != 0 || transport.calls.Load() != callsBefore {
		t.Fatalf("fresh-region cycle: stored=%d limited=%v calls %d->%d, want 0/false/unchanged",
			stored, limited, callsBefore, transport.calls.Load())
	}

	// Type 35 vanishes from the book; a re-sweep (gate aged past)
	// drops its live row and still upserts just one daily row.
	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 8, TypeID: 34, IsBuyOrder: false, Price: 6.5, VolumeRemain: 10},
		},
	}
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: regionSweepKind(10000002), State: fetchStateOK,
		AttemptedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("age sweep record: %v", err)
	}
	sweepUntilIdle(t, app)

	var live35 int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002 AND type_id = 35`).Scan(&live35); err != nil {
		t.Fatalf("count live type 35: %v", err)
	}
	if live35 != 0 {
		t.Fatalf("vanished type 35 still has %d live rows, want 0", live35)
	}
	daily34, err := q.ListMarketRegionStatsDaily(ctx, db.ListMarketRegionStatsDailyParams{RegionID: 10000002, TypeID: 34})
	if err != nil || len(daily34) != 1 {
		t.Fatalf("daily for type 34 after re-sweep: rows=%d err=%v, want still 1 (upsert by day)", len(daily34), err)
	}
	if daily34[0].TypicalSell != 6.5 {
		t.Fatalf("daily typical sell after re-sweep: %v, want 6.5", daily34[0].TypicalSell)
	}
	rows, err = q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 || rows[0].TypicalSell != 6.5 {
		t.Fatalf("live stats after re-sweep: %+v err=%v, want typical sell 6.5", rows, err)
	}
}

func TestRegionSweepIncrementalPageBudget(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// Aim the sweep at Domain with a 60-page book -- more pages
	// than one cycle may read.
	for _, region := range marketRegions {
		if region.ID != 10000043 {
			markRegionFresh(t, q, region.ID)
		}
	}
	pages := make([][]esi.MarketOrder, 60)
	for i := range pages {
		pages[i] = []esi.MarketOrder{
			{OrderID: int64(1000 + i), TypeID: 34, IsBuyOrder: false, Price: float64(10 + i), VolumeRemain: 1},
		}
	}
	transport.books[10000043] = pages

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited {
		t.Fatal("sweep reported the error limit on a healthy stub")
	}
	if stored != maxRegionSweepPagesPerCycle {
		t.Fatalf("first cycle read %d pages, want the %d-page budget", stored, maxRegionSweepPagesPerCycle)
	}
	var live int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000043`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("partial sweep stored %d stats rows, want 0 (writes land only on completion)", live)
	}
	// Progress lives on disk: the cursor sits at page 51 with
	// the first 50 pages staged behind it.
	var nextPage int64
	if err := conn.QueryRow(`SELECT next_page FROM market_sweep_state WHERE region_id = 10000043`).Scan(&nextPage); err != nil {
		t.Fatalf("read sweep cursor: %v", err)
	}
	if nextPage != 51 {
		t.Fatalf("sweep cursor at page %d, want 51", nextPage)
	}
	var staged int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_orders WHERE region_id = 10000043`).Scan(&staged); err != nil {
		t.Fatalf("count staged orders: %v", err)
	}
	if staged != 50 {
		t.Fatalf("staged orders after one cycle: %d, want 50 (one per page)", staged)
	}

	sweepUntilIdle(t, app)
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats after full sweep: rows=%d err=%v, want 1", len(rows), err)
	}
	if rows[0].SellOrders != 60 || rows[0].TypicalSell != 39.5 {
		t.Fatalf("60-page book: %d sell orders, typical %v, want 60 / 39.5", rows[0].SellOrders, rows[0].TypicalSell)
	}
	// Completion cleared the staging and the cursor.
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_orders WHERE region_id = 10000043`).Scan(&staged); err != nil {
		t.Fatalf("count staged orders after completion: %v", err)
	}
	if staged != 0 {
		t.Fatalf("staged orders after completion: %d, want 0", staged)
	}
	var stateRows int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_state WHERE region_id = 10000043`).Scan(&stateRows); err != nil {
		t.Fatalf("count sweep state after completion: %v", err)
	}
	if stateRows != 0 {
		t.Fatalf("sweep state rows after completion: %d, want 0", stateRows)
	}
}

func TestRegionSweepErrorLimitKeepsProgress(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}, failWith: 429}
	app, conn, q := buildCorpTestApp(t, transport)
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 1}},
	}

	stored, limited := app.sweepRegionStats(context.Background(), &fetchBudget{left: 120})
	if !limited || stored != 0 {
		t.Fatalf("error-limited sweep: stored=%d limited=%v, want 0/true", stored, limited)
	}
	var live int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_region_stats`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("error-limited sweep stored %d rows, want 0", live)
	}
	// The sweep itself is not lost: its cursor waits on disk at
	// page 1, so recovery resumes this sweep instead of starting
	// over.
	var open int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_state WHERE region_id = 10000002`).Scan(&open); err != nil {
		t.Fatalf("count sweep state: %v", err)
	}
	if open != 1 {
		t.Fatalf("error-limited sweep left %d sweep state rows, want 1 (the resumable cursor)", open)
	}

	// ESI recovers: the same sweep finishes and stores.
	transport.failWith = 0
	sweepUntilIdle(t, app)
	rows, err := q.ListMarketRegionStatsByType(context.Background(), 34)
	if err != nil || len(rows) != 1 || rows[0].BestSell != 5.0 {
		t.Fatalf("stats after recovery: %+v err=%v, want best sell 5", rows, err)
	}
}

func TestAttachRegionStatsZeroOutbound(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, seed := range []db.UpsertMarketRegionStatParams{
		{RegionID: 10000002, TypeID: 34, BestSell: 5, TypicalSell: 6, SellBand: 7,
			BestBuy: 4, TypicalBuy: 3.5, BuyBand: 3, SellOrders: 3, BuyOrders: 2,
			SellVolume: 600, BuyVolume: 110, UpdatedAt: now.Add(-12 * time.Minute)},
		{RegionID: 10000042, TypeID: 34, BestSell: 9, TypicalSell: 10.25, SellBand: 11,
			BestBuy: 8, TypicalBuy: 8.5, BuyBand: 8, SellOrders: 1, BuyOrders: 1,
			SellVolume: 5, BuyVolume: 5, UpdatedAt: now.Add(-3 * time.Hour)},
	} {
		if err := q.UpsertMarketRegionStat(ctx, seed); err != nil {
			t.Fatalf("seed stats: %v", err)
		}
	}

	item := &marketItem{TypeID: 34, RegionID: 10000032}
	app.attachRegionStats(ctx, item)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("ribbon attach made %d outbound calls, want 0", got)
	}
	if len(item.RegionStats) != len(marketRegions) {
		t.Fatalf("ribbon rows: %d, want one per hub region (%d)", len(item.RegionStats), len(marketRegions))
	}
	byRegion := map[int64]marketRegionStatRow{}
	for _, row := range item.RegionStats {
		byRegion[row.RegionID] = row
	}
	forge := byRegion[10000002]
	if !forge.HasData || forge.TypicalSell != esi.FormatISK(6.0) || forge.TypicalBuy != esi.FormatISK(3.5) || forge.Age != "12 minutes ago" {
		t.Fatalf("Forge ribbon row: %+v, want stored typicals and a 12-minute age", forge)
	}
	metro := byRegion[10000042]
	if !metro.HasData || metro.TypicalSell != esi.FormatISK(10.25) || metro.Age != "3 hours ago" {
		t.Fatalf("Metropolis ribbon row: %+v, want stored typicals and a 3-hour age", metro)
	}
	if current := byRegion[10000032]; !current.Active || current.HasData {
		t.Fatalf("Sinq Laison row: %+v, want Active with no data yet", current)
	}
	if heimatar := byRegion[10000030]; heimatar.HasData || heimatar.Age != "" {
		t.Fatalf("Heimatar row: %+v, want the empty no-data state", heimatar)
	}
}

func TestRibbonRendersStoredStats(t *testing.T) {
	transport := &marketStubTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if _, err := conn.Exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1)`); err != nil {
		t.Fatalf("seed SDE type: %v", err)
	}
	now := time.Now().UTC()
	for _, seed := range []db.UpsertMarketRegionStatParams{
		{RegionID: 10000002, TypeID: 34, BestSell: 5, TypicalSell: 6, SellBand: 7,
			BestBuy: 4, TypicalBuy: 3.5, BuyBand: 3, SellOrders: 3, BuyOrders: 2,
			SellVolume: 600, BuyVolume: 110, UpdatedAt: now.Add(-12 * time.Minute)},
		{RegionID: 10000042, TypeID: 34, BestSell: 9, TypicalSell: 10.25, SellBand: 11,
			BestBuy: 8, TypicalBuy: 8.5, BuyBand: 8, SellOrders: 1, BuyOrders: 1,
			SellVolume: 5, BuyVolume: 5, UpdatedAt: now.Add(-3 * time.Hour)},
	} {
		if err := q.UpsertMarketRegionStat(ctx, seed); err != nil {
			t.Fatalf("seed stats: %v", err)
		}
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("item page: status %d", code)
	}
	mustContain(t, "/market/?type=34 ribbon", body,
		"Typical prices across the hub regions",
		esi.FormatISK(6.0)+" ISK",   // The Forge typical sell, from storage
		esi.FormatISK(10.25)+" ISK", // Metropolis typical sell, from storage
		"region=10000042",           // region rows link to that region's view
		"12 minutes ago",
		"No data yet", // regions a sweep has not covered yet
	)
}

func TestRegionSweepStoresAvgDailyVolume(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}

	// Type 34: seven recorded days ending today at 10..70, plus
	// an eighth row seven days back that the 7-day window must
	// leave out (9999 would poison the average if it leaked in).
	// Type 35: only three recorded days (90, 60, 30) -- the
	// average runs over the days present, not the calendar span.
	// Type 36: in the book but with no history at all -- 0.
	now := time.Now().UTC()
	seedHistory := func(typeID int64, dayOffset int, volume int64) {
		t.Helper()
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: typeID,
			Date:    now.AddDate(0, 0, -dayOffset).Format("2006-01-02"),
			Average: 10, Highest: 10, Lowest: 10, Volume: volume, OrderCount: 1,
		}); err != nil {
			t.Fatalf("seed history type %d: %v", typeID, err)
		}
	}
	for i, vol := range []int64{70, 60, 50, 40, 30, 20, 10} {
		seedHistory(34, i, vol)
	}
	seedHistory(34, 7, 9999)
	seedHistory(35, 0, 90)
	seedHistory(35, 1, 60)
	seedHistory(35, 2, 30)

	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 100},
			{OrderID: 2, TypeID: 35, IsBuyOrder: false, Price: 100.0, VolumeRemain: 10},
			{OrderID: 3, TypeID: 36, IsBuyOrder: false, Price: 50.0, VolumeRemain: 10},
		},
	}
	sweepUntilIdle(t, app)

	rows, err := q.ListMarketRegionStatsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list region stats: %v", err)
	}
	byType := map[int64]db.MarketRegionStat{}
	for _, r := range rows {
		byType[r.TypeID] = r
	}
	close := func(got, want float64) bool {
		d := got - want
		return d < 1e-9 && d > -1e-9
	}
	if got := byType[34].AvgDailyVolume; !close(got, 40) {
		t.Fatalf("type 34 avg daily volume: %v, want 40 (mean of 10..70 over the newest 7 recorded days)", got)
	}
	if got := byType[35].AvgDailyVolume; !close(got, 60) {
		t.Fatalf("type 35 avg daily volume: %v, want 60 (mean over the 3 recorded days present)", got)
	}
	if got := byType[36].AvgDailyVolume; got != 0 {
		t.Fatalf("type 36 avg daily volume: %v, want 0 (no recorded history)", got)
	}
	// The stored figure must be the render-time window's figure:
	// markethistory.Window over the same rows agrees exactly.
	_, want, ok := markethistory.Window(app.recentHistoryRows(ctx, 10000002, 34, markethistory.ChartRows), 7)
	if !ok {
		t.Fatal("history window empty for seeded type 34")
	}
	if got := byType[34].AvgDailyVolume; !close(got, want) {
		t.Fatalf("stored avg %v disagrees with render-time historyWindow %v", got, want)
	}
}
