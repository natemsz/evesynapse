package app

// ---------------------------------------------------------------------------
// /healthz and the HTTP server's limits.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// workerStallAfter is how long the worker may go without starting
// a cycle before /healthz reports it stalled. A cycle starts every
// minute; this allows for one that runs long on a large instance
// while still catching a worker that has stopped coming round.
const workerStallAfter = 15 * time.Minute

// healthCacheFor is how long a /healthz answer stands before the
// checks run again. The endpoint is unauthenticated and cheap to
// call, so an uncached database ping per request would let
// anyone keep the database busy and fill the log with one error
// per call. A few seconds of cache keeps the answer truthful: a
// database that goes down is reported within the window, not
// hidden by it.
const healthCacheFor = 5 * time.Second

// healthLogEvery bounds the error logging while a problem
// persists: the first failing check logs at once, repeats at
// most once a minute, and a return to health re-arms the log so
// the next failure is never swallowed by the throttle.
const healthLogEvery = time.Minute

// handleHealthz answers a monitor: 200 "ok" when the app can reach its
// database and its background worker is running, 503 with one line per
// problem otherwise. The process being up is not enough: that is true
// of an app that has lost its database and of one whose worker stopped
// an hour ago.
//
// The endpoint is open to anyone, so the answer names what is
// wrong and no more; the actual error goes to the log.
func (app *Application) handleHealthz(w http.ResponseWriter, r *http.Request) {
	problems := app.healthProblems(r.Context())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if len(problems) == 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "unhealthy\n%s\n", strings.Join(problems, "\n"))
}

// healthProblems answers from the cache while the last check is
// fresh (healthCacheFor), and runs the checks again once it is
// not. Only a fresh check can log, and even then at most once a
// minute while the same outage persists.
func (app *Application) healthProblems(ctx context.Context) []string {
	app.healthMu.Lock()
	if !app.healthCacheAt.IsZero() && time.Since(app.healthCacheAt) < healthCacheFor {
		problems := app.healthCacheResult
		app.healthMu.Unlock()
		return problems
	}
	app.healthMu.Unlock()

	problems, logLines := app.checkHealth(ctx)

	app.healthMu.Lock()
	app.healthCacheAt = time.Now()
	app.healthCacheResult = problems
	logNow := false
	if len(problems) == 0 {
		app.healthLogAt = time.Time{}
	} else if time.Since(app.healthLogAt) >= healthLogEvery {
		app.healthLogAt = time.Now()
		logNow = true
	}
	app.healthMu.Unlock()

	if logNow {
		for _, line := range logLines {
			logging.Errorf("%s", line)
		}
	}
	return problems
}

// checkHealth runs the health checks once: the database ping and,
// for an application that runs a worker, the worker's freshness.
// It returns the problems to report and the log lines for them;
// the caller decides whether the lines are due to be logged.
func (app *Application) checkHealth(ctx context.Context) (problems, logLines []string) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := app.db.PingContext(pingCtx); err != nil {
		logLines = append(logLines, fmt.Sprintf("healthz: database: %v", err))
		problems = append(problems, "database: unreachable")
	}

	// Only an application that runs a worker can have a stalled one
	// (the test fixtures build it without).
	if app.workerDone != nil {
		last := app.snapshotWorkerStatus().LastRunAt
		if last.IsZero() {
			last = app.startedAt
		}
		if idle := time.Since(last); idle > workerStallAfter {
			logLines = append(logLines, fmt.Sprintf("healthz: worker: no cycle has started for %s", idle.Round(time.Second)))
			problems = append(problems, "worker: stalled")
		}
	}
	return problems, logLines
}

// NewHTTPServer builds the server both entrypoints run. Every
// phase of a request is bounded, so a client that connects and
// then stalls — sending its request a byte at a time, or never
// reading the answer — cannot hold a connection open indefinitely.
// The write limit leaves room for the slowest real responses:
// handlers that make live calls to EVE, each capped at ten
// seconds.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}
