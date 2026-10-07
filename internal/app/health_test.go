package app

// Tests for /healthz: it reports the database and the background
// worker, not merely that the process answers.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func healthz(app *Application) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestHealthzReportsDatabaseAndWorker(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})

	// Database reachable, no worker to speak of: healthy.
	if code, body := healthz(app); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthy app: status %d, body %q; want 200 ok", code, body)
	}

	// A worker that came round recently is healthy; one that has
	// not started a cycle for too long is not.
	app.workerDone = make(chan struct{})
	app.startedAt = time.Now()
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = time.Now().Add(-30 * time.Second) })
	if code, body := healthz(app); code != http.StatusOK {
		t.Fatalf("worker ran 30s ago: status %d, body %q; want 200", code, body)
	}
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = time.Now().Add(-workerStallAfter - time.Minute) })
	code, body := healthz(app)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "worker: stalled") {
		t.Fatalf("worker idle past the limit: status %d, body %q; want 503 naming the worker", code, body)
	}

	// A worker that has never run is measured from startup: fine
	// just after boot, stalled if it still has not run much later.
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = time.Time{} })
	if code, _ := healthz(app); code != http.StatusOK {
		t.Fatalf("just started, no cycle yet: status %d, want 200", code)
	}
	app.startedAt = time.Now().Add(-workerStallAfter - time.Minute)
	if code, body := healthz(app); code != http.StatusServiceUnavailable || !strings.Contains(body, "worker: stalled") {
		t.Fatalf("never ran since a long-ago start: status %d, body %q; want 503", code, body)
	}
	app.startedAt = time.Now()

	// Database gone: unhealthy, and the answer says which part
	// without repeating the connection details the error carries.
	conn.Close()
	code, body = healthz(app)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "database: unreachable") {
		t.Fatalf("database closed: status %d, body %q; want 503 naming the database", code, body)
	}
	for _, leak := range []string{"postgres", "127.0.0.1", "password", "sql:"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("health answer %q leaks %q", body, leak)
		}
	}
}

func TestHTTPServerBoundsEveryPhase(t *testing.T) {
	srv := NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	for name, d := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if d <= 0 {
			t.Errorf("%s is unset: that phase of a request could hang forever", name)
		}
	}
}
