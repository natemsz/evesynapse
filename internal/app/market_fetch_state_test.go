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
			AttemptedAt: time.Now().UTC().Add(-c.ago).Format(time.RFC3339),
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
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`[]`)),
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
		if app.marketFetchDue(ctx, kind, historyRefetchGate) {
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
	if app.marketFetchDue(ctx, kind, historyRefetchGate) {
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
