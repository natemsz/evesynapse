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
)

const (
	FixtureCharA  = int64(90000001)
	FixtureCharB  = int64(90000002)
	FixtureCorpA  = int64(98000001)
	FixtureCorpB  = int64(98000002)
	FixtureMember = int64(93300001)
)

// CountingTransport fails every request and counts calls; handlers
// must never reach it.
type CountingTransport struct {
	Calls atomic.Int64
}

func (s *CountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.Calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"must not be called"}`)),
	}, nil
}

// BuildCorpTestApp builds an Application over a fresh embedded-Postgres test database.
func BuildCorpTestApp(t *testing.T, transport http.RoundTripper) (*app.Application, *sql.DB, *db.Queries) {
	return app.NewCorpTestApp(t, transport)
}

// SeedCharacter inserts a character record in the test database.
func SeedCharacter(t *testing.T, q *db.Queries, userID, characterID int64, name string) db.Character {
	t.Helper()
	ch, err := q.UpsertCharacter(context.Background(), db.UpsertCharacterParams{
		CharacterID:  characterID,
		UserID:       userID,
		Name:         name,
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  sql.NullTime{Time: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
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
		CachedUntil: sql.NullTime{Time: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	}); err != nil {
		t.Fatalf("seed snapshot %s: %v", kind, err)
	}
}

// SessionCookie mints a signed-in session for the given user and character.
func SessionCookie(t *testing.T, a *app.Application, userID, characterID int64, name string) *http.Cookie {
	return a.SessionCookie(t, userID, characterID, name)
}

// GetPage executes a GET request against the HTTP handler with an optional session cookie.
func GetPage(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// MustContain asserts that body contains every required substring.
func MustContain(t *testing.T, path, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("GET %s: body missing %q", path, want)
		}
	}
}
