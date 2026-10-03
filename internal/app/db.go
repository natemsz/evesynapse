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
	return conn, nil
}

// applySchema executes a multi-statement DDL script statement by
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
