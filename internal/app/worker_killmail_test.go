package app

// Hermetic tests for the worker's killmail detail warming: a
// stubbed HTTP transport stands in for ESI, so the per-cycle
// bound and the error-limit short-circuit are verified without
// network or real killmails.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
)

// stubTransport answers every request with the configured status
// and a minimal killmail-shaped body, counting calls.
type stubTransport struct {
	status int
	calls  atomic.Int64
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	body := `{"killmail_id":1,"killmail_time":"2026-01-01T00:00:00Z","solar_system_id":30000142,"victim":{"character_id":2,"ship_type_id":587},"attackers":[]}`
	if s.status == http.StatusTooManyRequests {
		body = `{"error":"error limit"}`
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// seedKillmailCharacter builds an Application backed by a fresh
// test database holding one character whose recent-killmails
// snapshot lists n refs with no stored details.
func seedKillmailCharacter(t *testing.T, transport http.RoundTripper, n int) (*Application, db.Character) {
	t.Helper()
	conn, pool, err := openDB(context.Background(), pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { conn.Close(); pool.Close() })
	queries := db.New(conn)

	ctx := context.Background()
	user, err := queries.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch, err := queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID:  999000001,
		UserID:       user.ID,
		Name:         "Fixture Pilot",
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  sql.NullString{String: "2999-01-01T00:00:00Z", Valid: true},
		LinkState:    "ok",
	})
	if err != nil {
		t.Fatalf("upsert character: %v", err)
	}

	refs := make([]esi.KillmailRef, 0, n)
	for i := 1; i <= n; i++ {
		refs = append(refs, esi.KillmailRef{KillmailID: int64(i), KillmailHash: fmt.Sprintf("hash%d", i)})
	}
	payload, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: ch.CharacterID,
		Kind:        esi.SnapKillmails,
		Payload:     string(payload),
		FetchedAt:   "2026-01-01T00:00:00Z",
		CachedUntil: sql.NullString{String: "2999-01-01T00:00:00Z", Valid: true},
	}); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}

	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })
	return &Application{queries: queries, esi: client}, ch
}

func TestWarmKillmailDetailsBound(t *testing.T) {
	transport := &stubTransport{status: http.StatusOK}
	app, ch := seedKillmailCharacter(t, transport, 15)
	ctx := context.Background()

	// First pass: the per-cycle bound stops at 10 despite 15 refs.
	fetched, limited := app.warmKillmailDetails(ctx, ch)
	if limited {
		t.Fatal("first pass: unexpected error-limit stop")
	}
	if fetched != maxKillmailDetailsPerCycle {
		t.Fatalf("first pass: fetched %d, want %d", fetched, maxKillmailDetailsPerCycle)
	}
	if got := transport.calls.Load(); got != int64(maxKillmailDetailsPerCycle) {
		t.Fatalf("first pass: %d HTTP calls, want %d", got, maxKillmailDetailsPerCycle)
	}

	// Second pass: only the remaining 5 are fetched; stored ones skip.
	fetched, limited = app.warmKillmailDetails(ctx, ch)
	if limited || fetched != 5 {
		t.Fatalf("second pass: fetched %d limited %v, want 5/false", fetched, limited)
	}
	if got := transport.calls.Load(); got != 15 {
		t.Fatalf("second pass: %d HTTP calls total, want 15", got)
	}

	// Third pass: everything stored, no HTTP at all.
	fetched, _ = app.warmKillmailDetails(ctx, ch)
	if fetched != 0 || transport.calls.Load() != 15 {
		t.Fatalf("third pass: fetched %d, calls %d; want 0/15", fetched, transport.calls.Load())
	}

	ids, err := app.queries.ListKillmailDetailIDsByCharacter(ctx, ch.CharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 15 {
		t.Fatalf("stored detail ids: %d, want 15", len(ids))
	}
}

func TestWarmKillmailDetailsErrorLimit(t *testing.T) {
	transport := &stubTransport{status: http.StatusTooManyRequests}
	app, ch := seedKillmailCharacter(t, transport, 5)

	fetched, limited := app.warmKillmailDetails(context.Background(), ch)
	if !limited {
		t.Fatal("expected error-limit stop on 429")
	}
	if fetched != 0 {
		t.Fatalf("fetched %d on error limit, want 0", fetched)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("HTTP calls %d, want 1 (stop at first 429)", got)
	}
}
