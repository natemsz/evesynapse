// Package apptest holds the shared fixture helpers for the
// black-box page tests in internal/app/tests: the counting
// stub transport, fixture IDs, page/assertion helpers, and
// seeders that need nothing beyond exported sqlc queries.
// White-box tests stay in package app beside the code; this
// package exists so the page tests can live in their own
// folder without the app exporting its internals.
package apptest

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"evesynapse/internal/app"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/pgtest"
)

// Fixture IDs shared across the page tests.
const (
	FixtureCharA  = int64(90000001)
	FixtureCharB  = int64(90000002)
	FixtureCorpA  = int64(98000001)
	FixtureCorpB  = int64(98000002)
	FixtureMember = int64(93300001)
)

// CountingTransport fails every request and counts calls;
// handlers must never reach it.
type CountingTransport struct{ Calls atomic.Int64 }

func (s *CountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.Calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"must not be called"}`)),
	}, nil
}

// Build constructs a fixture application (no worker goroutine)
// over a fresh embedded-Postgres test database and registers
// its cleanup.
func Build(t *testing.T, transport http.RoundTripper) *app.TestRig {
	t.Helper()
	rig, err := app.NewTestRig(pgtest.FreshDSN(t), transport)
	if err != nil {
		t.Fatalf("build test rig: %v", err)
	}
	t.Cleanup(rig.Close)
	return rig
}

// SessionCookie mints a signed-in session for the user.
func SessionCookie(t *testing.T, rig *app.TestRig, userID, characterID int64, name string) *http.Cookie {
	t.Helper()
	cookie, err := rig.SessionCookie(userID, characterID, name)
	if err != nil {
		t.Fatalf("session cookie: %v", err)
	}
	return cookie
}

// GetPage serves one GET through the real router.
func GetPage(t *testing.T, rig *app.TestRig, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	rig.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// MustContain asserts each want appears in the body.
func MustContain(t *testing.T, path, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("GET %s: body missing %q", path, want)
		}
	}
}

// RFC formats a timestamp the way ESI and the fixture rows
// carry it (UTC, RFC3339).
func RFC(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// farFuture is when the fixture tokens and snapshots run out: never,
// as far as a test is concerned.
var farFuture = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

// SeedCharacter stores one linked character row.
func SeedCharacter(t *testing.T, q *db.Queries, userID, characterID int64, name string) db.Character {
	t.Helper()
	ch, err := q.UpsertCharacter(context.Background(), db.UpsertCharacterParams{
		CharacterID:  characterID,
		UserID:       userID,
		Name:         name,
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  sql.NullTime{Time: farFuture, Valid: true},
		LinkState:    "ok",
	})
	if err != nil {
		t.Fatalf("seed character %d: %v", characterID, err)
	}
	return ch
}

// SeedSnapshot stores one snapshot payload marked fresh until 2999.
func SeedSnapshot(t *testing.T, q *db.Queries, characterID int64, kind string, payload any) {
	t.Helper()
	var raw string
	switch p := payload.(type) {
	case string:
		raw = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	if err := q.UpsertSnapshot(context.Background(), db.UpsertSnapshotParams{
		CharacterID: characterID,
		Kind:        kind,
		Payload:     raw,
		FetchedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CachedUntil: sql.NullTime{Time: farFuture, Valid: true},
	}); err != nil {
		t.Fatalf("seed snapshot %s: %v", kind, err)
	}
}
