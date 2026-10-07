package app

// Tests for the refresh cycle's stop rules (worker.go): what a cycle
// does once ESI says to back off, and what it does with a character
// whose token cannot be used. They drive a whole cycle against an
// ESI that answers every request with its error-limit status.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// errorLimitESI answers every request with 420, ESI's "you have
// used up your error budget" status, and remembers what was asked.
type errorLimitESI struct {
	mu    sync.Mutex
	paths []string
}

func (e *errorLimitESI) RoundTrip(req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.paths = append(e.paths, req.URL.Path)
	e.mu.Unlock()
	return &http.Response{
		StatusCode: 420,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"error limited"}`)),
	}, nil
}

// askedAbout reports whether any request was for the character.
func (e *errorLimitESI) askedAbout(characterID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	needle := fmt.Sprintf("/characters/%d/", characterID)
	for _, p := range e.paths {
		if strings.Contains(p, needle) {
			return true
		}
	}
	return false
}

// TestRefreshCycleStopsAtTheErrorLimit: once ESI says to back off,
// the cycle finishes the character it is on and fetches nothing
// more: not the next character, not the public passes.
func TestRefreshCycleStopsAtTheErrorLimit(t *testing.T) {
	esiStub := &errorLimitESI{}
	app, _, q := buildCorpTestApp(t, esiStub)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Beta")

	app.refreshCycle(ctx)

	if !esiStub.askedAbout(fixtureCharA) {
		t.Error("the first character was never attempted")
	}
	if esiStub.askedAbout(fixtureCharB) {
		t.Errorf("the cycle went on to the next character after the error limit: %v", esiStub.paths)
	}
	// One failed fetch (the first character's first dataset), and
	// nothing stored.
	if got, want := app.snapshotWorkerStatus().Summary, "1 failure(s), ESI error limit — backing off"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if app.snapshotWorkerStatus().Warming {
		t.Error("the cycle ended but the status still says it is running")
	}
}

// TestRefreshCycleSkipsACharacterWithNoUsableToken: a character
// whose token cannot be used counts as a failure and is passed
// over, and the cycle moves on to the next character. That holds
// even when ESI has already said to back off: the back-off stops
// the cycle after a character it actually worked on, not after one
// it skipped.
func TestRefreshCycleSkipsACharacterWithNoUsableToken(t *testing.T) {
	esiStub := &errorLimitESI{}
	app, _, q := buildCorpTestApp(t, esiStub)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Alpha sorts first. Its access token ran out long ago and it
	// has no refresh token to get another with.
	if _, err := q.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID: fixtureCharA,
		UserID:      user.ID,
		Name:        "Alpha",
		AccessToken: "fixture",
		TokenExpiry: mustNullTime("2000-01-01T00:00:00Z"),
		LinkState:   "ok",
	}); err != nil {
		t.Fatalf("seed the character with no usable token: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharB, "Beta")

	app.refreshCycle(ctx)

	if esiStub.askedAbout(fixtureCharA) {
		t.Errorf("fetched for a character with no usable token: %v", esiStub.paths)
	}
	// Two failures: Alpha's token, then Beta's first fetch, which
	// is only attempted if the cycle moved on past Alpha.
	if got, want := app.snapshotWorkerStatus().Summary, "2 failure(s), ESI error limit — backing off"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
