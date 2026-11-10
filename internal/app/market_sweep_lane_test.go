package app

// Region sweep tests on the lane's read: parallel regions, and a
// read whose pages came from two of ESI's cache windows. The
// distilled stats themselves are covered by the region-stats and
// scanner suites, which drive the same store.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	failPages   map[[2]int64]int    // (region, page) -> status answered instead of the book
	modified    map[[2]int64]string // (region, page) -> Last-Modified answered, when set
	delay       time.Duration       // artificial latency per request
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
	modified := s.modified[[2]int64{regionID, page}]
	pages := s.books[regionID]
	s.mu.Unlock()
	if status > 0 {
		// Retry-After: 0 keeps the client's read retries from
		// sleeping on an injected 500: the failure stands either way.
		failed := header.Clone()
		failed.Set("Retry-After", "0")
		return &http.Response{
			StatusCode: status, Header: failed,
			Body: io.NopCloser(strings.NewReader(`{"error":"injected failure"}`)),
		}, nil
	}
	header.Set("X-Pages", fmt.Sprintf("%d", len(pages)))
	if modified != "" {
		header.Set("Last-Modified", modified)
	}
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

// pagesFor returns the pages requested for one region, in page
// order: a sweep reads a window of pages at once, so the order
// they were asked in is not fixed.
func (s *pagedBookTransport) pagesFor(regionID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, r := range s.requests {
		if r[0] == regionID {
			out = append(out, r[1])
		}
	}
	slices.Sort(out)
	return out
}

// drainRequests forgets every recorded request, so a test can
// assert exactly what the next pass fetched.
func (s *pagedBookTransport) drainRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func TestSweepConcurrentRegionsBothComplete(t *testing.T) {
	transport := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}, delay: 30 * time.Millisecond}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	// Only The Forge and Domain are due; every other swept
	// region (the global market included) is fresh.
	for _, region := range sweptRegions {
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
	}
}

func TestSweepMixedWindowStoresNothing(t *testing.T) {
	transport := &pagedBookTransport{books: map[int64][][]esi.MarketOrder{}, modified: map[[2]int64]string{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range sweptRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 1}},
		{{OrderID: 2, TypeID: 34, IsBuyOrder: false, Price: 6.0, VolumeRemain: 1}},
	}
	// Page 2 answers from the window after page 1's.
	transport.modified[[2]int64{10000002, 1}] = "Tue, 10 Nov 2026 01:25:13 GMT"
	transport.modified[[2]int64{10000002, 2}] = "Tue, 10 Nov 2026 01:30:13 GMT"

	app.sweepRegionStats(ctx, &fetchBudget{left: 1000})
	if rows, err := q.ListMarketRegionStatsByRegion(ctx, 10000002); err != nil || len(rows) != 0 {
		t.Fatalf("a read from two windows stored %d rows (err %v), want none", len(rows), err)
	}
	if app.sweepBackedOff(10000002, time.Now()) {
		t.Fatal("a mixed read entered the failure backoff; it waits for the next rollover instead")
	}
}
