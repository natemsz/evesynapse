package app

import (
	"bytes"
	"log"
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
	if code, _ := healthz(app); code != 503 {
		t.Fatalf("database closed: status %d", code)
	}
	for _, want := range []string{" ERROR GET /healthz -> 503 ", " ERROR healthz: database: "} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q is missing %q", logs.String(), want)
		}
	}
}
