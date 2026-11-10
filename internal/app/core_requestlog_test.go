package app

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRequestLogLevels: a request is routine (info) unless the
// server itself failed it (5xx), which is an error — so a log
// filtered to errors still shows the requests that went wrong.
func TestRequestLogLevels(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if code, _ := healthz(app); code != 200 {
		t.Fatalf("healthy app: status %d", code)
	}
	if !strings.Contains(logs.String(), " INFO GET /healthz -> 200 ") {
		t.Errorf("log %q does not record the 200 at info", logs.String())
	}

	logs.Reset()
	// /healthz serves its last check from a short cache, so the
	// cached answer is dropped here: the second call must run
	// the checks again, this time against the closed database.
	app.healthMu.Lock()
	app.healthCacheAt = time.Time{}
	app.healthCacheResult = nil
	app.healthMu.Unlock()
	conn.Close()
	app.pool.Close()
	if code, _ := healthz(app); code != 503 {
		t.Fatalf("database closed: status %d", code)
	}
	for _, want := range []string{" ERROR GET /healthz -> 503 ", " ERROR healthz: database: "} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q is missing %q", logs.String(), want)
		}
	}
}

// TestRequestLogReferrer: an external referrer on an entry page is
// logged as its origin, so a new player's discovery channel can be
// read from the log. Same-origin referrers are navigation inside
// the app, and other pages are not entry points, so neither logs
// a referrer.
func TestRequestLogReferrer(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	get := func(path, referer string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "evesynapse.app"
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
	}

	get("/", "https://www.google.com/search?q=evesynapse")
	if !strings.Contains(logs.String(), "referrer https://www.google.com") {
		t.Errorf("log %q does not record the external referrer", logs.String())
	}
	if strings.Contains(logs.String(), "search?q=") {
		t.Errorf("log %q leaks the referrer query string", logs.String())
	}

	logs.Reset()
	get("/", "https://evesynapse.app/market/")
	if strings.Contains(logs.String(), "referrer") {
		t.Errorf("log %q records a same-origin referrer", logs.String())
	}

	logs.Reset()
	get("/healthz", "https://www.google.com/")
	if strings.Contains(logs.String(), "referrer") {
		t.Errorf("log %q records a referrer on a non-entry page", logs.String())
	}
}
