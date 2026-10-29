package app

// Tests for /healthz: it reports the database and the background
// worker, not merely that the process answers.

import (
	"encoding/json"
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

// The public service summary is open without a session and says,
// for each service, a state and an age and nothing else: no error
// text, hosts, versions or counts, healthy or not.
func TestStatusSummaryIsPublicAndCarriesOnlyStateAndAge(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	app.cfg.statusOrigin = "https://status.example.com"

	check := func(phase string) map[string]any {
		t.Helper()
		app.status.mu.Lock()
		app.status.body = nil
		app.status.mu.Unlock()
		req := httptest.NewRequest(http.MethodGet, statusPath, nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d without a session, want 200", phase, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://status.example.com" {
			t.Errorf("%s: Access-Control-Allow-Origin %q, want the status page's origin only", phase, got)
		}
		if rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: the summary set a cookie", phase)
		}
		var top map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
			t.Fatalf("%s: body %q: %v", phase, rec.Body.String(), err)
		}
		states := map[any]bool{statusOK: true, statusDegraded: true, statusDown: true}
		for k := range top {
			if k != "state" && k != "checked_at" && k != "services" {
				t.Errorf("%s: unexpected top-level field %q", phase, k)
			}
		}
		if !states[top["state"]] {
			t.Errorf("%s: overall state %v", phase, top["state"])
		}
		services, _ := top["services"].([]any)
		if len(services) == 0 {
			t.Fatalf("%s: no services in %s", phase, rec.Body.String())
		}
		for _, raw := range services {
			svc, _ := raw.(map[string]any)
			for k, v := range svc {
				switch k {
				case "id":
					if _, ok := v.(string); !ok {
						t.Errorf("%s: id %v", phase, v)
					}
				case "state":
					if !states[v] {
						t.Errorf("%s: state %v", phase, v)
					}
				case "age":
					if _, ok := v.(float64); v != nil && !ok {
						t.Errorf("%s: age %v", phase, v)
					}
				default:
					t.Errorf("%s: service field %q leaks beyond state and age", phase, k)
				}
			}
		}
		return top
	}

	check("healthy")
	conn.Close()
	if top := check("database closed"); top["state"] != statusDown {
		t.Errorf("database closed: overall state %v, want down", top["state"])
	}
}
