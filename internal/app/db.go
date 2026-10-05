package app

import (
	"context"
	"database/sql"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx as a database/sql driver, registers as "pgx"
)

// openDB opens Postgres at dsn and returns the two handles the app
// runs on: a database/sql DB over the pgx stdlib driver (every
// sqlc query and every hand-rolled statement rides it) and a
// pgxpool that exists only to back the scs session store
// (pgxstore). On a database where the EveSynapse tables are
// absent it applies the collapsed Postgres baseline
// (schema_pg/001_baseline.sql) — the one-time fold of the old
// SQLite migrations 001–035, so a fresh install comes up with the
// full schema on first boot. Later schema changes land as new
// numbered files in schema_pg applied by this same guarded path.
func openDB(ctx context.Context, dsn string) (*sql.DB, *pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	conn.SetMaxOpenConns(20)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	// Sessions are the store's own table now (pgxstore does not
	// create it); the baseline carries its DDL, so a fresh
	// database gets it with everything else below.
	var usersTables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'users'`,
	).Scan(&usersTables); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if usersTables == 0 {
		if err := applySchema(conn, pgBaselineSchema); err != nil {
			conn.Close()
			pool.Close()
			return nil, nil, err
		}
	}
	return conn, pool, nil
}

// applySchema applies a schema script one statement at a time
// (the pgx stdlib driver Exec handles one statement at a time).
// Full-line -- comments are stripped first: they may contain ";"
// and would otherwise break the naive split.
func applySchema(conn *sql.DB, script string) error {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
