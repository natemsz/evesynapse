package app

// Cost-index refresh tests: the worker mirrors
// GET /industry/systems/ into the table and honors the
// cadence window on the next call.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// costIndexTransport serves the industry systems document once,
// counting calls.
type costIndexTransport struct {
	calls atomic.Int64
	body  string
}

func (s *costIndexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	header := make(http.Header)
	if req.URL.Path == "/industry/systems/" {
		header.Set("Expires", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body: io.NopCloser(strings.NewReader(s.body)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError, Header: header,
		Body: io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

// TestCostIndicesWorkerRefresh: the first call stores every
// system's manufacturing index; the second call sees the fresh
// stamp and fetches nothing.
func TestCostIndicesWorkerRefresh(t *testing.T) {
	transport := &costIndexTransport{body: `[
		{"solar_system_id":30000142,"cost_indices":[
			{"activity":"manufacturing","cost_index":0.101},
			{"activity":"researching_time_efficiency","cost_index":0.02}]},
		{"solar_system_id":30002537,"cost_indices":[
			{"activity":"manufacturing","cost_index":0.064}]}
	]`}
	app, _, _ := buildCorpTestApp(t, transport)
	ctx := context.Background()

	stored, limited := app.refreshCostIndices(ctx)
	if limited || !stored {
		t.Fatalf("first refresh = (stored %v, limited %v), want (true, false)", stored, limited)
	}
	got, err := app.queries.GetIndustryCostIndex(ctx, db.GetIndustryCostIndexParams{
		SolarSystemID: 30000142, Activity: "manufacturing",
	})
	if err != nil || got.CostIndex != 0.101 {
		t.Fatalf("jita manufacturing index = %+v err %v, want 0.101", got, err)
	}
	stamp, err := app.queries.GetIndustryCostIndicesStamp(ctx)
	if err != nil || stamp.RowCount != 3 {
		t.Fatalf("index rows = %+v err %v, want 3", stamp, err)
	}

	// Inside the cadence window: no second fetch.
	stored, limited = app.refreshCostIndices(ctx)
	if limited || stored {
		t.Fatalf("second refresh = (stored %v, limited %v), want (false, false)", stored, limited)
	}
	if n := transport.calls.Load(); n != 1 {
		t.Fatalf("cost index fetches = %d, want exactly 1", n)
	}
}
