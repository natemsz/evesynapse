package app

// Tests for the Admin analytics recorder and page: the flush
// aggregates what the middleware and ticks counted, the hourly
// housekeeping rolls old minute rows up and prunes, and the
// page renders from stored rows with an honest empty state.

import (
	"context"
	"net/http"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestStatsRecorderFlushAggregates(t *testing.T) {
	app, _, q := NewCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	rec := app.recorder()

	rec.countRequest("/market/", http.StatusOK, 30*time.Millisecond)
	rec.countRequest("/market/", http.StatusOK, 42*time.Millisecond)
	rec.countRequest("/market/", http.StatusOK, 12*time.Millisecond)
	rec.countRequest("/market/", http.StatusNotFound, 3*time.Millisecond)
	rec.countRequest("", http.StatusTeapot, time.Millisecond) // unmatched route group
	rec.tick(statsEventSignIn)
	rec.tick(statsEventSignIn)

	now := time.Now().UTC()
	app.flushStats(ctx, workerTiming{
		At: now, Took: 90 * time.Second, Fetches: 10, Budget: 100,
		Deferred: 2, Characters: 3,
		Phases: []phaseTiming{{Name: "characters", Took: time.Second}},
	})

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)
	traffic, err := q.StatsTrafficMinutes(ctx, db.StatsTrafficMinutesParams{FromTs: from, ToTs: to, StepSeconds: 3600})
	if err != nil {
		t.Fatalf("traffic read: %v", err)
	}
	var total int64
	byClass := map[int16]int64{}
	for _, r := range traffic {
		total += r.Requests
		byClass[r.StatusClass] += r.Requests
	}
	if total != 5 || byClass[2] != 3 || byClass[4] != 2 {
		t.Errorf("traffic after flush: total %d, by class %v; want 5 total, 3 x 2xx, 2 x 4xx (418 counts as 4xx)", total, byClass)
	}

	events, err := q.StatsEventsWindowMinutes(ctx, db.StatsEventsWindowMinutesParams{FromTs: from, ToTs: to})
	if err != nil {
		t.Fatalf("events read: %v", err)
	}
	if len(events) != 1 || events[0].Kind != statsEventSignIn || events[0].Count != 2 {
		t.Errorf("events after flush: %v; want one signin row of 2", events)
	}

	sum, err := q.StatsCycleSummary(ctx, db.StatsCycleSummaryParams{FromTs: from, ToTs: to})
	if err != nil {
		t.Fatalf("cycle summary: %v", err)
	}
	if sum.Cycles != 1 || sum.AvgMs != 90000 || sum.Fetches != 10 {
		t.Errorf("cycle summary: %+v; want 1 cycle, avg 90000 ms, 10 fetches", sum)
	}
	phases, err := q.StatsCyclePhasesWindow(ctx, db.StatsCyclePhasesWindowParams{FromTs: from, ToTs: to})
	if err != nil {
		t.Fatalf("cycle phases: %v", err)
	}
	if len(phases) != 1 || phases[0].Phase != "characters" || phases[0].TookSumMs != 1000 {
		t.Errorf("cycle phases: %v; want the characters phase at 1000 ms", phases)
	}

	// A second flush with nothing counted writes no new traffic:
	// the counters were drained, not copied.
	app.flushStats(ctx, workerTiming{At: now.Add(time.Minute), Took: time.Second})
	traffic, err = q.StatsTrafficMinutes(ctx, db.StatsTrafficMinutesParams{FromTs: from, ToTs: to, StepSeconds: 3600})
	if err != nil {
		t.Fatalf("traffic re-read: %v", err)
	}
	total = 0
	for _, r := range traffic {
		total += r.Requests
	}
	if total != 5 {
		t.Errorf("traffic after empty flush: total %d; want still 5", total)
	}
}

func TestStatsRetentionRollupAndPrune(t *testing.T) {
	app, conn, q := NewCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour).Truncate(time.Minute)
	recent := now.Add(-time.Hour).Truncate(time.Minute)

	seed := func(minute time.Time, n int64) {
		t.Helper()
		if err := q.UpsertStatsRequest(ctx, db.UpsertStatsRequestParams{
			Minute: minute, RouteGroup: "/", StatusClass: 2, Requests: n,
		}); err != nil {
			t.Fatalf("seed request: %v", err)
		}
	}
	seed(old, 7)
	seed(recent, 3)
	if err := q.InsertStatsCycle(ctx, db.InsertStatsCycleParams{
		At: old, TookMs: 5000, Fetches: 1, Budget: 1, Characters: 1,
	}); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}
	// An hourly row past the 90-day prune horizon.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO stats_requests_hourly (hour, route_group, status_class, requests) VALUES ($1, '/', 2, 11)`,
		now.Add(-100*24*time.Hour).Truncate(time.Hour)); err != nil {
		t.Fatalf("seed hourly: %v", err)
	}

	housekeep := func() {
		t.Helper()
		app.stats = newStatsRecorder() // reset the hourly gate
		tx, err := app.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if err := app.statsHousekeep(ctx, app.queries.WithTx(tx), app.recorder(), now); err != nil {
			t.Fatalf("housekeep: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	housekeep()

	// The 8-day-old minute rows rolled into the hourly table and
	// left the minute table; the recent row stayed put.
	hours, err := q.StatsTrafficHours(ctx, db.StatsTrafficHoursParams{
		FromTs: old.Add(-time.Hour), ToTs: old.Add(2 * time.Hour), StepSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("hourly read: %v", err)
	}
	var rolled int64
	for _, r := range hours {
		rolled += r.Requests
	}
	if rolled != 7 {
		t.Errorf("hourly rows around the old minute: %d requests; want the rolled-up 7", rolled)
	}
	mins, err := q.StatsTrafficMinutes(ctx, db.StatsTrafficMinutesParams{
		FromTs: old.Add(-time.Hour), ToTs: now.Add(time.Hour), StepSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("minute read: %v", err)
	}
	var left int64
	for _, r := range mins {
		left += r.Requests
	}
	if left != 3 {
		t.Errorf("minute rows after rollup: %d requests; want only the recent 3", left)
	}
	cycles, err := q.StatsCycleSummaryHours(ctx, db.StatsCycleSummaryHoursParams{
		FromTs: old.Add(-time.Hour), ToTs: old.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("cycle hours: %v", err)
	}
	if cycles.Cycles != 1 {
		t.Errorf("rolled cycles: %d; want 1", cycles.Cycles)
	}
	// The 100-day-old hourly row was pruned.
	var pruned int64
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM stats_requests_hourly WHERE requests = 11`).Scan(&pruned); err != nil {
		t.Fatalf("prune check: %v", err)
	}
	if pruned != 0 {
		t.Errorf("hourly rows past 90 days still present: %d", pruned)
	}
}

func TestAdminAnalyticsRenders(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()
	admin, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create admin account: %v", err)
	}
	seedCharacter(t, q, admin.ID, fixtureCharA, "Admin Main")
	asAdmin := sessionCookie(t, app, admin.ID, fixtureCharA, "Admin Main")

	// A fresh instance has recorded nothing: the section says so
	// instead of charting zeros.
	code, body := getPage(t, app, asAdmin, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/: status %d, want 200", code)
	}
	mustContain(t, "/admin/ (empty)", body,
		"Analytics", "No analytics yet", "Data freshness", "Analytics section:")

	// Seed the stored aggregates the section reads.
	now := time.Now().UTC()
	minute := now.Add(-30 * time.Minute).Truncate(time.Minute)
	if err := q.UpsertStatsRequest(ctx, db.UpsertStatsRequestParams{
		Minute: minute, RouteGroup: "/", StatusClass: 2, Requests: 120,
	}); err != nil {
		t.Fatalf("seed traffic: %v", err)
	}
	if err := q.UpsertStatsDuration(ctx, db.UpsertStatsDurationParams{
		Minute: minute, RouteGroup: "/", Bucket: 4, Hits: 120,
	}); err != nil {
		t.Fatalf("seed durations: %v", err)
	}
	if err := q.InsertStatsCycle(ctx, db.InsertStatsCycleParams{
		At: minute, TookMs: 45000, Fetches: 8, Budget: 100, Characters: 2,
	}); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}
	if err := q.UpsertStatsHost(ctx, db.UpsertStatsHostParams{
		Minute: minute, Load1: 1.25, Load5: 1, Load15: 1,
		MemUsedBytes: 8 << 30, MemTotalBytes: 20 << 30, AppRssBytes: 400 << 20,
		Goroutines: 120, DiskUsedBytes: 100 << 30, DiskTotalBytes: 200 << 30,
	}); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	hour := now.Truncate(time.Hour)
	if err := q.UpsertStatsInventory(ctx, db.UpsertStatsInventoryParams{
		Hour: hour, Name: "", RowsEst: 0, SizeBytes: 6 << 30,
	}); err != nil {
		t.Fatalf("seed inventory db: %v", err)
	}
	if err := q.UpsertStatsInventory(ctx, db.UpsertStatsInventoryParams{
		Hour: hour, Name: "market_orders", RowsEst: 1000, SizeBytes: 300 << 20,
	}); err != nil {
		t.Fatalf("seed inventory table: %v", err)
	}

	code, body = getPage(t, app, asAdmin, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/ (seeded): status %d, want 200", code)
	}
	mustContain(t, "/admin/ (seeded)", body,
		"Traffic", "Worker cycles", "Host", "ESI calls · 24h", "Growth",
		"Top pages · 24h", "Activity · 24h", "Data freshness", "Swarm",
		"Data inventory", "market_orders", "Requests · 24h", "120")

	// The 30-day range reads the hourly rollups; with none
	// seeded it answers with the honest notes, not an error.
	code, body = getPage(t, app, asAdmin, "/admin/?range=30d")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/?range=30d: status %d, want 200", code)
	}
	mustContain(t, "/admin/?range=30d", body, "ESI calls · 30 days", "No requests recorded in this window yet")
}
