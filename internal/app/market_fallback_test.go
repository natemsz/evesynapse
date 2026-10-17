package app

// Fallback prices: the Fuzzwork mirror under our own sweeps.
// The parser must keep only the median and the five-percent
// figure (a poisoned weighted average in the fixture must land
// nowhere), a side with no orders is absent data, the ingest
// downloads only when the orderset advances, and the read-side
// swap prefers our fresh sweep, then the fallback inside its
// hard expiry, and never an expired fallback.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// fuzzworkSideRow renders one aggregate CSV side row. The
// weighted average column is the caller's to poison: the
// parser must never let it reach a stored field.
func fuzzworkSideRow(regionID, typeID int64, buy bool, weightedAvg, median, fivePct, volume float64, numOrders, orderset int64) string {
	side := "false"
	if buy {
		side = "true"
	}
	return fmt.Sprintf("%d|%d|%s,%v,999999999,0.01,1.5,%v,%v,%d,%v,%d",
		regionID, typeID, side, weightedAvg, median, volume, numOrders, fivePct, orderset)
}

func fuzzworkCSV(rows ...string) string {
	return "what,weightedaverage,maxval,minval,stddev,median,volume,numorders,fivepercent,orderSet\n" +
		strings.Join(rows, "\n") + "\n"
}

func TestParseFuzzworkAggregates(t *testing.T) {
	csvText := fuzzworkCSV(
		// Tritanium in The Forge: the sell side carries a joke
		// max (in the maxval column, unparsed) and the buy
		// side a weighted average dragged by a lowball.
		fuzzworkSideRow(10000002, 34, true, 349850298.51, 3.3, 3.75, 12498044257, 56, 777),
		fuzzworkSideRow(10000002, 34, false, 4.10, 4.36, 3.90, 987654321, 61, 777),
		// A type whose sell side has prices but no orders:
		// that side is absent data, not a price.
		fuzzworkSideRow(10000002, 35, true, 10, 11, 10.5, 500, 3, 777),
		fuzzworkSideRow(10000002, 35, false, 999, 12345, 12000, 0, 0, 777),
		// A type with no orders on either side: no row at all.
		fuzzworkSideRow(10000043, 36, true, 1, 1, 1, 0, 0, 777),
		fuzzworkSideRow(10000043, 36, false, 1, 1, 1, 0, 0, 777),
	)
	rows, orderset, err := parseFuzzworkAggregates(strings.NewReader(csvText))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if orderset != 777 {
		t.Errorf("orderset = %d, want 777", orderset)
	}
	trit := rows[fallbackKey{RegionID: 10000002, TypeID: 34}]
	if trit == nil {
		t.Fatal("no merged row for Forge/Tritanium")
	}
	if trit.MedianBuy != 3.3 || trit.FivePctBuy != 3.75 || trit.BuyOrders != 56 {
		t.Errorf("buy side = median %v fivepct %v orders %d, want 3.3 / 3.75 / 56", trit.MedianBuy, trit.FivePctBuy, trit.BuyOrders)
	}
	if trit.MedianSell != 4.36 || trit.FivePctSell != 3.90 || trit.SellOrders != 61 {
		t.Errorf("sell side = median %v fivepct %v orders %d, want 4.36 / 3.90 / 61", trit.MedianSell, trit.FivePctSell, trit.SellOrders)
	}
	pyrox := rows[fallbackKey{RegionID: 10000002, TypeID: 35}]
	if pyrox == nil {
		t.Fatal("no merged row for Forge/Pyroxeres")
	}
	if pyrox.MedianSell != 0 || pyrox.SellOrders != 0 {
		t.Errorf("no-order sell side stored as median %v over %d orders, want absent (0/0)", pyrox.MedianSell, pyrox.SellOrders)
	}
	if _, ok := rows[fallbackKey{RegionID: 10000043, TypeID: 36}]; ok {
		t.Error("type with no orders on either side got a row")
	}
}

func TestParseFuzzworkAggregatesMixedOrderset(t *testing.T) {
	csvText := fuzzworkCSV(
		fuzzworkSideRow(10000002, 34, true, 3, 3.3, 3.1, 100, 5, 777),
		fuzzworkSideRow(10000002, 34, false, 4, 4.4, 4.1, 100, 5, 778),
	)
	if _, _, err := parseFuzzworkAggregates(strings.NewReader(csvText)); err == nil {
		t.Fatal("mixed-orderset file parsed without error, want refusal")
	}
}

func TestResolvedRegionTypicals(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	own := func(age time.Duration, sell float64) *db.MarketRegionStat {
		return &db.MarketRegionStat{
			RegionID: 10000002, TypeID: 34, TypicalSell: sell, TypicalBuy: sell / 2,
			SellBand: sell * 2, BuyBand: sell / 3, SellOrders: 10, BuyOrders: 9,
			UpdatedAt: now.Add(-age),
		}
	}
	fb := func(age time.Duration, sell float64) *db.MarketStatsFallback {
		return &db.MarketStatsFallback{
			RegionID: 10000002, TypeID: 34, MedianSell: sell, MedianBuy: sell / 2,
			FivepctSell: sell * 1.5, FivepctBuy: sell / 3, SellOrders: 8, BuyOrders: 7,
			Source: "fuzzwork", OrdersetID: 777, SourceAsof: now.Add(-age), FetchedAt: now,
		}
	}
	cases := []struct {
		name     string
		own      *db.MarketRegionStat
		fbRow    *db.MarketStatsFallback
		wantOK   bool
		wantSell float64
		wantFb   bool
	}{
		{"own fresh beats fallback", own(30*time.Minute, 111), fb(time.Hour, 222), true, 111, false},
		{"own stale yields to fallback", own(4*time.Hour, 111), fb(time.Hour, 222), true, 222, true},
		{"no own row, fallback speaks", nil, fb(time.Hour, 222), true, 222, true},
		{"expired fallback is a gap", nil, fb(49*time.Hour, 222), false, 0, false},
		{"expired fallback leaves our stale row", own(4*time.Hour, 111), fb(49*time.Hour, 222), true, 111, false},
		{"own fresh ignores expired fallback", own(30*time.Minute, 111), fb(49*time.Hour, 222), true, 111, false},
		{"nothing stored", nil, nil, false, 0, false},
	}
	for _, tc := range cases {
		got, ok := resolvedRegionTypicals(tc.own, tc.fbRow, now)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.TypicalSell != tc.wantSell || got.Fallback != tc.wantFb {
			t.Errorf("%s: picked sell %v fallback=%v, want %v / %v", tc.name, got.TypicalSell, got.Fallback, tc.wantSell, tc.wantFb)
		}
	}
}

// fuzzworkFixture is a stand-in aggregate service: an orderset
// counter the test can advance and a CSV whose download count
// is observable.
type fuzzworkFixture struct {
	orderset  atomic.Int64
	downloads atomic.Int64
	polls     atomic.Int64
	csvBody   []byte // gzipped
}

func (f *fuzzworkFixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/orderset", func(w http.ResponseWriter, r *http.Request) {
		f.polls.Add(1)
		fmt.Fprintf(w, `{"orderset":%d}`, f.orderset.Load())
	})
	mux.HandleFunc("/aggregatecsv.csv.gz", func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		w.Header().Set("Last-Modified", "Sat, 17 Oct 2026 11:30:00 GMT")
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(f.csvBody)
	})
	return mux
}

// fuzzworkSnapshotCSV builds a snapshot over one region with
// enough types to pass the ingest sanity floor. Type 34's sell
// side has no orders, so its stored row must show that side as
// absent.
func fuzzworkSnapshotCSV(t *testing.T, orderset int64, typeCount int) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("what,weightedaverage,maxval,minval,stddev,median,volume,numorders,fivepercent,orderSet\n")
	for id := int64(1); id <= int64(typeCount); id++ {
		sellOrders := int64(12)
		if id == 34 {
			sellOrders = 0
		}
		fmt.Fprintf(&b, "10000002|%d|true,999999999,999999999,0.01,1.5,3.3,1000,56,3.75,%d\n", id, orderset)
		fmt.Fprintf(&b, "10000002|%d|false,4.10,999999999,0.01,1.5,4.36,900,%d,3.90,%d\n", id, sellOrders, orderset)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(b.String())); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip fixture close: %v", err)
	}
	return gz.Bytes()
}

func TestFallbackIngestDownloadsOnlyOnOrdersetAdvance(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	fixture := &fuzzworkFixture{}
	fixture.orderset.Store(4242)
	fixture.csvBody = fuzzworkSnapshotCSV(t, 4242, fallbackMinRows+50)
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	app.cfg.fuzzworkBaseURL = srv.URL

	if !app.refreshFallbackPrices(ctx) {
		t.Fatal("first refresh stored nothing, want a stored snapshot")
	}
	if got := fixture.downloads.Load(); got != 1 {
		t.Fatalf("downloads after first refresh = %d, want 1", got)
	}
	meta, err := q.GetMarketStatsFallbackMeta(ctx)
	if err != nil {
		t.Fatalf("meta after ingest: %v", err)
	}
	if meta.OrdersetID != 4242 || meta.RowCount != fallbackMinRows+50 {
		t.Errorf("meta = orderset %d rows %d, want 4242 / %d", meta.OrdersetID, meta.RowCount, fallbackMinRows+50)
	}
	row, err := q.GetMarketStatsFallback(ctx, db.GetMarketStatsFallbackParams{RegionID: 10000002, TypeID: 34})
	if err != nil {
		t.Fatalf("stored row for type 34: %v", err)
	}
	// The fixture's weighted average (999,999,999) must land
	// nowhere: the stored figures are the median and the
	// five-percent, and the orderless sell side is absent.
	if row.MedianBuy != 3.3 || row.FivepctBuy != 3.75 {
		t.Errorf("stored buy = median %v fivepct %v, want 3.3 / 3.75", row.MedianBuy, row.FivepctBuy)
	}
	if row.MedianSell != 0 || row.SellOrders != 0 {
		t.Errorf("orderless sell side stored as median %v over %d orders, want absent", row.MedianSell, row.SellOrders)
	}
	if row.Source != "fuzzwork" || row.OrdersetID != 4242 {
		t.Errorf("provenance = source %q orderset %d, want fuzzwork / 4242", row.Source, row.OrdersetID)
	}
	wantAsof := time.Date(2026, 10, 17, 11, 30, 0, 0, time.UTC)
	if !row.SourceAsof.Equal(wantAsof) {
		t.Errorf("source_asof = %v, want the file's Last-Modified %v", row.SourceAsof, wantAsof)
	}

	// An hour later (gate reset by hand) with the same
	// orderset: a poll, no download, nothing stored.
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: fuzzworkFetchKind, State: fetchStateOK, AttemptedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("reset poll gate: %v", err)
	}
	if app.refreshFallbackPrices(ctx) {
		t.Error("refresh with unchanged orderset stored a snapshot, want none")
	}
	if got := fixture.downloads.Load(); got != 1 {
		t.Errorf("downloads after unchanged poll = %d, want still 1", got)
	}
	if got := fixture.polls.Load(); got != 2 {
		t.Errorf("polls = %d, want 2 (the unchanged check still polls)", got)
	}

	// The orderset advances: one more download, meta follows.
	fixture.orderset.Store(4243)
	fixture.csvBody = fuzzworkSnapshotCSV(t, 4243, fallbackMinRows+50)
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: fuzzworkFetchKind, State: fetchStateOK, AttemptedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("reset poll gate: %v", err)
	}
	if !app.refreshFallbackPrices(ctx) {
		t.Fatal("refresh after orderset advance stored nothing")
	}
	if got := fixture.downloads.Load(); got != 2 {
		t.Errorf("downloads after advance = %d, want 2", got)
	}
	meta, err = q.GetMarketStatsFallbackMeta(ctx)
	if err != nil || meta.OrdersetID != 4243 {
		t.Errorf("meta after advance = %+v err %v, want orderset 4243", meta, err)
	}
}

func seedFallbackRow(t *testing.T, q *db.Queries, regionID, typeID int64, medianSell, medianBuy float64, asof time.Time) {
	t.Helper()
	if err := q.InsertMarketStatsFallback(context.Background(), db.InsertMarketStatsFallbackParams{
		RegionID: regionID, TypeID: typeID,
		MedianSell: medianSell, FivepctSell: medianSell * 0.9,
		MedianBuy: medianBuy, FivepctBuy: medianBuy * 1.1,
		SellOrders: 8, BuyOrders: 7, SellVolume: 100, BuyVolume: 90,
		Source: "fuzzwork", OrdersetID: 777, SourceAsof: asof, FetchedAt: asof,
	}); err != nil {
		t.Fatalf("seed fallback %d/%d: %v", regionID, typeID, err)
	}
}

func seedOwnStat(t *testing.T, q *db.Queries, regionID, typeID int64, typicalSell, typicalBuy float64, updatedAt time.Time) {
	t.Helper()
	if err := q.UpsertMarketRegionStat(context.Background(), db.UpsertMarketRegionStatParams{
		RegionID: regionID, TypeID: typeID,
		BestSell: typicalSell, TypicalSell: typicalSell, SellBand: typicalSell * 2,
		BestBuy: typicalBuy, TypicalBuy: typicalBuy, BuyBand: typicalBuy / 2,
		SellOrders: 10, BuyOrders: 9, SellVolume: 100, BuyVolume: 90,
		AvgDailyVolume: 1, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("seed own stat %d/%d: %v", regionID, typeID, err)
	}
}

func TestFallbackStripSwap(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	now := time.Now()

	// The Forge: our sweep is fresh -- it wins over a fresh
	// fallback row.
	seedOwnStat(t, q, 10000002, 34, 111, 55, now.Add(-30*time.Minute))
	seedFallbackRow(t, q, 10000002, 34, 222, 60, now.Add(-time.Hour))
	// Domain: no sweep row at all -- the fallback speaks.
	seedFallbackRow(t, q, 10000043, 34, 333, 60, now.Add(-time.Hour))
	// Sinq Laison: our row went stale -- the fallback takes over.
	seedOwnStat(t, q, 10000032, 34, 444, 55, now.Add(-5*time.Hour))
	seedFallbackRow(t, q, 10000032, 34, 555, 60, now.Add(-time.Hour))
	// Metropolis: our row is stale and the fallback expired --
	// our row still shows (with its age), the expired fallback
	// never does.
	seedOwnStat(t, q, 10000042, 34, 666, 55, now.Add(-5*time.Hour))
	seedFallbackRow(t, q, 10000042, 34, 777, 60, now.Add(-49*time.Hour))
	// Heimatar: nothing stored anywhere.

	item := &marketItem{TypeID: 34, RegionID: 10000002}
	app.attachRegionStats(context.Background(), item)
	byRegion := make(map[int64]marketRegionStatRow)
	for _, row := range item.RegionStats {
		byRegion[row.RegionID] = row
	}
	want := map[int64]struct {
		sell   string
		source string
		has    bool
	}{
		10000002: {esi.FormatISK(111), "", true},
		10000043: {esi.FormatISK(333), fallbackSourceLabel, true},
		10000032: {esi.FormatISK(555), fallbackSourceLabel, true},
		10000042: {esi.FormatISK(666), "", true},
		10000030: {"", "", false},
	}
	for regionID, w := range want {
		row := byRegion[regionID]
		if row.HasData != w.has {
			t.Errorf("region %d: HasData = %v, want %v", regionID, row.HasData, w.has)
			continue
		}
		if !w.has {
			continue
		}
		if row.TypicalSell != w.sell {
			t.Errorf("region %d: typical sell = %q, want %q", regionID, row.TypicalSell, w.sell)
		}
		if row.Source != w.source {
			t.Errorf("region %d: source = %q, want %q", regionID, row.Source, w.source)
		}
	}
}

func getAnonPage(t *testing.T, app *Application, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestFallbackProvenanceOnMarketPage(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	// The anonymous item page exists only for types the local
	// SDE carries.
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 18, 2, 1)`); err != nil {
		t.Fatalf("seed sde type: %v", err)
	}

	// Only the fallback speaks for Tritanium in The Forge,
	// and its fixture weighted average (never stored) must
	// not appear on the page either.
	seedFallbackRow(t, q, 10000002, 34, 4.36, 3.30, now.Add(-26*time.Minute))

	code, body := getAnonPage(t, app, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?type=34: status %d", code)
	}
	mustContain(t, "/market/?type=34", body, fallbackSourceLabel, esi.FormatISK(4.36))

	// The same row past its hard expiry: the provenance and
	// the price both disappear.
	if err := q.DeleteMarketStatsFallback(context.Background()); err != nil {
		t.Fatalf("clear fallback: %v", err)
	}
	seedFallbackRow(t, q, 10000002, 34, 4.36, 3.30, now.Add(-49*time.Hour))
	_, body = getAnonPage(t, app, "/market/?type=34")
	if strings.Contains(body, fallbackSourceLabel) {
		t.Error("expired fallback still renders its provenance label on the market page")
	}
	if strings.Contains(body, esi.FormatISK(4.36)) {
		t.Error("expired fallback still renders its price on the market page")
	}
}
