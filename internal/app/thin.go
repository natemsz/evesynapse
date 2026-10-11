package app

// ---------------------------------------------------------------------------
// Thin client: the same binary as a headless public-data daemon.
// THIN_CLIENT=1 serves only the swarm machine API and the health
// check, and the worker runs only the public passes (guide
// prices, market, structure/planet/pilot names, record drains,
// swarm, name warming). No logins, no sessions, no character or
// corporation passes, no notifications, no Discord: every ESI
// call it makes already runs without a token (accessToken ""
// sends no Authorization header), so the box holds no credential
// worth stealing — no TOKEN_ENCRYPTION_KEY, no owner hashes.
//
// It exists for one reason: separate ESI headroom on a separate
// machine, feeding the shared cache. Thin mode refuses to start
// with sharing off.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"evesynapse/internal/logging"
)

// checkThinConfig rejects a thin client that would share
// nothing: without the swarm it is a daemon with no purpose.
func (c Config) checkThinConfig() error {
	if c.thin && !c.swarm.enabled() {
		return fmt.Errorf("THIN_CLIENT=1 needs sharing on: set SHARED_CACHE=open (or allowlist)")
	}
	return nil
}

// thinHandler is the whole thin HTTP surface: the health check
// and the swarm machine API (swarm_api.go), bearer-gated by the
// shared secret and dark without it. No sessions, no pages, no
// auth routes — instances are not browsers, and there is nobody
// here to log in.
func (app *Application) thinHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Get("/healthz", app.handleHealthz)
	r.Route("/swarm", func(r chi.Router) {
		r.Get("/members", app.handleSwarmMembers)
		r.Post("/pull", app.handleSwarmPull)
	})
	return r
}

// refreshThinCycle is one worker cycle for a thin client: the
// public passes only, with no characters anywhere. Guide prices
// first (asset valuation downstream never waits), then the
// public-data cluster, the swarm, and name warming. Compare
// refreshCycle, which additionally runs the character lanes,
// ops, foresight, pulse, notifications, and Discord.
func (app *Application) refreshThinCycle(ctx context.Context) {
	clock := newPhaseClock(time.Now())
	app.updateWorkerStatus(func(s *workerStatus) {
		s.LastRunAt = clock.start
		s.Warming = true
	})
	timing := workerTiming{At: clock.start, Budget: app.fetchesPerCycle()}
	c := &cycleState{app: app, allowance: &fetchBudget{left: app.fetchesPerCycle()}, halt: newHalt()}

	c.refreshGuidePricesPass(ctx)
	c.refreshPublicData(ctx, nil, true)
	clock.mark("public data", time.Now())

	c.pass("sharing with the swarm", func() (int, bool) {
		return app.swarmCycle(ctx, c.allowance, nil, time.Now())
	})
	clock.mark("swarm", time.Now())

	c.warmNames(ctx, nil)
	clock.mark("names", time.Now())

	app.finishRefreshCycle(c, clock, timing, 0)
}

// refreshGuidePricesPass runs the market-guide mirror pass
// shared by both cycle flavors: one public call into the stored
// table on ESI's cache window.
func (c *cycleState) refreshGuidePricesPass(ctx context.Context) {
	app := c.app
	if stored, gLimited := app.refreshGuidePrices(ctx); stored {
		c.refreshed++
	} else if gLimited {
		logging.Warnf("worker: ESI error limit hit refreshing guide prices; backing off until next cycle")
		c.limited = true
	}
}

// finishRefreshCycle records timing, status, and the summary
// line for either cycle flavor.
func (app *Application) finishRefreshCycle(c *cycleState, clock *phaseClock, timing workerTiming, parked int) {
	timing.Took, timing.Phases = time.Since(clock.start), clock.phases
	timing.Fetches, timing.Deferred = c.allowance.used(app.fetchesPerCycle()), c.deferred
	timing.RateHeld = c.rateHeld
	timing.Budgets = app.esi.TakeRateBudgets()
	app.recordWorkerTiming(timing)

	summary := cycleSummary(c.refreshed, c.namesResolved, c.failed, c.limited, parked, c.deferred)
	app.updateWorkerStatus(func(s *workerStatus) {
		s.Warming = false
		s.Summary = summary
		s.NamesResolvedTotal += c.namesResolved
	})

	if c.refreshed > 0 || c.failed > 0 || c.namesResolved > 0 {
		logging.Infof("worker: cycle done: %s", summary)
	}
}
