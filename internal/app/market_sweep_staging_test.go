package app

// P1 sweep-upgrade tests (schema 034): disk-staged sweeps --
// page budget, restart resume, transient-failure patience, and
// parallel region sweeps. Staging parity of the distilled stats
// themselves is covered by the region-stats and scanner suites,
// which drive the same completion path.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// pagedBookTransport serves whole-region books like ESI (paged,
// X-Pages header) while recording every (region, page) request,
// failing individual pages on demand, and tracking the peak
// number of requests in flight so the concurrency test can
// prove regions really do advance in parallel.
type pagedBookTransport struct {
	mu          sync.Mutex
	books       map[int64][][]esi.MarketOrder
	failPages   map[[2]int64]int // (region, page) -> status answered instead of the book
	delay       time.Duration    // artificial latency per request
	requests    [][2]int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (s *pagedBookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cur := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		max := s.maxInFlight.Load()
		if cur <= max || s.maxInFlight.CompareAndSwap(max, cur) {
			break
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	var regionID int64
	if _, err := fmt.Sscanf(req.URL.Path, "/markets/%d/orders/", &regionID); err != nil {
		return &http.Response{
			StatusCode: http.StatusNotFound, Header: header,
			Body: io.NopCloser(strings.NewReader(`{"error":"unexpected path"}`)),
		}, nil
	}
	page := int64(1)
	fmt.Sscanf(req.URL.Query().Get("page"), "%d", &page)
	s.mu.Lock()
	s.requests = append(s.requests, [2]int64{regionID, page})
	status := s.failPages[[2]int64{regionID, page}]
	pages := s.books[regionID]
	s.mu.Unlock()
	if status > 0 {
		return &http.Response{
			StatusCode: status, Header: header,
			Body: io.NopCloser(strings.NewReader(`{"error":"injected failure"}`)),
		}, nil
	}
	header.Set("X-Pages", fmt.Sprintf("%d", len(pages)))
	orders := []esi.MarketOrder{}
	if page >= 1 && page <= int64(len(pages)) {
		orders = pages[page-1]
	}
	raw, _ := json.Marshal(orders)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}, nil
}

// pagesFor returns the pages requested for one region, in
// request order.
func (s *pagedBookTransport) pagesFor(regionID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, r := range s.requests {
		if r[0] == regionID {
			out = append(out, r[1])
		}
	}
	return out
}

// drainRequests forgets every recorded request, so a test can
// assert exactly what the next pass fetched.
func (s *pagedBookTransport) drainRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (s *pagedBookTransport) failPage(regionID, page int64, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPages == nil {
		s.failPages = map[[2]int64]int{}
	}
	s.failPages[[2]int64{regionID, page}] = status
}

func (s *pagedBookTransport) clearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPages = nil
}

// secondProcessApp builds a fresh Application over an existing
// test database: a simulated process restart. All sweep state
// lives on disk (schema 034), so the "new process" resumes
// exactly where the killed one stopped.
func secondProcessApp(t *testing.T, conn *sql.DB, transport http.RoundTripper) *Application {
	t.Helper()
	queries := db.New(conn)
	sessionManager := scs.New()
	sessionManager.Store = sqlite3store.New(conn)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"
	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })
	return &Application{
		cfg:           Config{},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
}

func sweepStateCount(t *testing.T, conn *sql.DB, regionID int64) (states, staged int) {
	t.Helper()
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_state WHERE region_id = ?`, regionID).Scan(&states); err != nil {
		t.Fatalf("count sweep state: %v", err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_sweep_orders WHERE region_id = ?`, regionID).Scan(&staged); err != nil {
		t.Fatalf("count staged orders: %v", err)
	}
	return states, staged
}

func TestMarketSweepPageBudgetIsFifty(t *testing.T) {
	if maxRegionSweepPagesPerCycle != 50 {
		t.Fatalf("maxRegionSweepPagesPerCycle = %d, want 50", maxRegionSweepPagesPerCycle)
	}
}

func TestSweepRestartResumesMidSweep(t *testing.T) {
	transport := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	// Five one-order pages: sells at 10..50, 10 units each.
	pages := make([][]esi.MarketOrder, 5)
	for i := range pages {
		pages[i] = []esi.MarketOrder{
			{OrderID: int64(2000 + i), TypeID: 34, IsBuyOrder: false, Price: float64(10 * (i + 1)), VolumeRemain: 10},
		}
	}
	transport.books[10000002] = pages

	// The first process gets through two pages, then "dies":
	// the cycle allowance is the only thing that stops it.
	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 2})
	if limited || stored != 2 {
		t.Fatalf("first-process pass: stored=%d limited=%v, want 2/false", stored, limited)
	}
	if got := fmt.Sprint(transport.pagesFor(10000002)); got != "[1 2]" {
		t.Fatalf("first-process pages: %s, want [1 2]", got)
	}
	states, staged := sweepStateCount(t, conn, 10000002)
	if states != 1 || staged != 2 {
		t.Fatalf("after kill: %d state rows, %d staged orders, want 1/2", states, staged)
	}
	var live int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("killed sweep left %d live stats rows, want 0", live)
	}

	// The restarted process resumes at page 3 -- the two staged
	// pages are never re-read -- and stores the whole book's
	// figures, indistinguishable from an uninterrupted sweep.
	restarted := secondProcessApp(t, conn, transport)
	transport.drainRequests()
	stored, limited = restarted.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 3 {
		t.Fatalf("restarted pass: stored=%d limited=%v, want 3/false", stored, limited)
	}
	if got := fmt.Sprint(transport.pagesFor(10000002)); got != "[3 4 5]" {
		t.Fatalf("restarted pages: %s, want [3 4 5] (resume, not restart)", got)
	}
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats after resume: rows=%d err=%v, want 1", len(rows), err)
	}
	row := rows[0]
	if row.BestSell != 10 || row.TypicalSell != 30 || row.SellBand != 50 || row.SellOrders != 5 || row.SellVolume != 50 {
		t.Fatalf("resumed sweep stats: best %v typical %v band %v, %d orders / %d volume, want 10/30/50, 5/50",
			row.BestSell, row.TypicalSell, row.SellBand, row.SellOrders, row.SellVolume)
	}
	states, staged = sweepStateCount(t, conn, 10000002)
	if states != 0 || staged != 0 {
		t.Fatalf("after completion: %d state rows, %d staged orders, want 0/0", states, staged)
	}
}

func TestSweepTransientErrorKeepsStagedProgress(t *testing.T) {
	transport := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 1}},
		{{OrderID: 2, TypeID: 34, IsBuyOrder: false, Price: 6.0, VolumeRemain: 1}},
		{{OrderID: 3, TypeID: 34, IsBuyOrder: false, Price: 7.0, VolumeRemain: 1}},
		{{OrderID: 4, TypeID: 34, IsBuyOrder: false, Price: 8.0, VolumeRemain: 1}},
	}
	transport.failPage(10000002, 3, http.StatusInternalServerError)

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 2 {
		t.Fatalf("failing pass: stored=%d limited=%v, want 2/false (a transient 500 is not the error limit)", stored, limited)
	}
	states, staged := sweepStateCount(t, conn, 10000002)
	if states != 1 || staged != 2 {
		t.Fatalf("after transient failure: %d state rows, %d staged orders, want 1/2 (nothing rolled back)", states, staged)
	}
	var nextPage int64
	if err := conn.QueryRow(`SELECT next_page FROM market_sweep_state WHERE region_id = 10000002`).Scan(&nextPage); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if nextPage != 3 {
		t.Fatalf("cursor after transient failure: page %d, want 3 (the failed page is the retry point)", nextPage)
	}
	var live int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("transient failure stored %d stats rows, want 0", live)
	}

	// ESI heals: the next pass retries page 3, finishes the
	// book, and the stats cover all four pages.
	transport.clearFailures()
	transport.drainRequests()
	stored, limited = app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 2 {
		t.Fatalf("recovery pass: stored=%d limited=%v, want 2/false", stored, limited)
	}
	if got := fmt.Sprint(transport.pagesFor(10000002)); got != "[3 4]" {
		t.Fatalf("recovery pages: %s, want [3 4]", got)
	}
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats after recovery: rows=%d err=%v, want 1", len(rows), err)
	}
	if rows[0].BestSell != 5 || rows[0].TypicalSell != 6.5 || rows[0].SellOrders != 4 {
		t.Fatalf("recovered stats: best %v typical %v, %d orders, want 5/6.5/4", rows[0].BestSell, rows[0].TypicalSell, rows[0].SellOrders)
	}
	states, staged = sweepStateCount(t, conn, 10000002)
	if states != 0 || staged != 0 {
		t.Fatalf("after completion: %d state rows, %d staged orders, want 0/0", states, staged)
	}
}

func TestSweepConcurrentRegionsBothComplete(t *testing.T) {
	transport := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}, delay: 30 * time.Millisecond}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	// Only The Forge and Domain are due; the rest are fresh.
	for _, region := range marketRegions {
		if region.ID != 10000002 && region.ID != 10000043 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 1}},
		{{OrderID: 2, TypeID: 34, IsBuyOrder: false, Price: 6.0, VolumeRemain: 1}},
		{{OrderID: 3, TypeID: 34, IsBuyOrder: true, Price: 4.0, VolumeRemain: 1}},
	}
	transport.books[10000043] = [][]esi.MarketOrder{
		{{OrderID: 4, TypeID: 35, IsBuyOrder: false, Price: 100.0, VolumeRemain: 2}},
		{{OrderID: 5, TypeID: 35, IsBuyOrder: false, Price: 110.0, VolumeRemain: 2}},
		{{OrderID: 6, TypeID: 35, IsBuyOrder: true, Price: 90.0, VolumeRemain: 2}},
	}

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited {
		t.Fatal("concurrent sweep reported the error limit on a healthy stub")
	}
	if stored != 6 {
		t.Fatalf("concurrent sweep read %d pages, want 6 (3 per region)", stored)
	}
	if peak := transport.maxInFlight.Load(); peak < 2 {
		t.Fatalf("peak in-flight requests %d, want >= 2 (regions advance in parallel)", peak)
	}
	for regionID, wantType := range map[int64]int64{10000002: 34, 10000043: 35} {
		rows, err := q.ListMarketRegionStatsByRegion(ctx, regionID)
		if err != nil || len(rows) != 1 || rows[0].TypeID != wantType {
			t.Fatalf("region %d stats after concurrent sweep: %+v err=%v, want one row for type %d", regionID, rows, err, wantType)
		}
		states, staged := sweepStateCount(t, conn, regionID)
		if states != 0 || staged != 0 {
			t.Fatalf("region %d after completion: %d state rows, %d staged orders, want 0/0", regionID, states, staged)
		}
	}
}
