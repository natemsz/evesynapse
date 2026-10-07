package store

import (
	"context"
	"database/sql"
	_ "embed" // the schema steps below
	"fmt"
	"strings"
)

// The schema, one embedded file per step. schemaSteps lists them in
// order and migrateSchema applies whichever a database is missing,
// each in one transaction, recording it in schema_migrations. A new
// schema change is the next numbered file in schema_pg/, an embed
// here, and a line in schemaSteps.

// Step 001: the whole schema as of the move to Postgres.
//
//go:embed schema_pg/001_baseline.sql
var pgBaselineSchema string

// Step 002: the station leaderboard.
//
//go:embed schema_pg/002_station_leaderboard.sql
var pgStationLeaderboardSchema string

// Step 003: fitting metadata (is_public / is_draft on
// local_fittings).
//
//go:embed schema_pg/003_fit_metadata.sql
var pgFitMetadataSchema string

// Step 004 (v0.3.33): per-type market price TTL cache and industry
// cost index tracking.
//
//go:embed schema_pg/004_price_cache_costindex.sql
var pgPriceCacheSchema string

// Step 005 (v0.3.34): restock planner targets.
//
//go:embed schema_pg/005_restock.sql
var pgRestockSchema string

// Step 006 (v0.3.35): custom jump-clone names.
//
//go:embed schema_pg/006_clone_names.sql
var pgCloneNamesSchema string

// Step 007: foreign keys to users on the four per-user tables that
// lacked one. The first step applied purely by its record; steps
// 001–006 also carry a probe, for databases older than the record.
//
//go:embed schema_pg/007_user_foreign_keys.sql
var pgUserForeignKeysSchema string

// Step 008: the ETag each snapshot was stored with, so a refresh
// can ask ESI whether it changed instead of downloading it again.
//
//go:embed schema_pg/008_snapshot_etags.sql
var pgSnapshotETagsSchema string

// Steps 009–011: every time kept as TEXT becomes a timestamptz, one
// group of tables per step (accounts and snapshots; the market; the
// record and name caches).
//
//go:embed schema_pg/009_timestamps_accounts_snapshots.sql
var pgTimestampsAccountsSchema string

//go:embed schema_pg/010_timestamps_market.sql
var pgTimestampsMarketSchema string

//go:embed schema_pg/011_timestamps_records.sql
var pgTimestampsRecordsSchema string

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
// change is a new numbered file in schema_pg, an embed for it
// above, and one line here (no probe).
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
