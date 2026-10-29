package app

// ---------------------------------------------------------------------------
// /status.json: the public service summary the status page reads
// (status/ at the repo root, hosted apart from this server).
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// statusPath is where the summary is served.
const statusPath = "/status.json"

// statusCacheFor is how long one built summary stands. Anyone may
// ask, so the reads behind it run at most twice a minute however
// many ask.
const statusCacheFor = 30 * time.Second

// The summary's own throttle per client address: the status page
// asks once a minute, so a burst of 10 and then one every 6
// seconds leaves room for a few open tabs behind one address.
const (
	statusPerMinute = 10
	statusBurst     = 10
)

// The states a service can be in. Coarse on purpose: the summary
// is public, so it says how a part is doing and never why.
const (
	statusOK       = "ok"
	statusDegraded = "degraded"
	statusDown     = "down"
)

// How stale the region sweeps may get. A region is swept again
// every regionSweepGate and a sweep takes a few minutes, so the
// stalest region passing an hour and a half means the sweeps are
// lagging, and six hours means they have stopped.
const (
	marketLagAfter  = 90 * time.Minute
	marketDownAfter = 6 * time.Hour
)

// workerLagAfter is how long the worker may go without starting a
// cycle before the summary calls it degraded; workerStallAfter
// calls it down.
const workerLagAfter = 5 * time.Minute

// esiErrorShare is the share of ESI calls over the last ten
// minutes that may fail before the summary calls ESI degraded,
// once there are enough calls (esiErrorMinCalls) to judge by.
const (
	esiErrorShare    = 0.25
	esiErrorMinCalls = 20
)

// statusService is one line of the summary: what the part is, how
// it is doing, and how many seconds ago it last showed it was
// working (nil when it never has, or that is not known).
type statusService struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Age   *int64 `json:"age"`
}

// statusSummary is the whole answer.
type statusSummary struct {
	State     string          `json:"state"` // the worst of the services
	CheckedAt time.Time       `json:"checked_at"`
	Services  []statusService `json:"services"`
}

// statusCache holds the last built answer. buildMu lets one
// request build while the others wait for its result instead of
// each running the reads.
type statusCache struct {
	buildMu  sync.Mutex
	mu       sync.Mutex
	at       time.Time
	body     []byte
	throttle *anonThrottle
	once     sync.Once
}

// statusThrottle is the summary's per-address throttle, made on
// first use (the test rig builds an Application without New).
func (app *Application) statusThrottle() *anonThrottle {
	app.status.once.Do(func() {
		app.status.throttle = newAnonThrottle(statusPerMinute, statusBurst)
	})
	return app.status.throttle
}

// handleStatus serves the summary. It is open to anyone, without a
// session, and reads only what the server already stores or holds;
// the one browser origin allowed to read it is the status page's.
func (app *Application) handleStatus(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if origin := app.cfg.statusOrigin; origin != "" {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
	}
	if ok, wait := app.statusThrottle().allow(clientAddr(r)); !ok {
		h.Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		http.Error(w, "Too many requests.", http.StatusTooManyRequests)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=30")
	_, _ = w.Write(app.statusBody(r.Context()))
}

// statusBody returns the cached answer while it is fresh and
// builds a new one once it is not.
func (app *Application) statusBody(ctx context.Context) []byte {
	c := &app.status
	if body := c.fresh(); body != nil {
		return body
	}
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	if body := c.fresh(); body != nil {
		return body // built while this request waited
	}
	// The reads run apart from the asker: one client hanging up
	// must not leave everyone else an answer built from errors.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	body, err := json.Marshal(app.buildStatus(buildCtx, time.Now()))
	if err != nil {
		logging.Errorf("status: encode: %v", err)
		body = []byte(`{"state":"down","services":[]}`)
	}
	c.mu.Lock()
	c.at, c.body = time.Now(), body
	c.mu.Unlock()
	return body
}

func (c *statusCache) fresh() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Since(c.at) < statusCacheFor {
		return c.body
	}
	return nil
}

// buildStatus reads each part's state. Every read is bounded: one
// ping, one row per swept region, two small aggregates.
func (app *Application) buildStatus(ctx context.Context, now time.Time) statusSummary {
	s := statusSummary{CheckedAt: now.UTC().Truncate(time.Second)}
	add := func(id, state string, since time.Time) {
		svc := statusService{ID: id, State: state}
		if !since.IsZero() {
			age := int64(now.Sub(since) / time.Second)
			if age < 0 {
				age = 0
			}
			svc.Age = &age
		}
		s.Services = append(s.Services, svc)
	}

	dbUp := app.statusDatabase(ctx)
	if dbUp {
		add("database", statusOK, now)
	} else {
		add("database", statusDown, time.Time{})
	}

	state, since := app.statusWorker(now)
	add("worker", state, since)
	state, since = app.statusNotifications(now)
	add("notifications", state, since)
	state, since = app.statusESI(ctx, now, dbUp)
	add("esi", state, since)
	state, since = app.statusMarket(ctx, now, dbUp)
	add("market", state, since)
	if app.push != nil {
		state, since = app.statusPush(ctx, dbUp)
		add("push", state, since)
	}

	s.State = statusOK
	for _, svc := range s.Services {
		s.State = worseStatus(s.State, svc.State)
	}
	return s
}

func worseStatus(a, b string) string {
	rank := map[string]int{statusOK: 0, statusDegraded: 1, statusDown: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// statusDatabase pings the database. The error goes nowhere: the
// /healthz check logs it.
func (app *Application) statusDatabase(ctx context.Context) bool {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return app.db.PingContext(pingCtx) == nil
}

// statusWorker is the worker's last cycle start, under the same
// stall limit /healthz uses. A worker that has not run yet is
// measured from startup, without an age of its own.
func (app *Application) statusWorker(now time.Time) (string, time.Time) {
	last := app.snapshotWorkerStatus().LastRunAt
	from := last
	if from.IsZero() {
		from = app.startedAt
	}
	switch idle := now.Sub(from); {
	case idle > workerStallAfter:
		return statusDown, last
	case idle > workerLagAfter:
		return statusDegraded, last
	}
	return statusOK, last
}

// statusNotifications is the notifications pass: the last finished
// worker cycle, and whether that cycle reached the pass (it runs
// last, so a cycle that stopped early did not).
func (app *Application) statusNotifications(now time.Time) (string, time.Time) {
	t := app.snapshotWorkerStatus().Timing
	if t.At.IsZero() {
		if now.Sub(app.startedAt) > workerStallAfter {
			return statusDown, time.Time{}
		}
		return statusOK, time.Time{}
	}
	if now.Sub(t.At) > workerStallAfter {
		return statusDown, t.At
	}
	for _, p := range t.Phases {
		if p.Name == "notifications" {
			if now.Sub(t.At) > workerLagAfter {
				return statusDegraded, t.At
			}
			return statusOK, t.At
		}
	}
	return statusDegraded, t.At
}

// statusESI is ESI as the Overview bar's pill reads it (esiHealth):
// the stored Tranquility status's age and the error budget, plus
// the share of calls that failed over the last ten minutes.
func (app *Application) statusESI(ctx context.Context, now time.Time, dbUp bool) (string, time.Time) {
	if !dbUp {
		return statusDown, time.Time{}
	}
	snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalStatus)
	if err != nil {
		return statusDegraded, time.Time{} // the first status check has not landed
	}
	if now.Sub(snap.FetchedAt) > esiDownAfter {
		return statusDown, snap.FetchedAt
	}
	if app.esi != nil && app.esi.ErrorBudgetLow() {
		return statusDegraded, snap.FetchedAt
	}
	rows, err := app.queries.StatsESIWindowMinutes(ctx, db.StatsESIWindowMinutesParams{
		FromTs: now.Add(-10 * time.Minute), ToTs: now, Lim: 100,
	})
	if err == nil {
		var calls, errs int64
		for _, row := range rows {
			calls += row.Calls
			errs += row.Errors
		}
		if calls >= esiErrorMinCalls && float64(errs) > esiErrorShare*float64(calls) {
			return statusDegraded, snap.FetchedAt
		}
	}
	return statusOK, snap.FetchedAt
}

// statusMarket is the region sweeps' freshness: the stalest swept
// region's last finished sweep (its market_fetch_state row, which
// the sweep marks ok when it stores the region's stats). A region
// never swept counts as lagging.
func (app *Application) statusMarket(ctx context.Context, now time.Time, dbUp bool) (string, time.Time) {
	if !dbUp {
		return statusDown, time.Time{}
	}
	kinds := make([]string, 0, len(sweptRegions))
	for _, region := range sweptRegions {
		kinds = append(kinds, regionSweepKind(region.ID))
	}
	rows, err := app.queries.ListMarketFetchStatesByKinds(ctx, kinds)
	if err != nil {
		return statusDown, time.Time{}
	}
	var stalest time.Time
	done := 0
	for _, row := range rows {
		if row.State != fetchStateOK {
			continue // a sweep under way or retrying: its last success is unknown here
		}
		done++
		if stalest.IsZero() || row.AttemptedAt.Before(stalest) {
			stalest = row.AttemptedAt
		}
	}
	if stalest.IsZero() {
		return statusDegraded, time.Time{}
	}
	switch age := now.Sub(stalest); {
	case age > marketDownAfter:
		return statusDown, stalest
	case age > marketLagAfter || done < len(kinds):
		return statusDegraded, stalest
	}
	return statusOK, stalest
}

// statusPush is browser push delivery: degraded once more than
// half the subscribed browsers refused their last send. Push
// services are outside this server, so they never make it down.
// No subscriptions at all is fine: nothing is waiting to be sent.
func (app *Application) statusPush(ctx context.Context, dbUp bool) (string, time.Time) {
	if !dbUp {
		return statusDown, time.Time{}
	}
	sum, err := app.queries.PushDeliverySummary(ctx)
	if err != nil {
		return statusDegraded, time.Time{}
	}
	var since time.Time
	if sum.LastOkAt.After(time.Unix(0, 0)) {
		since = sum.LastOkAt
	}
	if sum.Total > 0 && sum.Failing*2 > sum.Total {
		return statusDegraded, since
	}
	return statusOK, since
}

// parseStatusOrigin reads STATUS_PAGE_ORIGIN: the status page's
// address as scheme and host only ("https://status.example.com").
// Anything else is dropped with a warning, and no browser origin
// may then read the summary.
func parseStatusOrigin(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.User != nil {
		logging.Warnf("evesynapse: STATUS_PAGE_ORIGIN=%q is not an origin like https://status.example.com; ignoring it", raw)
		return ""
	}
	return u.Scheme + "://" + u.Host
}
