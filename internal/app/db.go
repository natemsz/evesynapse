package app

import (
	"database/sql"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers as "sqlite"
)

// openDB opens the SQLite database at path and applies the embedded
// schemas on first boot (001 on an empty database, 002 when the
// snapshot tables are absent, 003 for the SDE tables, 004 for the
// killmail detail store, 005 for the corporation tables), plus the
// sessions table the scs sqlite3store expects.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		return nil, err
	}
	// The scs sqlite3store expects a sessions table; create it if needed.
	// (users/characters live in schema/001_init.sql and are managed via sqlc.)
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		expiry REAL NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(`CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expiry)`)
	if err != nil {
		return nil, err
	}
	// Apply the initial schema on first boot: a fresh DB_PATH has no
	// users/characters tables, and nothing else in the app creates them.
	// This is one-time creation of the sqlc-managed schema, not a
	// migration framework — later schema changes need new schema files.
	var usersTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&usersTables); err != nil {
		return nil, err
	}
	if usersTables == 0 {
		if err := applySchema(conn, initSchema); err != nil {
			return nil, err
		}
	}
	// Schema 002 (snapshot + type-name caches), applied the same
	// guarded way: only when its tables don't exist yet. On an
	// existing database this adds the new tables alongside the old
	// ones without disturbing them.
	var snapshotTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'character_snapshots'`).Scan(&snapshotTables); err != nil {
		return nil, err
	}
	if snapshotTables == 0 {
		if err := applySchema(conn, snapshotsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 003 (SDE static data), applied the same guarded way.
	var sdeTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_types'`).Scan(&sdeTables); err != nil {
		return nil, err
	}
	if sdeTables == 0 {
		if err := applySchema(conn, sdeSchema); err != nil {
			return nil, err
		}
	}
	// Schema 004 (module sweep: killmail detail store), applied the
	// same guarded way.
	var killmailTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'killmail_details'`).Scan(&killmailTables); err != nil {
		return nil, err
	}
	if killmailTables == 0 {
		if err := applySchema(conn, moduleSweepSchema); err != nil {
			return nil, err
		}
	}
	// Schema 005 (corporation cluster: fetch-state log, character →
	// corporation map, item names), applied the same guarded way.
	var corpTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'snapshot_fetch_state'`).Scan(&corpTables); err != nil {
		return nil, err
	}
	if corpTables == 0 {
		if err := applySchema(conn, corpSchema); err != nil {
			return nil, err
		}
	}
	// Schema 006 (economy cluster: contract detail store), applied
	// the same guarded way.
	var contractTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'contract_details'`).Scan(&contractTables); err != nil {
		return nil, err
	}
	if contractTables == 0 {
		if err := applySchema(conn, economySchema); err != nil {
			return nil, err
		}
	}
	// Schema 007 (intel cluster: global public-data store + war
	// detail store), applied the same guarded way.
	var intelTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'global_snapshots'`).Scan(&intelTables); err != nil {
		return nil, err
	}
	if intelTables == 0 {
		if err := applySchema(conn, intelSchema); err != nil {
			return nil, err
		}
	}
	// Schema 008 (marketable SDE types: market group + published
	// flag), guarded on the columns themselves: an existing
	// database gets the ALTERs, a new one gets them right after
	// 003 creates the base table.
	var marketCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sde_types') WHERE name IN ('market_group_id', 'published')`).Scan(&marketCols); err != nil {
		return nil, err
	}
	if marketCols < 2 {
		if err := applySchema(conn, marketableSchema); err != nil {
			return nil, err
		}
	}
	// Schema 009 (multi-character foundation: owner hash, tags,
	// link state on characters), guarded on the columns
	// themselves like 008.
	var foundationCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('characters') WHERE name IN ('owner_hash', 'tags', 'link_state', 'link_state_at')`).Scan(&foundationCols); err != nil {
		return nil, err
	}
	if foundationCols < 4 {
		if err := applySchema(conn, characterFoundationSchema); err != nil {
			return nil, err
		}
	}
	// Schema 010 (Phase 1B widget home: per-account home layout
	// JSON). Column-existence guard, same as schema 009's columns.
	var homeLayoutCols int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name IN ('home_layout')`,
	).Scan(&homeLayoutCols); err != nil {
		return nil, err
	}
	if homeLayoutCols < 1 {
		if err := applySchema(conn, homeLayoutSchema); err != nil {
			return nil, err
		}
	}
	// Schema 011 (Phase 3 industry planner: blueprint static
	// data), applied the same guarded way: only when its table
	// doesn't exist yet.
	var plannerTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_blueprints'`).Scan(&plannerTables); err != nil {
		return nil, err
	}
	if plannerTables == 0 {
		if err := applySchema(conn, industryPlannerSchema); err != nil {
			return nil, err
		}
	}
	// Schema 012 (Phase 4 skill plans: dogma skill graph +
	// user plans), applied the same guarded way.
	var skillPlanTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_skill_meta'`).Scan(&skillPlanTables); err != nil {
		return nil, err
	}
	if skillPlanTables == 0 {
		if err := applySchema(conn, skillPlansSchema); err != nil {
			return nil, err
		}
	}
	// Schema 013 (Phase 5 market history + alerts: history rows,
	// wants, fetch state, watchlist, order health), applied the
	// same guarded way.
	var marketHistoryTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'market_history'`).Scan(&marketHistoryTables); err != nil {
		return nil, err
	}
	if marketHistoryTables == 0 {
		if err := applySchema(conn, marketHistorySchema); err != nil {
			return nil, err
		}
	}
	// Schema 014 (player structure-name resolution queue), applied
	// the same guarded way.
	var structureNameTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'structure_names'`).Scan(&structureNameTables); err != nil {
		return nil, err
	}
	if structureNameTables == 0 {
		if err := applySchema(conn, structureNamesSchema); err != nil {
			return nil, err
		}
	}
	// Schema 015 (public pilot records + item type details: the
	// wants queues behind /pilot/ and the item details page),
	// applied the same guarded way.
	var pilotTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pilot_records'`).Scan(&pilotTables); err != nil {
		return nil, err
	}
	if pilotTables == 0 {
		if err := applySchema(conn, pilotRecordsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 016 (pilot drain priority: viewed wants outrank the
	// proactively noted orbit), guarded on the column itself like
	// 008/009.
	var pilotPriorityCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pilot_records') WHERE name = 'priority'`).Scan(&pilotPriorityCols); err != nil {
		return nil, err
	}
	if pilotPriorityCols < 1 {
		if err := applySchema(conn, pilotPrioritySchema); err != nil {
			return nil, err
		}
	}
	// Schema 017 (Phase 6 briefing: the account's last-looked
	// anchor for the home digest), column-guarded like 010.
	var briefingAnchorCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'last_briefing_at'`).Scan(&briefingAnchorCols); err != nil {
		return nil, err
	}
	if briefingAnchorCols < 1 {
		if err := applySchema(conn, briefingAnchorSchema); err != nil {
			return nil, err
		}
	}
	// Schema 018 (item descriptions bulk-cached from the SDE
	// invTypes dump), column-guarded like 008/009: every database
	// that lacks the column — fresh ones included, since 003
	// predates it — gains it here.
	var sdeDescriptionCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sde_types') WHERE name = 'description'`).Scan(&sdeDescriptionCols); err != nil {
		return nil, err
	}
	if sdeDescriptionCols < 1 {
		if err := applySchema(conn, sdeTypeDescriptionsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 019 (daily wallet-history sampler: one balance row
	// per character per day, written by the worker from snapshots
	// it already keeps), applied the same guarded way.
	var walletHistoryTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'wallet_history'`).Scan(&walletHistoryTables); err != nil {
		return nil, err
	}
	if walletHistoryTables == 0 {
		if err := applySchema(conn, walletHistorySchema); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// statement (the modernc driver Exec handles one statement at a time).
// Full-line -- comments are stripped first: they may contain ";" and
// would otherwise break the naive split.
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
