package app

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// secondProcessApp builds a fresh Application over an existing
// test database: a simulated process restart, holding only what
// the database holds.
func secondProcessApp(t *testing.T, conn *sql.DB, pool *pgxpool.Pool, transport http.RoundTripper) *Application {
	t.Helper()
	queries := db.New(pool)
	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"
	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })
	client.RetryBackoffs = []time.Duration{0, 0} // retry, but do not sleep
	return &Application{
		cfg:           Config{workerTiersOff: true},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		pool:          pool,
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
}
