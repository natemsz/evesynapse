package app

import (
	"context"
	"testing"
	"time"

	"evesynapse/internal/app/swarm"
	db "evesynapse/internal/db/sqlc"
)

// TestSwarmHistoryApplyBatchesDaysAndKeepsTheLastRepeat: one
// apply of a history payload lands every in-window day, and a
// date the payload repeats lands with its last values, as the
// day-by-day upserts landed it.
func TestSwarmHistoryApplyBatchesDaysAndKeepsTheLastRepeat(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	fetched := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	origin := swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: fetched}
	payload := `{"region_id":10000002,"type_id":34,"days":[` +
		`{"date":"2026-10-03","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2},` +
		`{"date":"2026-10-04","average":7,"highest":8,"lowest":6,"volume":20,"order_count":3},` +
		`{"date":"2026-10-03","average":9,"highest":11,"lowest":8,"volume":30,"order_count":4}]}`
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindHistory, "10000002/34", payload, origin, fetched.Add(time.Hour)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	days, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 20})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("history rows %+v, want the two distinct dates", days)
	}
	byDate := make(map[string]db.MarketHistory, len(days))
	for _, d := range days {
		byDate[d.Date] = d
	}
	d3 := byDate["2026-10-03"]
	if d3.Average != 9 || d3.Highest != 11 || d3.Lowest != 8 || d3.Volume != 30 || d3.OrderCount != 4 {
		t.Fatalf("repeated date landed %+v, want its last values", d3)
	}
	d4 := byDate["2026-10-04"]
	if d4.Average != 7 || d4.Highest != 8 || d4.Lowest != 6 || d4.Volume != 20 || d4.OrderCount != 3 {
		t.Fatalf("plain date landed %+v, want its payload values", d4)
	}
}
