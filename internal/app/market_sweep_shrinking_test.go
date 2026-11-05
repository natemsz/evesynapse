package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// missingPageTransport answers one (region, page) the way ESI
// answers a page past the end of a book, and delegates every
// other request.
type missingPageTransport struct {
	inner  *pagedBookTransport
	region int64
	page   int64
}

func (s *missingPageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var regionID int64
	fmt.Sscanf(req.URL.Path, "/markets/%d/orders/", &regionID)
	page := int64(1)
	fmt.Sscanf(req.URL.Query().Get("page"), "%d", &page)
	if regionID == s.region && page == s.page {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"Requested page does not exist!"}`)),
		}, nil
	}
	return s.inner.RoundTrip(req)
}

// shrinkingBookApp builds a test app whose sweep is aimed at The
// Forge only: every other swept region is settled fresh.
func shrinkingBookApp(t *testing.T, transport http.RoundTripper) (*Application, *db.Queries) {
	t.Helper()
	app, _, q := buildCorpTestApp(t, transport)
	for _, region := range sweptRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	return app, q
}

func shrinkingBook(pages int) [][]esi.MarketOrder {
	book := make([][]esi.MarketOrder, pages)
	for i := range book {
		book[i] = []esi.MarketOrder{
			{OrderID: int64(3000 + i), TypeID: 34, IsBuyOrder: false, Price: float64(10 * (i + 1)), VolumeRemain: 10},
		}
	}
	return book
}

func sweepFetchState(t *testing.T, app *Application) string {
	t.Helper()
	var state string
	if err := app.db.QueryRowContext(context.Background(),
		`SELECT state FROM market_fetch_state WHERE kind = $1`, regionSweepKind(10000002)).Scan(&state); err != nil {
		t.Fatalf("read sweep fetch state: %v", err)
	}
	return state
}

func TestSweepFinalPage404CompletesWithLandedPages(t *testing.T) {
	book := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	transport := &missingPageTransport{inner: book, region: 10000002, page: 3}
	app, q := shrinkingBookApp(t, transport)
	ctx := context.Background()
	book.books[10000002] = shrinkingBook(3)

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 2 {
		t.Fatalf("shrinking sweep: stored=%d limited=%v, want 2/false", stored, limited)
	}
	if app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("shrinking sweep entered backoff for an end-of-book 404")
	}
	states, staged := sweepStateCount(t, app.db, 10000002)
	if states != 0 || staged != 0 {
		t.Fatalf("after completion: %d state rows, %d staged orders, want 0/0", states, staged)
	}
	if got := sweepFetchState(t, app); got != fetchStateOK {
		t.Fatalf("sweep fetch state = %q, want %q (completion stamped)", got, fetchStateOK)
	}
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats after shrinking sweep: rows=%d err=%v, want 1", len(rows), err)
	}
	if rows[0].SellOrders != 2 || rows[0].BestSell != 10 || rows[0].TypicalSell != 15 {
		t.Fatalf("landed stats: %d orders, best %v, typical %v, want 2/10/15 (pages 1-2 only)",
			rows[0].SellOrders, rows[0].BestSell, rows[0].TypicalSell)
	}
}

func TestSweepPage1NotFoundStillFailsAndBacksOff(t *testing.T) {
	book := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	transport := &missingPageTransport{inner: book, region: 10000002, page: 1}
	app, _ := shrinkingBookApp(t, transport)
	ctx := context.Background()
	book.books[10000002] = shrinkingBook(2)

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 0 {
		t.Fatalf("page-1 404 sweep: stored=%d limited=%v, want 0/false", stored, limited)
	}
	if !app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("page-1 404 did not enter backoff")
	}
	states, staged := sweepStateCount(t, app.db, 10000002)
	if states != 1 || staged != 0 {
		t.Fatalf("after page-1 404: %d state rows, %d staged orders, want 1/0", states, staged)
	}
	var live int
	if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("page-1 404 stored %d stats rows, want 0", live)
	}
}

func TestSweepMidBook404CompletesWithStagedPrefix(t *testing.T) {
	book := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	transport := &missingPageTransport{inner: book, region: 10000002, page: 3}
	app, q := shrinkingBookApp(t, transport)
	ctx := context.Background()
	book.books[10000002] = shrinkingBook(5)

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 2 {
		t.Fatalf("mid-book 404 sweep: stored=%d limited=%v, want 2/false", stored, limited)
	}
	if app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("mid-book 404 entered backoff")
	}
	states, staged := sweepStateCount(t, app.db, 10000002)
	if states != 0 || staged != 0 {
		t.Fatalf("after mid-book completion: %d state rows, %d staged orders, want 0/0", states, staged)
	}
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats after mid-book 404: rows=%d err=%v, want 1", len(rows), err)
	}
	if rows[0].SellOrders != 2 || rows[0].TypicalSell != 15 {
		t.Fatalf("prefix stats: %d orders, typical %v, want 2/15 (pages 1-2 only)", rows[0].SellOrders, rows[0].TypicalSell)
	}
}

// timeoutPageTransport answers one (region, page) with a deadline
// error and delegates every other request.
type timeoutPageTransport struct {
	inner  *pagedBookTransport
	region int64
	page   int64
}

func (s *timeoutPageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var regionID int64
	fmt.Sscanf(req.URL.Path, "/markets/%d/orders/", &regionID)
	page := int64(1)
	fmt.Sscanf(req.URL.Query().Get("page"), "%d", &page)
	if regionID == s.region && page == s.page {
		return nil, fmt.Errorf("read page: %w", context.DeadlineExceeded)
	}
	return s.inner.RoundTrip(req)
}

func TestSweepFinalPageTimeoutStillFailsAndBacksOff(t *testing.T) {
	book := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	transport := &timeoutPageTransport{inner: book, region: 10000002, page: 3}
	app, _ := shrinkingBookApp(t, transport)
	ctx := context.Background()
	book.books[10000002] = shrinkingBook(3)

	stored, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if limited || stored != 2 {
		t.Fatalf("final-page timeout sweep: stored=%d limited=%v, want 2/false (pages before it stay staged)", stored, limited)
	}
	if !app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("final-page timeout did not enter backoff")
	}
	states, staged := sweepStateCount(t, app.db, 10000002)
	if states != 1 || staged != 2 {
		t.Fatalf("after final-page timeout: %d state rows, %d staged orders, want 1/2", states, staged)
	}
	var nextPage int64
	if err := app.db.QueryRowContext(ctx, `SELECT next_page FROM market_sweep_state WHERE region_id = 10000002`).Scan(&nextPage); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if nextPage != 3 {
		t.Fatalf("cursor after final-page timeout: page %d, want 3", nextPage)
	}
	var live int
	if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	if live != 0 {
		t.Fatalf("final-page timeout stored %d stats rows, want 0", live)
	}
}
