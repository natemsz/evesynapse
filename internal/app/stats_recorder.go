package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Admin analytics recorder. Everything the Analytics section
// charts is collected here, in memory, and written by the worker:
// request and event counters tick on atomically cheap map bumps
// (a short lock, no I/O, no formatting), the worker flushes them
// once a cycle into per-minute aggregate rows, and an hourly
// housekeeping pass inside the same flush snapshots the data
// inventory and rolls minute rows older than a week into hourly
// rows kept for 90 days. Nothing is written per request, and the
// Admin page reads only the stored aggregates.
//
// Two of the Activity counts tick here because no table owns
// them: sign-ins (a session's creation time is not stored) and
// contract searches. The rest are counted from their tables at
// render.
// ---------------------------------------------------------------------------

// Event kinds for stats_events_minute.
const (
	statsEventSignIn         = "signin"
	statsEventContractSearch = "contract_search"
)

// statsDurationEdgesMs are the response-time histogram edges in
// milliseconds: bucket i counts durations over edge i-1 and at or
// under edge i; the final bucket counts everything above the last
// edge. The Analytics page estimates medians and p95s from these.
var statsDurationEdgesMs = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func statsDurationBucket(d time.Duration) int16 {
	ms := d.Milliseconds()
	for i, edge := range statsDurationEdgesMs {
		if ms <= edge {
			return int16(i)
		}
	}
	return int16(len(statsDurationEdgesMs))
}

// statsPercentileMs estimates the p-th percentile (0..100) of a
// histogram in the shared bucket layout: the upper edge of the
// bucket where the cumulative count crosses p. ok is false when
// the histogram is empty.
func statsPercentileMs(hits []int64, p float64) (float64, bool) {
	var total int64
	for _, h := range hits {
		total += h
	}
	if total == 0 {
		return 0, false
	}
	target := float64(total) * p / 100
	var cum int64
	for i, h := range hits {
		cum += h
		if float64(cum) >= target {
			if i < len(statsDurationEdgesMs) {
				return float64(statsDurationEdgesMs[i]), true
			}
			return float64(statsDurationEdgesMs[len(statsDurationEdgesMs)-1]) * 2, true
		}
	}
	return 0, false
}

type statsReqKey struct {
	minute int64 // unix minutes
	group  string
	class  int16
}

type statsDurKey struct {
	minute int64
	group  string
	bucket int16
}

type statsEventKey struct {
	minute int64
	kind   string
}

// statsRecorder is the in-memory side of the analytics pipeline.
type statsRecorder struct {
	mu     sync.Mutex
	reqs   map[statsReqKey]int64
	durs   map[statsDurKey]int64
	events map[statsEventKey]int64
	// lastHousekeep is the hour the inventory snapshot and the
	// rollup last ran for; zero until the first flush.
	lastHousekeep time.Time
}

func newStatsRecorder() *statsRecorder {
	return &statsRecorder{
		reqs:   make(map[statsReqKey]int64),
		durs:   make(map[statsDurKey]int64),
		events: make(map[statsEventKey]int64),
	}
}

// recorder returns the application's shared recorder, created on
// first use so every Application construction (production and
// tests alike) has one.
func (app *Application) recorder() *statsRecorder {
	app.statsMu.Lock()
	defer app.statsMu.Unlock()
	if app.stats == nil {
		app.stats = newStatsRecorder()
	}
	return app.stats
}

// countRequest records one served request under its route group
// and status class, with its duration in the shared histogram.
func (r *statsRecorder) countRequest(group string, status int, d time.Duration) {
	if group == "" {
		group = "unmatched"
	}
	if status < 100 || status > 599 {
		status = 200
	}
	class := int16(status / 100)
	if class < 2 {
		class = 2
	}
	minute := time.Now().Unix() / 60
	r.mu.Lock()
	r.reqs[statsReqKey{minute, group, class}]++
	r.durs[statsDurKey{minute, group, statsDurationBucket(d)}]++
	r.mu.Unlock()
}

// tick records one site event (a sign-in, a contract search).
func (r *statsRecorder) tick(kind string) { r.tickN(kind, 1) }

// tickN adds n to one event counter (the swarm's bytes-served
// counter adds byte counts).
func (r *statsRecorder) tickN(kind string, n int64) {
	if n == 0 {
		return
	}
	minute := time.Now().Unix() / 60
	r.mu.Lock()
	r.events[statsEventKey{minute, kind}] += n
	r.mu.Unlock()
}

// NoteActivity is the module-facing event tick (module.Deps).
func (app *Application) NoteActivity(kind string) { app.recorder().tick(kind) }

// NoteActivityN is the module-facing counted tick (module.Deps).
func (app *Application) NoteActivityN(kind string, n int64) { app.recorder().tickN(kind, n) }

// drain swaps the accumulated counters out; the caller owns the
// returned maps.
func (r *statsRecorder) drain() (map[statsReqKey]int64, map[statsDurKey]int64, map[statsEventKey]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs, durs, events := r.reqs, r.durs, r.events
	r.reqs = make(map[statsReqKey]int64)
	r.durs = make(map[statsDurKey]int64)
	r.events = make(map[statsEventKey]int64)
	return reqs, durs, events
}

// countRequestMiddleware counts every request the router serves.
// It runs after routing, so the route pattern (the route group)
// is known; the counting itself is two map bumps under a short
// lock, with no I/O and no allocation beyond the pattern string
// chi already built.
func (app *Application) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		group := ""
		if rc := chi.RouteContext(r.Context()); rc != nil {
			group = rc.RoutePattern()
		}
		app.recorder().countRequest(group, ww.Status(), time.Since(start))
	})
}

// ---------------------------------------------------------------------------
// Host sampling: /proc and the Go runtime, read once per flush by
// the worker, never at render. Any source that is unavailable
// (non-Linux development) leaves its fields zero and hasHost
// false, and the page says so instead of inventing numbers.
// ---------------------------------------------------------------------------

type statsHostSample struct {
	load1, load5, load15 float64
	memUsed, memTotal    int64
	appRSS               int64
	goroutines           int
	diskUsed, diskTotal  int64
}

func sampleHost() (statsHostSample, bool) {
	var s statsHostSample
	load, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return s, false
	}
	fields := strings.Fields(string(load))
	if len(fields) < 3 {
		return s, false
	}
	for i, dst := range []*float64{&s.load1, &s.load5, &s.load15} {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return s, false
		}
		*dst = v
	}
	if mem, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, available int64
		for _, line := range strings.Split(string(mem), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = kb * 1024
			case "MemAvailable:":
				available = kb * 1024
			}
		}
		s.memTotal, s.memUsed = total, total-available
	}
	if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
		f := strings.Fields(string(statm))
		if len(f) >= 2 {
			pages, _ := strconv.ParseInt(f[1], 10, 64)
			s.appRSS = pages * int64(os.Getpagesize())
		}
	}
	s.goroutines = runtime.NumGoroutine()
	s.diskUsed, s.diskTotal = diskUsage(".")
	return s, true
}

// ---------------------------------------------------------------------------
// The flush. Called by refreshCycle once per finished cycle with
// that cycle's timing, so per-minute rows land about once a
// minute and the cycle row rides the same transaction.
// ---------------------------------------------------------------------------

func (app *Application) flushStats(ctx context.Context, t workerTiming) {
	rec := app.recorder()
	reqs, durs, events := rec.drain()
	families := app.esi.TakeFamilyCounts()
	now := time.Now().UTC()
	minute := now.Truncate(time.Minute)
	host, hasHost := sampleHost()

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("stats: begin flush: %v", err)
		return
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)

	for k, n := range reqs {
		if err := q.UpsertStatsRequest(ctx, db.UpsertStatsRequestParams{
			Minute: time.Unix(k.minute*60, 0).UTC(), RouteGroup: k.group,
			StatusClass: k.class, Requests: n,
		}); err != nil {
			logging.Errorf("stats: flush requests: %v", err)
			return
		}
	}
	for k, n := range durs {
		if err := q.UpsertStatsDuration(ctx, db.UpsertStatsDurationParams{
			Minute: time.Unix(k.minute*60, 0).UTC(), RouteGroup: k.group,
			Bucket: k.bucket, Hits: n,
		}); err != nil {
			logging.Errorf("stats: flush durations: %v", err)
			return
		}
	}
	for k, n := range events {
		if err := q.UpsertStatsEvent(ctx, db.UpsertStatsEventParams{
			Minute: time.Unix(k.minute*60, 0).UTC(), Kind: k.kind, Count: n,
		}); err != nil {
			logging.Errorf("stats: flush events: %v", err)
			return
		}
	}
	for fam, fc := range families {
		if err := q.UpsertStatsESI(ctx, db.UpsertStatsESIParams{
			Minute: minute, Family: fam, Calls: fc.Calls, Errors: fc.Errors,
		}); err != nil {
			logging.Errorf("stats: flush esi: %v", err)
			return
		}
	}
	if hasHost {
		if err := q.UpsertStatsHost(ctx, db.UpsertStatsHostParams{
			Minute: minute, Load1: host.load1, Load5: host.load5, Load15: host.load15,
			MemUsedBytes: host.memUsed, MemTotalBytes: host.memTotal,
			AppRssBytes: host.appRSS, Goroutines: int64(host.goroutines),
			DiskUsedBytes: host.diskUsed, DiskTotalBytes: host.diskTotal,
		}); err != nil {
			logging.Errorf("stats: flush host: %v", err)
			return
		}
	}

	// The finished cycle itself.
	esiState := ""
	switch {
	case app.esi.HardLimitActive(now):
		esiState = "limited"
	case app.esi.ErrorBudgetLow():
		esiState = "low"
	}
	if err := q.InsertStatsCycle(ctx, db.InsertStatsCycleParams{
		At: t.At, TookMs: t.Took.Milliseconds(), Fetches: int64(t.Fetches),
		Budget: int64(t.Budget), Deferred: int64(t.Deferred),
		Characters: int64(t.Characters), EsiState: esiState,
	}); err != nil {
		logging.Errorf("stats: flush cycle: %v", err)
		return
	}
	for _, p := range t.Phases {
		if err := q.InsertStatsCyclePhase(ctx, db.InsertStatsCyclePhaseParams{
			CycleAt: t.At, Phase: p.Name, TookMs: p.Took.Milliseconds(),
		}); err != nil {
			logging.Errorf("stats: flush cycle phase: %v", err)
			return
		}
	}

	if err := app.statsHousekeep(ctx, q, rec, now); err != nil {
		logging.Errorf("stats: housekeeping: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("stats: commit flush: %v", err)
	}
}

// statsHousekeep runs once an hour inside a flush transaction:
// snapshot the data inventory, roll minute rows older than the
// 7-day raw window into the hourly tables (deleting them in the
// same transaction, so a crash cannot count them twice), and
// prune hourly rows past 90 days.
func (app *Application) statsHousekeep(ctx context.Context, q *db.Queries, rec *statsRecorder, now time.Time) error {
	hour := now.Truncate(time.Hour)
	rec.mu.Lock()
	due := rec.lastHousekeep.Before(hour)
	if due {
		rec.lastHousekeep = hour
	}
	rec.mu.Unlock()
	if !due {
		return nil
	}

	// Inventory: the largest tables from the catalog, plus the
	// database itself under an empty name. Raw SQL: sqlc's schema
	// input has no pg_catalog.
	rows, err := app.db.QueryContext(ctx, `
		SELECT c.relname, c.reltuples::bigint, pg_total_relation_size(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		ORDER BY 3 DESC LIMIT 40`)
	if err != nil {
		return fmt.Errorf("inventory tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var est, size int64
		if err := rows.Scan(&name, &est, &size); err != nil {
			return fmt.Errorf("inventory scan: %w", err)
		}
		if err := q.UpsertStatsInventory(ctx, db.UpsertStatsInventoryParams{
			Hour: hour, Name: name, RowsEst: est, SizeBytes: size,
		}); err != nil {
			return fmt.Errorf("inventory upsert: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventory rows: %w", err)
	}
	rows.Close()
	var dbSize int64
	if err := app.db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&dbSize); err != nil {
		return fmt.Errorf("database size: %w", err)
	}
	if err := q.UpsertStatsInventory(ctx, db.UpsertStatsInventoryParams{
		Hour: hour, Name: "", RowsEst: 0, SizeBytes: dbSize,
	}); err != nil {
		return fmt.Errorf("inventory database: %w", err)
	}

	rollCutoff := now.Add(-7 * 24 * time.Hour).Truncate(time.Hour)
	roll := []func(context.Context, time.Time) error{
		q.RollupStatsRequests, q.RollupStatsDurations, q.RollupStatsESI,
		q.RollupStatsEvents, q.RollupStatsHost, q.RollupStatsCycles, q.RollupStatsCyclePhases,
	}
	for _, fn := range roll {
		if err := fn(ctx, rollCutoff); err != nil {
			return fmt.Errorf("rollup: %w", err)
		}
	}
	del := []func(context.Context, time.Time) error{
		q.DeleteStatsRequestsRolled, q.DeleteStatsDurationsRolled, q.DeleteStatsESIRolled,
		q.DeleteStatsEventsRolled, q.DeleteStatsHostRolled, q.DeleteStatsCyclesRolled,
		q.DeleteStatsCyclePhasesRolled,
	}
	for _, fn := range del {
		if err := fn(ctx, rollCutoff); err != nil {
			return fmt.Errorf("rollup delete: %w", err)
		}
	}
	pruneCutoff := now.Add(-90 * 24 * time.Hour).Truncate(time.Hour)
	prune := []func(context.Context, time.Time) error{
		q.PruneStatsRequestsHourly, q.PruneStatsDurationsHourly, q.PruneStatsESIHourly,
		q.PruneStatsEventsHourly, q.PruneStatsHostHourly, q.PruneStatsCyclesHourly,
		q.PruneStatsCyclePhasesHourly, q.PruneStatsInventory,
	}
	for _, fn := range prune {
		if err := fn(ctx, pruneCutoff); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	return nil
}
