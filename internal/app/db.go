package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib" // pgx as a database/sql driver
)

// openDB opens Postgres at dsn and returns the two handles the app
// runs on: a database/sql DB over the pgx stdlib driver (every
// sqlc query and every hand-rolled statement rides it) and a
// pgxpool that exists only to back the scs session store
// (pgxstore). It then brings the schema up to date (migrateSchema):
// a fresh database gets the collapsed Postgres baseline
// (schema_pg/001_baseline.sql) and every later numbered step, so a
// fresh install comes up with the full schema on first boot, and an
// existing install gains whichever steps it is missing.
func openDB(ctx context.Context, dsn string) (*sql.DB, *pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	conn := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(readTimesInUTC))
	conn.SetMaxOpenConns(20)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if err := migrateSchema(ctx, conn); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	return conn, pool, nil
}

// readTimesInUTC runs on every new connection and makes each
// timestamptz it reads come back in UTC. Left alone the driver hands
// times back in the machine's own zone, so a page would print one
// thing on a UTC server and another on a laptop. EVE runs on UTC and
// so does everything here; the instant itself is the same either way.
func readTimesInUTC(_ context.Context, c *pgx.Conn) error {
	c.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
}

// schemaStep is one numbered file in schema_pg. Steps apply in
// version order, each exactly once per database; schema_migrations
// records which ones have landed.
type schemaStep struct {
	version int
	name    string
	script  string
	// legacyProbe counts the objects the step creates. It exists
	// for databases that predate schema_migrations: a step whose
	// objects are already there is recorded as applied instead of
	// being run again. Steps added from here on leave it empty —
	// the record alone decides.
	legacyProbe string
}

// schemaSteps lists every schema step in order. A new schema
// change is a new numbered file in schema_pg, an embed for it in
// app.go, and one line here (no probe).
func schemaSteps() []schemaStep {
	return []schemaStep{
		// The baseline carries the sessions table too (pgxstore
		// does not create it).
		{1, "baseline", pgBaselineSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'users'`},
		{2, "station_leaderboard", pgStationLeaderboardSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_station_leaderboard'`},
		{3, "fit_metadata", pgFitMetadataSchema,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'local_fittings' AND column_name = 'is_public'`},
		{4, "price_cache_costindex", pgPriceCacheSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_type_prices'`},
		{5, "restock", pgRestockSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'restock_targets'`},
		{6, "clone_names", pgCloneNamesSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'clone_names'`},
		// From here on the record alone decides: no probes.
		{7, "user_foreign_keys", pgUserForeignKeysSchema, ""},
		{8, "snapshot_etags", pgSnapshotETagsSchema, ""},
		{9, "timestamps_accounts_snapshots", pgTimestampsAccountsSchema, ""},
		{10, "timestamps_market", pgTimestampsMarketSchema, ""},
		{11, "timestamps_records", pgTimestampsRecordsSchema, ""},
	}
}

// schemaMigrationsDDL is the record of applied steps. It lives
// here rather than in schema_pg because it has to exist before any
// step can be recorded.
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migrateSchema applies every schema step the database is missing,
// in order.
func migrateSchema(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("schema: create schema_migrations: %w", err)
	}
	for _, step := range schemaSteps() {
		if err := applySchemaStep(ctx, conn, step); err != nil {
			return fmt.Errorf("schema step %03d (%s): %w", step.version, step.name, err)
		}
	}
	return nil
}

// applySchemaStep lands one step atomically: its statements and
// its schema_migrations row commit together or not at all (DDL is
// transactional in Postgres). A step that fails halfway therefore
// leaves nothing behind for the next boot to mistake for a
// finished schema — it simply runs again. The advisory lock keeps
// two processes starting at the same moment (the server and a
// maintenance run) from applying the same step twice; it is
// released with the transaction.
func applySchemaStep(ctx context.Context, conn *sql.DB, step schemaStep) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed

	// 1165387091 is "EveS" in ASCII (0x45766553): an arbitrary
	// key this app alone takes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1165387091)`); err != nil {
		return err
	}
	var recorded int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, step.version,
	).Scan(&recorded); err != nil {
		return err
	}
	if recorded > 0 {
		return tx.Commit()
	}

	present := 0
	if step.legacyProbe != "" {
		if err := tx.QueryRowContext(ctx, step.legacyProbe).Scan(&present); err != nil {
			return err
		}
	}
	if present == 0 {
		for _, stmt := range splitSchemaStatements(step.script) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, step.version, step.name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// splitSchemaStatements cuts a schema script into its statements
// (the pgx stdlib driver executes one statement at a time).
// Full-line -- comments are stripped first: they may contain ";"
// and would otherwise break the naive split.
func splitSchemaStatements(script string) []string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var stmts []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}
