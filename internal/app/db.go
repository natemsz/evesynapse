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
	// Schema step 002 (station leaderboard) rides the same
	// guarded path: fresh installs get it right after the
	// baseline above, existing installs gain it on their next
	// boot, and a database that already has it is left alone.
	var leaderboardTables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_station_leaderboard'`,
	).Scan(&leaderboardTables); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if leaderboardTables == 0 {
		if err := applySchema(conn, pgStationLeaderboardSchema); err != nil {
			conn.Close()
			pool.Close()
			return nil, nil, err
		}
	}
	// Schema step 003 (fitting metadata) rides the same guarded
	// path, probed on the is_public column: fresh installs get it
	// right after the baseline above, existing installs gain it on
	// their next boot, and a database that already has it is left
	// alone.
	var fitMetaCols int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'local_fittings' AND column_name = 'is_public'`,
	).Scan(&fitMetaCols); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if fitMetaCols == 0 {
		if err := applySchema(conn, pgFitMetadataSchema); err != nil {
			conn.Close()
			pool.Close()
			return nil, nil, err
		}
	}
	// Schema step 004 (v0.3.33: per-type price TTL cache + industry
	// cost indices) rides the same guarded path, probed on the
	// market_type_prices table.
	var priceCacheTables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_type_prices'`,
	).Scan(&priceCacheTables); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if priceCacheTables == 0 {
		if err := applySchema(conn, pgPriceCacheSchema); err != nil {
			conn.Close()
			pool.Close()
			return nil, nil, err
		}
	}
	// Schema step 005 (v0.3.34: restock planner targets) rides the
	// same guarded path, probed on the restock_targets table.
	var restockTables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'restock_targets'`,
	).Scan(&restockTables); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if restockTables == 0 {
		if err := applySchema(conn, pgRestockSchema); err != nil {
			conn.Close()
			pool.Close()
			return nil, nil, err
		}
	}
	// Schema step 006 (v0.3.35: custom jump-clone names) rides the
	// same guarded path, probed on the clone_names table.
	var cloneNameTables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'clone_names'`,
	).Scan(&cloneNameTables); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if cloneNameTables == 0 {
		if err := applySchema(conn, pgCloneNamesSchema); err != nil {
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
