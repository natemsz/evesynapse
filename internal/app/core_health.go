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

// handleHealthz answers a monitor: 200 "ok" when the app can reach
// its database and its background worker is running, 503 with one
// line per problem otherwise. It used to answer "ok" whenever the
// process was up, which is true of an app that has lost its
// database and of one whose worker stopped an hour ago.
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

func (app *Application) healthProblems(ctx context.Context) []string {
	var problems []string

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := app.db.PingContext(pingCtx); err != nil {
		logging.Errorf("healthz: database: %v", err)
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
			logging.Errorf("healthz: worker: no cycle has started for %s", idle.Round(time.Second))
			problems = append(problems, "worker: stalled")
		}
	}
	return problems
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
