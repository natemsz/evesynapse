package app

// Status pill tests: the Overview pill's combined ESI and
// our-side state, with blame attributed by precedence (ESI down,
// then lag that traces to ESI, then ours-down, then
// ours-lagging, then ESI limited, then the route problems CCP
// itself reports, then healthy).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedPillSnapshot(t *testing.T, q *db.Queries, fetchedAt time.Time) {
	t.Helper()
	if err := q.UpsertGlobalSnapshot(context.Background(), db.UpsertGlobalSnapshotParams{
		Kind:      esi.GlobalStatus,
		Payload:   `{"players": 23456, "server_version": "1132976", "start_time": "2026-10-01T11:00:00Z", "vip": false}`,
		FetchedAt: fetchedAt,
	}); err != nil {
		t.Fatalf("seed status snapshot: %v", err)
	}
}

func seedPillMarket(t *testing.T, q *db.Queries, attemptedAt time.Time) {
	t.Helper()
	for _, region := range sweptRegions {
		if err := q.UpsertMarketFetchState(context.Background(), db.UpsertMarketFetchStateParams{
			Kind: regionSweepKind(region.ID), State: fetchStateOK, AttemptedAt: attemptedAt,
		}); err != nil {
			t.Fatalf("seed market state %d: %v", region.ID, err)
		}
	}
}

func seedPillWorker(app *Application, lastRun time.Time) {
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = lastRun })
}

// (a) ESI down plus market down shows ESI down: our lag is ESI's
// fault then, and must not be blamed on us.
func TestPillESIDownBeatsMarketDown(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now.Add(-20*time.Minute))
	seedPillMarket(t, q, now.Add(-7*time.Hour))
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "down" || view.Label != "ESI down" {
		t.Fatalf("pill = %q/%q, want down/ESI down", view.State, view.Label)
	}
	if view.Players != "" {
		t.Fatalf("players = %q, want none while ESI is down", view.Players)
	}
}

// (b) ESI healthy plus market lagging shows ours-lagging with
// the Sync behind label, the player count, and a hover naming how
// old the market orders are.
func TestPillMarketLaggingShowsSyncBehind(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now.Add(-(marketLagAfter+marketDownAfter)/2))
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "ours-lagging" || view.Label != "Sync behind" {
		t.Fatalf("pill = %q/%q, want ours-lagging/Sync behind", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456 (ESI is answering)", view.Players)
	}
	if !strings.Contains(view.Title, "Market orders are "+pillAge((marketLagAfter+marketDownAfter)/2)+" old") {
		t.Fatalf("title = %q, want it to name the market age", view.Title)
	}
	if !strings.Contains(view.Title, "ESI itself is answering normally") {
		t.Fatalf("title = %q, want it to clear ESI", view.Title)
	}
}

// (c) ESI healthy plus a stalled worker shows ours-down.
func TestPillWorkerStalledShowsSyncStalled(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now)
	seedPillWorker(app, now.Add(-20*time.Minute))

	view := app.esiHealth(ctx)
	if view.State != "ours-down" || view.Label != "Sync stalled" {
		t.Fatalf("pill = %q/%q, want ours-down/Sync stalled", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456 (ESI is answering)", view.Players)
	}
	if !strings.Contains(view.Title, "worker has not started a cycle") {
		t.Fatalf("title = %q, want it to name the worker", view.Title)
	}
}

// (d) ESI limited plus market lagging shows ours-lagging, and
// the hover names both conditions.
func TestPillLimitedPlusLaggingNamesBoth(t *testing.T) {
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":             []string{"application/json"},
				"X-Esi-Error-Limit-Remain": []string{"0"},
				"X-Esi-Error-Limit-Reset":  []string{"60"},
			},
			Body: io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now.Add(-(marketLagAfter+marketDownAfter)/2))
	seedPillWorker(app, now)
	if _, _, err := app.esi.FetchRaw(ctx, "", "/status/"); err != nil {
		t.Fatalf("prime error budget: %v", err)
	}

	view := app.esiHealth(ctx)
	if view.State != "ours-lagging" || view.Label != "Sync behind" {
		t.Fatalf("pill = %q/%q, want ours-lagging/Sync behind", view.State, view.Label)
	}
	if !strings.Contains(view.Title, "Market orders are") || !strings.Contains(view.Title, "close to its error limit") {
		t.Fatalf("title = %q, want it to name both the market lag and the error limit", view.Title)
	}
}

// The standalone status refresh stores a fresh snapshot with
// the worker cycle never run, and the pill reads healthy from
// it: ESI recovery reaches the status surfaces on the refresh's
// own ticker, not on the cycle's schedule.
func TestStatusRefreshStoresFreshSnapshotWithoutWorker(t *testing.T) {
	transport := &intelStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()
	seedPillMarket(t, q, now)
	seedPillWorker(app, now)

	if _, err := q.GetGlobalSnapshot(ctx, esi.GlobalStatus); err == nil {
		t.Fatal("status snapshot exists before the refresh, want none")
	}

	app.refreshServerStatus(ctx)

	snap, err := q.GetGlobalSnapshot(ctx, esi.GlobalStatus)
	if err != nil {
		t.Fatalf("status snapshot after the refresh: %v", err)
	}
	if age := time.Since(snap.FetchedAt); age > time.Minute {
		t.Fatalf("status snapshot is %v old, want fresh", age)
	}
	view := app.esiHealth(ctx)
	if view.State != "ok" || view.Label != "ESI healthy" {
		t.Fatalf("pill = %q/%q, want ok/ESI healthy", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456", view.Players)
	}
}

func seedRouteStatus(t *testing.T, q *db.Queries, fetchedAt time.Time) {
	t.Helper()
	if err := q.UpsertGlobalSnapshot(context.Background(), db.UpsertGlobalSnapshotParams{
		Kind:      esi.GlobalRouteStatus,
		Payload:   `{"routes":[{"method":"GET","path":"/corporations/{corporation_id}/","status":"Degraded"},{"method":"GET","path":"/universe/structures/{structure_id}/","status":"Recovering"}]}`,
		FetchedAt: fetchedAt,
	}); err != nil {
		t.Fatalf("seed route status: %v", err)
	}
}

func seedCleanRouteStatus(t *testing.T, q *db.Queries, fetchedAt time.Time) {
	t.Helper()
	if err := q.UpsertGlobalSnapshot(context.Background(), db.UpsertGlobalSnapshotParams{
		Kind:      esi.GlobalRouteStatus,
		Payload:   `{"routes":[]}`,
		FetchedAt: fetchedAt,
	}); err != nil {
		t.Fatalf("seed clean route status: %v", err)
	}
}

func seedESIErrorStats(t *testing.T, q *db.Queries, now time.Time) {
	t.Helper()
	if err := q.UpsertStatsESI(context.Background(), db.UpsertStatsESIParams{
		Minute: now.Add(-2 * time.Minute), Family: "characters", Calls: 40, Errors: 30,
	}); err != nil {
		t.Fatalf("seed ESI error stats: %v", err)
	}
}

// CCP's own route health degrades the ESI read even when our
// snapshot is fresh and our calls are clean: a fresh stored
// report naming problem routes makes both statusESI and the
// pill read degraded, and a stale report stops speaking. And
// when our sync is behind, either witness of ESI trouble (the
// route report, or our own failing calls) puts the blame on
// ESI in the headline; with both witnesses clean the lag is
// ours and says so.
func TestStoredRouteStatusDegradesESIReads(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now)
	seedPillWorker(app, now)

	if state, _ := app.statusESI(ctx, now, true); state != statusOK {
		t.Fatalf("statusESI with no route report = %q, want ok", state)
	}

	seedRouteStatus(t, q, now)
	if state, _ := app.statusESI(ctx, now, true); state != statusDegraded {
		t.Fatalf("statusESI with a fresh route report = %q, want degraded", state)
	}
	view := app.esiHealth(ctx)
	if view.State != "degraded" || view.Label != "ESI degraded" {
		t.Fatalf("pill = %q/%q, want degraded/ESI degraded", view.State, view.Label)
	}
	if !strings.Contains(view.Title, "corporations") || !strings.Contains(view.Title, "structures") {
		t.Fatalf("title = %q, want it to name the problem areas", view.Title)
	}
	if areas := app.degradedRouteAreas(ctx, now); len(areas) != 2 || areas[0] != "corporations" || areas[1] != "structures" {
		t.Fatalf("areas = %v, want [corporations structures]", areas)
	}

	seedRouteStatus(t, q, now.Add(-esiDownAfter-time.Minute))
	if state, _ := app.statusESI(ctx, now, true); state != statusOK {
		t.Fatalf("statusESI with a stale route report = %q, want ok", state)
	}
	if view := app.esiHealth(ctx); view.State != "ok" {
		t.Fatalf("pill with a stale route report = %q, want ok", view.State)
	}

	// Our sync falls behind. With the route report as witness,
	// the headline blames ESI, not our sync.
	seedPillMarket(t, q, now.Add(-(marketLagAfter+marketDownAfter)/2))
	seedRouteStatus(t, q, now)
	view = app.esiHealth(ctx)
	if view.State != "degraded" || view.Label != "ESI degraded" {
		t.Fatalf("lagging pill with a fresh route report = %q/%q, want degraded/ESI degraded", view.State, view.Label)
	}
	if !strings.Contains(view.Title, "Market orders are") || !strings.Contains(view.Title, "corporations") || !strings.Contains(view.Title, "sync is behind") {
		t.Fatalf("title = %q, want it to name the lag, the areas and the blame", view.Title)
	}

	// Both witnesses clean: the lag is ours and the headline
	// says so.
	seedCleanRouteStatus(t, q, now)
	view = app.esiHealth(ctx)
	if view.State != "ours-lagging" || view.Label != "Sync behind" {
		t.Fatalf("lagging pill with clean witnesses = %q/%q, want ours-lagging/Sync behind", view.State, view.Label)
	}

	// Our own calls failing over the threshold are the second
	// witness: the headline blames ESI again.
	seedESIErrorStats(t, q, now)
	view = app.esiHealth(ctx)
	if view.State != "degraded" || view.Label != "ESI degraded" {
		t.Fatalf("lagging pill with failing calls = %q/%q, want degraded/ESI degraded", view.State, view.Label)
	}
	if !strings.Contains(view.Title, "ESI calls are failing") || !strings.Contains(view.Title, "sync is behind") {
		t.Fatalf("title = %q, want it to name the failing calls and the lag", view.Title)
	}
}

// (e) Everything healthy shows the healthy pill, player count
// included, unchanged.
func TestPillHealthyUnchanged(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now)
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "ok" || view.Label != "ESI healthy" {
		t.Fatalf("pill = %q/%q, want ok/ESI healthy", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456", view.Players)
	}
	if view.Title != "ESI is answering normally." {
		t.Fatalf("title = %q, want the healthy text unchanged", view.Title)
	}
}
