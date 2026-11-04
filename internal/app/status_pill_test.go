package app

// Status pill tests: the Overview pill's combined ESI and
// our-side state, with blame attributed by precedence (ESI down,
// then ours-down, then ours-lagging, then ESI limited, then
// healthy).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedPillSnapshot(t *testing.T, q *db.Queries, fetchedAt time.Time) {
	t.Helper()
	if err := q.UpsertGlobalSnapshot(context.Background(), db.UpsertGlobalSnapshotParams{
		Kind:      esi.GlobalStatus,
		Payload:   `{"players": 23456, "server_version": "1132976", "start_time": "2026-10-01T11:00:00Z", "vip": false}`,
		FetchedAt: fetchedAt,
	}); err != nil {
		t.Fatalf("seed status snapshot: %v", err)
	}
}

func seedPillMarket(t *testing.T, q *db.Queries, attemptedAt time.Time) {
	t.Helper()
	for _, region := range sweptRegions {
		if err := q.UpsertMarketFetchState(context.Background(), db.UpsertMarketFetchStateParams{
			Kind: regionSweepKind(region.ID), State: fetchStateOK, AttemptedAt: attemptedAt,
		}); err != nil {
			t.Fatalf("seed market state %d: %v", region.ID, err)
		}
	}
}

func seedPillWorker(app *Application, lastRun time.Time) {
	app.updateWorkerStatus(func(s *workerStatus) { s.LastRunAt = lastRun })
}

// (a) ESI down plus market down shows ESI down: our lag is ESI's
// fault then, and must not be blamed on us.
func TestPillESIDownBeatsMarketDown(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now.Add(-20*time.Minute))
	seedPillMarket(t, q, now.Add(-7*time.Hour))
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "down" || view.Label != "ESI down" {
		t.Fatalf("pill = %q/%q, want down/ESI down", view.State, view.Label)
	}
	if view.Players != "" {
		t.Fatalf("players = %q, want none while ESI is down", view.Players)
	}
}

// (b) ESI healthy plus market lagging shows ours-lagging with
// the Sync behind label, the player count, and a hover naming how
// old the market orders are.
func TestPillMarketLaggingShowsSyncBehind(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now.Add(-5*time.Hour-12*time.Minute))
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "ours-lagging" || view.Label != "Sync behind" {
		t.Fatalf("pill = %q/%q, want ours-lagging/Sync behind", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456 (ESI is answering)", view.Players)
	}
	if !strings.Contains(view.Title, "Market orders are 5h12m old") {
		t.Fatalf("title = %q, want it to name the market age", view.Title)
	}
	if !strings.Contains(view.Title, "ESI itself is answering normally") {
		t.Fatalf("title = %q, want it to clear ESI", view.Title)
	}
}

// (c) ESI healthy plus a stalled worker shows ours-down.
func TestPillWorkerStalledShowsSyncStalled(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now)
	seedPillWorker(app, now.Add(-20*time.Minute))

	view := app.esiHealth(ctx)
	if view.State != "ours-down" || view.Label != "Sync stalled" {
		t.Fatalf("pill = %q/%q, want ours-down/Sync stalled", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456 (ESI is answering)", view.Players)
	}
	if !strings.Contains(view.Title, "worker has not started a cycle") {
		t.Fatalf("title = %q, want it to name the worker", view.Title)
	}
}

// (d) ESI limited plus market lagging shows ours-lagging, and
// the hover names both conditions.
func TestPillLimitedPlusLaggingNamesBoth(t *testing.T) {
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":             []string{"application/json"},
				"X-Esi-Error-Limit-Remain": []string{"0"},
				"X-Esi-Error-Limit-Reset":  []string{"60"},
			},
			Body: io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now.Add(-2*time.Hour))
	seedPillWorker(app, now)
	if _, _, err := app.esi.FetchRaw(ctx, "", "/status/"); err != nil {
		t.Fatalf("prime error budget: %v", err)
	}

	view := app.esiHealth(ctx)
	if view.State != "ours-lagging" || view.Label != "Sync behind" {
		t.Fatalf("pill = %q/%q, want ours-lagging/Sync behind", view.State, view.Label)
	}
	if !strings.Contains(view.Title, "Market orders are") || !strings.Contains(view.Title, "close to its error limit") {
		t.Fatalf("title = %q, want it to name both the market lag and the error limit", view.Title)
	}
}

// (e) Everything healthy shows the healthy pill, player count
// included, unchanged.
func TestPillHealthyUnchanged(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	seedPillSnapshot(t, q, now)
	seedPillMarket(t, q, now)
	seedPillWorker(app, now)

	view := app.esiHealth(ctx)
	if view.State != "ok" || view.Label != "ESI healthy" {
		t.Fatalf("pill = %q/%q, want ok/ESI healthy", view.State, view.Label)
	}
	if view.Players != "23,456" {
		t.Fatalf("players = %q, want 23,456", view.Players)
	}
	if view.Title != "ESI is answering normally." {
		t.Fatalf("title = %q, want the healthy text unchanged", view.Title)
	}
}
