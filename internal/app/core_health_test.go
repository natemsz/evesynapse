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

// bustHealthCache forgets the cached /healthz answer so the
// next call runs the checks again. Production answers stand for
// healthCacheFor; the phases below change state faster than
// that and mean every call to be a fresh check.
func bustHealthCache(app *Application) {
	app.healthMu.Lock()
	app.healthCacheAt = time.Time{}
	app.healthMu.Unlock()
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
	bustHealthCache(app)
	if code, body := healthz(app); code != http.StatusOK {
		t.Fatalf("worker ran 30s ago: status %d, body %q; want 200", code, body)
	}
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = time.Now().Add(-workerStallAfter - time.Minute) })
	bustHealthCache(app)
	code, body := healthz(app)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "worker: stalled") {
		t.Fatalf("worker idle past the limit: status %d, body %q; want 503 naming the worker", code, body)
	}

	// A worker that has never run is measured from startup: fine
	// just after boot, stalled if it still has not run much later.
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = time.Time{} })
	bustHealthCache(app)
	if code, _ := healthz(app); code != http.StatusOK {
		t.Fatalf("just started, no cycle yet: status %d, want 200", code)
	}
	app.startedAt = time.Now().Add(-workerStallAfter - time.Minute)
	bustHealthCache(app)
	if code, body := healthz(app); code != http.StatusServiceUnavailable || !strings.Contains(body, "worker: stalled") {
		t.Fatalf("never ran since a long-ago start: status %d, body %q; want 503", code, body)
	}
	app.startedAt = time.Now()

	// Database gone: unhealthy, and the answer says which part
	// without repeating the connection details the error carries.
	conn.Close()
	bustHealthCache(app)
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

// TestHealthzCachesItsAnswer pins the cache: a health check
// inside the window is answered from the last check, so a flood
// of checks never reaches the database; the first check after
// the window runs the checks again and tells the truth.
func TestHealthzCachesItsAnswer(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})

	if code, body := healthz(app); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthy app: status %d, body %q; want 200 ok", code, body)
	}

	// The database goes away. Inside the cache window the last
	// answer stands — that is the point: no new ping happened,
	// or this call would already report the loss.
	conn.Close()
	if code, body := healthz(app); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("within the cache window: status %d, body %q; want the cached 200 ok", code, body)
	}

	// Past the window the checks run again and the outage is
	// reported.
	bustHealthCache(app)
	if code, body := healthz(app); code != http.StatusServiceUnavailable || !strings.Contains(body, "database: unreachable") {
		t.Fatalf("after the cache window: status %d, body %q; want 503 naming the database", code, body)
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
