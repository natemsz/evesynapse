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

// liveStats counts The Forge's stored stats rows.
func liveStats(t *testing.T, app *Application) int {
	t.Helper()
	var live int
	if err := app.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM market_region_stats WHERE region_id = 10000002`).Scan(&live); err != nil {
		t.Fatalf("count live stats: %v", err)
	}
	return live
}

func TestSweepPage404PastPage1WaitsForTheNextWindow(t *testing.T) {
	book := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}}
	transport := &missingPageTransport{inner: book, region: 10000002, page: 3}
	app, _ := shrinkingBookApp(t, transport)
	ctx := context.Background()
	book.books[10000002] = shrinkingBook(5)

	if _, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000}); limited {
		t.Fatal("shrinking sweep reported the error limit")
	}
	if app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("a book that shrank mid-read entered the failure backoff")
	}
	if live := liveStats(t, app); live != 0 {
		t.Fatalf("a book that shrank mid-read stored %d stats rows, want 0", live)
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
		t.Fatalf("page-1 404 sweep: read=%d limited=%v, want 0/false", stored, limited)
	}
	if !app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("page-1 404 did not enter backoff")
	}
	if live := liveStats(t, app); live != 0 {
		t.Fatalf("page-1 404 stored %d stats rows, want 0", live)
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

	if _, limited := app.sweepRegionStats(ctx, &fetchBudget{left: 1000}); limited {
		t.Fatal("final-page timeout reported the error limit")
	}
	if !app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("final-page timeout did not enter backoff")
	}
	if live := liveStats(t, app); live != 0 {
		t.Fatalf("final-page timeout stored %d stats rows, want 0", live)
	}
}
