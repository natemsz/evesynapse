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

//go:embed schema_pg/012_notifications.sql
var pgNotificationsSchema string

//go:embed schema_pg/013_push_subscriptions.sql
var pgPushSubscriptionsSchema string

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
		{12, "notifications", pgNotificationsSchema, ""},
		{13, "push_subscriptions", pgPushSubscriptionsSchema, ""},
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
// (the pgx stdlib driver executes one statement at a time). The
// split is lexically aware: semicolons inside string literals,
// quoted identifiers, comments, and dollar-quoted function bodies
// do not end a statement, and comments are stripped. The old
// splitter cut on every ";" outside full-line comments, so the
// first migration with a literal ';' or a plpgsql body would have
// failed boot.
func splitSchemaStatements(script string) []string {
	var stmts []string
	var cur strings.Builder
	flush := func() {
		if stmt := strings.TrimSpace(cur.String()); stmt != "" {
			stmts = append(stmts, stmt)
		}
		cur.Reset()
	}
	i, n := 0, len(script)
	for i < n {
		c := script[i]
		switch {
		case c == ';':
			flush()
			i++
		case c == '\'':
			i = copyQuoted(&cur, script, i, '\'', isEscapeStringPrefix(script, i))
		case c == '"':
			i = copyQuoted(&cur, script, i, '"', false)
		case c == '-' && i+1 < n && script[i+1] == '-':
			// Line comment to the end of the line; the newline that
			// ends it is not skipped, so the tokens around it cannot fuse.
			j := i + 2
			for j < n && script[j] != '\n' {
				j++
			}
			i = j
		case c == '/' && i+1 < n && script[i+1] == '*':
			// Block comment, which nests in Postgres; a space
			// stays behind for the same reason as above.
			depth := 1
			j := i + 2
			for j < n && depth > 0 {
				if script[j] == '/' && j+1 < n && script[j+1] == '*' {
					depth++
					j += 2
				} else if script[j] == '*' && j+1 < n && script[j+1] == '/' {
					depth--
					j += 2
				} else {
					j++
				}
			}
			cur.WriteByte(' ')
			i = j
		case c == '$':
			if end, ok := scanDollarQuote(script, i); ok {
				cur.WriteString(script[i:end])
				i = end
			} else {
				cur.WriteByte(c) // a $1 parameter, not a quote
				i++
			}
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts
}

// copyQuoted copies a '- or "-quoted span starting at script[i]
// (the opening quote) into cur and returns the index past it. A
// doubled quote is an escaped quote; with escapes (E'...' strings)
// a backslash escapes the next byte too. An unterminated span
// copies to the end and lets Postgres report the error.
func copyQuoted(cur *strings.Builder, script string, i int, quote byte, escapes bool) int {
	j, n := i+1, len(script)
	for j < n {
		if escapes && script[j] == '\\' {
			j += 2
			continue
		}
		if script[j] == quote {
			if j+1 < n && script[j+1] == quote {
				j += 2
				continue
			}
			j++
			break
		}
		j++
	}
	if j > n {
		j = n
	}
	cur.WriteString(script[i:j])
	return j
}

// isEscapeStringPrefix reports whether the quote at script[i] opens
// an E'...' escape string: an E immediately before it that is not
// itself part of a longer identifier.
func isEscapeStringPrefix(script string, i int) bool {
	if i == 0 || (script[i-1] != 'e' && script[i-1] != 'E') {
		return false
	}
	if i >= 2 && isIdentByte(script[i-2]) {
		return false
	}
	return true
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scanDollarQuote matches the dollar-quoted body starting at
// script[i] (the opening $) and returns the index past its closing
// tag. It reports false when the $ opens no quote (a $1 parameter)
// or the body never closes (copied literally; Postgres reports it).
func scanDollarQuote(script string, i int) (int, bool) {
	j, n := i+1, len(script)
	if j < n && script[j] >= '0' && script[j] <= '9' {
		return 0, false // $1: a parameter (tags never start with a digit)
	}
	for j < n && script[j] != '$' {
		if !isDollarTagByte(script[j]) {
			return 0, false
		}
		j++
	}
	if j >= n {
		return 0, false
	}
	tag := script[i : j+1]
	end := strings.Index(script[j+1:], tag)
	if end < 0 {
		return 0, false
	}
	return j + 1 + end + len(tag), true
}

func isDollarTagByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
