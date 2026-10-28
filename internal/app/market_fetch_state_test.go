package app

// Market fetch-lane tests: the market pass runs on its own
// allowance (region sweeps advance with no character budget
// anywhere in sight), and failed market fetches settle --
// a definitive failure (dead) waits out the full refetch
// gate, a transient one backs off instead of retrying on
// every pass. Both regress the 2026-10-04 live stall: the
// sweeps silently starved behind character warming while
// two poisoned history pairs refetched every 5 seconds.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestMarketFetchDueGates(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	gate := 20 * time.Hour

	if !app.marketFetchDue(ctx, "history_1_1", gate) {
		t.Fatal("missing record: not due, want due")
	}
	cases := []struct {
		kind  string
		state string
		ago   time.Duration
		due   bool
	}{
		{"history_2_1", fetchStateOK, time.Minute, false},
		{"history_2_2", fetchStateOK, 21 * time.Hour, true},
		{"history_2_3", fetchStateError, time.Minute, false},
		{"history_2_4", fetchStateError, marketFetchErrorGate + time.Minute, true},
		{"history_2_5", fetchStateDead, time.Minute, false},
		{"history_2_6", fetchStateDead, 21 * time.Hour, true},
	}
	for _, c := range cases {
		if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
			Kind: c.kind, State: c.state,
			AttemptedAt: time.Now().UTC().Add(-c.ago),
		}); err != nil {
			t.Fatalf("seed %s: %v", c.kind, err)
		}
		if got := app.marketFetchDue(ctx, c.kind, gate); got != c.due {
			t.Fatalf("%s (%s, %v ago): due=%v, want %v", c.kind, c.state, c.ago, got, c.due)
		}
	}
}

// historyStatusTransport answers history reads with a
// per-type status code (200 with an empty body otherwise).
type historyStatusTransport struct {
	calls  atomic.Int64
	byType map[int64]int
}

func (s *historyStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	var typeID int64
	fmt.Sscanf(req.URL.Query().Get("type_id"), "%d", &typeID)
	code := http.StatusOK
	if c, ok := s.byType[typeID]; ok {
		code = c
	}
	// Retry-After: 0 keeps the client's read retries from
	// sleeping on the 500 case: the answer stays a 500 either way.
	return &http.Response{
		StatusCode: code,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"0"},
		},
		Body: io.NopCloser(strings.NewReader(`[]`)),
	}, nil
}

func TestFetchAndStoreHistoryDefinitiveFailureSettles(t *testing.T) {
	transport := &historyStatusTransport{byType: map[int64]int{
		99001: http.StatusNotFound,
		99002: http.StatusBadRequest,
		99003: http.StatusInternalServerError,
	}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	for _, typeID := range []int64{99001, 99002} {
		key := marketKey{RegionID: 10000002, TypeID: typeID}
		if fetched, limited := app.fetchAndStoreHistory(ctx, key); fetched || limited {
			t.Fatalf("type %d: fetched=%v limited=%v, want false/false", typeID, fetched, limited)
		}
		kind := marketFetchKind("history", key)
		state, err := q.GetMarketFetchState(ctx, kind)
		if err != nil || state.State != fetchStateDead {
			t.Fatalf("type %d: state=%v err=%v, want dead", typeID, state.State, err)
		}
		if app.historyFetchDue(ctx, key) {
			t.Fatalf("type %d: dead pair due immediately, want gated", typeID)
		}
	}

	key := marketKey{RegionID: 10000002, TypeID: 99003}
	if fetched, limited := app.fetchAndStoreHistory(ctx, key); fetched || limited {
		t.Fatalf("transient: fetched=%v limited=%v, want false/false", fetched, limited)
	}
	kind := marketFetchKind("history", key)
	state, err := q.GetMarketFetchState(ctx, kind)
	if err != nil || state.State != fetchStateError {
		t.Fatalf("transient: state=%v err=%v, want error", state.State, err)
	}
	if app.historyFetchDue(ctx, key) {
		t.Fatal("transient: error due immediately, want error-gate backoff")
	}
}

func TestRefreshMarketDataRunsSweepOnItsOwnLane(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{
		{
			{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 5.0, VolumeRemain: 100},
			{OrderID: 2, TypeID: 34, IsBuyOrder: true, Price: 4.0, VolumeRemain: 50},
		},
	}
	for i := 0; i < 10; i++ {
		if _, limited := app.refreshMarketData(ctx, nil); limited {
			t.Fatal("market pass hit the error limit on a healthy stub")
		}
		var open int
		if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM market_sweep_state`).Scan(&open); err != nil {
			t.Fatalf("count in-progress sweeps: %v", err)
		}
		if open == 0 {
			break
		}
	}
	rows, err := q.ListMarketRegionStatsByType(ctx, 34)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stats for type 34 after market pass: rows=%d err=%v, want 1", len(rows), err)
	}
}

// historyBodyTransport answers every history read with one body.
type historyBodyTransport struct{ body string }

func (h historyBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(h.body)),
	}, nil
}

// A pair's whole payload lands in one statement, and a day ESI
// repeats is stored once with its last answer.
func TestFetchAndStoreHistoryStoresEveryDayOnce(t *testing.T) {
	app, _, q := buildCorpTestApp(t, historyBodyTransport{body: `[
		{"date":"2026-10-01","average":10,"highest":11,"lowest":9,"volume":100,"order_count":5},
		{"date":"2026-10-02","average":20,"highest":21,"lowest":19,"volume":200,"order_count":6},
		{"date":"2026-10-02","average":30,"highest":31,"lowest":29,"volume":300,"order_count":7}
	]`})
	ctx := context.Background()
	key := marketKey{RegionID: 10000002, TypeID: 34}
	if fetched, limited := app.fetchAndStoreHistory(ctx, key); !fetched || limited {
		t.Fatalf("fetched=%v limited=%v, want true/false", fetched, limited)
	}
	rows, err := q.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: key.RegionID, TypeID: key.TypeID, RowLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored %d days, want 2", len(rows))
	}
	if rows[0].Date != "2026-10-02" || rows[0].Average != 30 || rows[0].Volume != 300 {
		t.Fatalf("repeated day = %+v, want its last answer", rows[0])
	}
}

// The batched gate read keeps the history rule: a pair fetched since
// ESI last published a day is skipped, an older or missing one fetched.
func TestWarmMarketHistoryBatchedGate(t *testing.T) {
	transport := &historyStatusTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	fresh := marketKey{RegionID: 10000002, TypeID: 34}
	stale := marketKey{RegionID: 10000002, TypeID: 35}
	for _, k := range []marketKey{fresh, stale} {
		if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: k.RegionID, TypeID: k.TypeID, LastRequestedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for k, at := range map[marketKey]time.Time{fresh: now, stale: module.HistoryPublishedAt(now).Add(-time.Minute)} {
		if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
			Kind: marketFetchKind("history", k), State: fetchStateOK,
			AttemptedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stored, limited := app.warmMarketHistory(ctx, &fetchBudget{left: 10})
	if limited || stored != 1 || transport.calls.Load() != 1 {
		t.Fatalf("stored=%d limited=%v calls=%d, want 1/false/1", stored, limited, transport.calls.Load())
	}
}

// A history fetch stays current until ESI next publishes a day: a
// fetch just before the publish moment is due right after it, one
// just after is not due again until the next day's.
func TestHistoryCurrent(t *testing.T) {
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	cases := []struct {
		name           string
		attempted, now time.Time
		current        bool
	}{
		{"before publish, asked before publish", at(9, 0), at(10, 0), true},
		{"before publish, asked after it", at(11, 0), at(12, 0), false},
		{"after publish, same day", at(11, 31), at(23, 0), true},
		{"after publish, next morning", at(11, 31), at(34, 0), true},
		{"after publish, next day after publish", at(11, 31), at(36, 0), false},
		{"yesterday evening, this morning", at(-4, 0), at(9, 0), true},
	}
	for _, c := range cases {
		if got := module.HistoryCurrent(c.attempted, c.now); got != c.current {
			t.Errorf("%s: current=%v, want %v", c.name, got, c.current)
		}
	}
}
