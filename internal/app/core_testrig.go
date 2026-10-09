package app

// Test rig: the one exported seam the black-box page tests in
// internal/app/tests build through. It assembles a fixture
// Application over a fresh test database (an empty Postgres
// database minted by internal/pgtest, handed in as a DSN) with
// a caller-supplied ESI transport (the hermetic mirror of
// buildCorpTestApp), and exposes only the five operations those
// tests are allowed: the real router, the query handle, the raw
// DB, a signed-in session cookie, and Close. No internals leave
// the package, and no worker goroutine ever starts.

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/store"
)

// TestRig is an opaque fixture Application for black-box tests.
type TestRig struct {
	app *Application
}

// NewTestRig builds a fixture Application over the fresh
// database at dsn, with ESI served by transport.
func NewTestRig(dsn string, transport http.RoundTripper) (*TestRig, error) {
	conn, pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	queries := db.New(conn)

	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"

	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })
	client.RetryBackoffs = []time.Duration{0, 0} // retry, but do not sleep

	return &TestRig{app: &Application{
		cfg:           Config{workerTiersOff: true},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		pool:          pool,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}}, nil
}

// Handler returns the application's HTTP handler (the real
// chi router, sessions included).
func (r *TestRig) Handler() http.Handler { return r.app.Handler() }

// Queries returns the fixture database's query handle.
func (r *TestRig) Queries() *db.Queries { return r.app.queries }

// DB returns the fixture database connection.
func (r *TestRig) DB() *sql.DB { return r.app.db }

// SessionCookie mints a signed-in session for the user.
func (r *TestRig) SessionCookie(userID, characterID int64, name string) (*http.Cookie, error) {
	sessions := r.app.sessions
	ctx, err := sessions.Load(context.Background(), "")
	if err != nil {
		return nil, err
	}
	sessions.Put(ctx, sessionAuthenticated, true)
	sessions.Put(ctx, sessionUserID, int(userID))
	putSessionCharID(sessions, ctx, characterID)
	sessions.Put(ctx, sessionCharacterName, name)
	token, _, err := sessions.Commit(ctx)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{Name: "evesynapse_session", Value: token, Path: "/"}, nil
}

// Close closes the fixture database handles.
func (r *TestRig) Close() { r.app.Close() }

// GrantAdmin marks characterID as an admin in the rig's config
// (Issues 23/24): /admin/ and /sync/ require admin character
// identity, so tests asserting the full nav must opt in.
func (r *TestRig) GrantAdmin(characterID int64) {
	if r.app.cfg.adminCharIDs == nil {
		r.app.cfg.adminCharIDs = map[int64]bool{}
	}
	r.app.cfg.adminCharIDs[characterID] = true
}
