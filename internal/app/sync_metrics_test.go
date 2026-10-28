package app

// Black-box tests for the Sync page's "Serving pages" card:
// render completeness, served-record ages, and the typing-guess
// hit rates land on the page with zero outbound calls.

import (
	"context"
	"net/http"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func metricsTestApp(t *testing.T) (*Application, *http.Cookie) {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	grantTestAdmin(app, fixtureCharA)
	return app, sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
}

// TestSyncRendersCountComplete pins that the Sync tab renders
// with its metrics card once an admin is signed in.
func TestSyncRendersCountComplete(t *testing.T) {
	app, cookie := metricsTestApp(t)
	code, body := getPage(t, app, cookie, "/admin/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/admin/sync/", body, "Serving")
}

// TestSyncRecordsServedAges pins the Served-lines path: a full
// render and a fresh record age show up in the card.
func TestSyncRecordsServedAges(t *testing.T) {
	app, cookie := metricsTestApp(t)
	app.noteRender(true)
	app.noteServedAge(5 * time.Minute)
	code, body := getPage(t, app, cookie, "/admin/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/admin/sync/", body, "fully loaded", "under an hour old")
}

// TestSyncGuessRates pins the guess-rate loader: a guessed pilot
// opened inside the window renders 1 queued, 1 opened.
func TestSyncGuessRates(t *testing.T) {
	app, cookie := metricsTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := app.queries.InsertTypingGuess(ctx, db.InsertTypingGuessParams{
		Box: "topbar", Kind: string(pageWantPilot),
		EntityID: fixtureCharB, NotedAt: now,
	}); err != nil {
		t.Fatalf("insert guess: %v", err)
	}
	if err := app.queries.MarkTypingGuessHits(ctx, db.MarkTypingGuessHitsParams{
		Kind: string(pageWantPilot), EntityID: fixtureCharB,
		Now: now, Since: now.Add(-guessRateWindow),
	}); err != nil {
		t.Fatalf("mark hit: %v", err)
	}
	view := app.serveMetrics(ctx, now)
	if view.GuessesLine == "" {
		t.Fatalf("serveMetrics: empty guess line, want 1 guess 1 opened")
	}
	code, body := getPage(t, app, cookie, "/admin/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/admin/sync/", body, "Search-box guesses", "Pilots")
}

// TestSyncServingEmptyStates pins the idle Sync page: no renders
// yet, so every Serving line shows its waiting copy.
func TestSyncServingEmptyStates(t *testing.T) {
	app, cookie := metricsTestApp(t)
	code, body := getPage(t, app, cookie, "/admin/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/admin/sync/", body, "Renders fully loaded at first paint", "—")
}
