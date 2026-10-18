package app

// Audit 3.2.4 (home half): the Briefing's market lines, the
// market widget's health line and the watchlist widget used to
// query order health and the watchlist separately, so one home
// render read both tables up to three times. They now share the
// render's single lazy fetch: repeat reads serve the snapshot
// the first read took, even if the tables move underneath.

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestHomeMarketSharedFetchesOnce(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
		CharacterID: fixtureCharA, OrderID: 700, TypeID: 587, RegionID: 10000002, LocationID: 60003760,
		MyPrice: 1000000, StationBest: 990000, RegionBest: 990000, Status: "undercut_station", ComputedAt: now,
	}); err != nil {
		t.Fatalf("seed order health: %v", err)
	}
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 34, RegionID: 10000002,
		ThresholdPct: 5, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}

	shared := &homeMarketShared{app: app, userID: user.ID}
	if got := shared.orderHealth(ctx); len(got) != 1 || got[0].Status != "undercut_station" {
		t.Fatalf("first order-health read = %+v, want the seeded verdict", got)
	}
	if got := shared.watchlist(ctx); len(got) != 1 || got[0].TypeID != 34 {
		t.Fatalf("first watchlist read = %+v, want the seeded entry", got)
	}

	// Wipe both tables behind the render's back: a second read
	// must serve the rows the first one fetched, not re-query.
	if _, err := conn.ExecContext(ctx, `DELETE FROM order_health`); err != nil {
		t.Fatalf("wipe order health: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM market_watchlist`); err != nil {
		t.Fatalf("wipe watchlist: %v", err)
	}
	if got := shared.orderHealth(ctx); len(got) != 1 {
		t.Fatalf("second order-health read = %+v, want the render's snapshot", got)
	}
	if got := shared.watchlist(ctx); len(got) != 1 {
		t.Fatalf("second watchlist read = %+v, want the render's snapshot", got)
	}

	// A fresh render (a new shared fetch) does see the wipe.
	fresh := &homeMarketShared{app: app, userID: user.ID}
	if got := fresh.orderHealth(ctx); len(got) != 0 {
		t.Fatalf("fresh order-health read = %+v, want none after the wipe", got)
	}
	if got := fresh.watchlist(ctx); len(got) != 0 {
		t.Fatalf("fresh watchlist read = %+v, want none after the wipe", got)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("shared home fetches made %d outbound calls, want 0", got)
	}
}
