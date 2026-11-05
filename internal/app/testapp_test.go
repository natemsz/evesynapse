package app

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// NewCorpTestApp builds an Application over a fresh embedded-
// Postgres test database with a stub ESI transport (no worker
// goroutine — tests drive worker functions directly).
func NewCorpTestApp(t *testing.T, transport http.RoundTripper) (*Application, *sql.DB, *db.Queries) {
	t.Helper()
	conn, pool, err := store.Open(context.Background(), pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close(); pool.Close() })
	queries := db.New(conn)

	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"

	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })
	client.RetryBackoffs = []time.Duration{0, 0} // retry, but do not sleep

	app := &Application{
		cfg:           Config{adminCharIDs: map[int64]bool{}, workerTiersOff: true},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		pool:          pool,
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
	return app, conn, queries
}

// SessionCookie mints a signed-in session for the given user and character.
func (app *Application) SessionCookie(t *testing.T, userID, characterID int64, name string) *http.Cookie {
	t.Helper()
	ctx, err := app.sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionUserID, int(userID))
	putSessionCharID(app.sessions, ctx, characterID)
	app.sessions.Put(ctx, sessionCharacterName, name)
	token, _, err := app.sessions.Commit(ctx)
	if err != nil {
		t.Fatalf("session commit: %v", err)
	}
	return &http.Cookie{Name: "evesynapse_session", Value: token, Path: "/"}
}
