package app

// Hermetic tests for -refresh: its cache expiry against a seeded
// database, earned data included so the "never touched" list is
// proven, not promised, and its refusal to run while the server is
// up (against a throwaway `sleep` process). The -update tests are
// with the updater, in internal/selfupdate.

import (
	"bytes"
	"context"
	"database/sql"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/pidfile/pidfiletest"
	"evesynapse/internal/store"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// -refresh: expiry against a seeded database.
// ---------------------------------------------------------------------------

const testFreshStamp = "2026-10-04T00:00:00Z"

// seedRefreshDB builds a database holding one of everything the
// refresh cares about: live caches with fresh stamps, and earned
// data whose survival the test then asserts. Returns the DSN
// (for building the Config runRefresh takes) and the open handle.
func seedRefreshDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	stmts := []string{
		`INSERT INTO users (id) VALUES (1)`,
		`INSERT INTO characters (character_id, user_id, name, access_token, refresh_token, cached_until)
		 VALUES (9001, 1, 'Test Char', 'access', 'refresh', '` + testFreshStamp + `')`,
		// Caches with fresh stamps.
		`INSERT INTO character_snapshots (character_id, kind, payload, fetched_at, cached_until)
		 VALUES (9001, 'wallet', '{"balance":1}', '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO global_snapshots (kind, payload, fetched_at, cached_until)
		 VALUES ('incursions', '[]', '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO guide_prices_meta (id, fetched_at, cached_until)
		 VALUES (1, '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO snapshot_fetch_state (character_id, kind, state, detail, attempted_at)
		 VALUES (9001, 'skills', 'ok', '', '` + testFreshStamp + `')`,
		`INSERT INTO market_fetch_state (kind, state, detail, attempted_at)
		 VALUES ('history_10000002_34', 'ok', '', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (777, '{"name":"Pilot"}', 'ready', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (778, '', 'missing', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (779, '', 'pending', NULL)`,
		`INSERT INTO type_details (type_id, description, fetched_at)
		 VALUES (34, 'A mineral.', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000001, 'Jita IV-4', 'resolved', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000002, '', 'missing', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000003, '', 'pending', NULL)`,
		`INSERT INTO sde_meta (key, value) VALUES ('sde_import_version', '6')`,
		// Earned data: must survive untouched.
		`INSERT INTO wallet_history (user_id, character_id, day, balance, net_worth, sampled_at)
		 VALUES (1, 9001, '2026-10-03', 123.45, 999.5, '` + testFreshStamp + `')`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
		 VALUES (10000002, 34, '2026-10-01', 5.5, 6.0, 5.0, 100, 10)`,
		`INSERT INTO market_watchlist (user_id, type_id, region_id, threshold_pct, created_at)
		 VALUES (1, 34, 10000002, 5.0, '` + testFreshStamp + `')`,
		`INSERT INTO widget_configs (user_id, widget_id, config, updated_at)
		 VALUES (1, 'orders', '{"scope":"all"}', '` + testFreshStamp + `')`,
		`INSERT INTO skill_plans (id, user_id, character_id, name, created_at)
		 VALUES (10, 1, 9001, 'My Plan', '` + testFreshStamp + `')`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return dsn, conn
}

func queryString(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	var v string
	if err := conn.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// queryStamp reads one time column the way the app prints a time
// (RFC 3339, UTC), with "" for one that was never set.
func queryStamp(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	var v sql.NullTime
	if err := conn.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return rfc3339Or(v, "")
}

func TestRefreshExpiresCachesKeepsEarnedData(t *testing.T) {
	dsn, conn := seedRefreshDB(t)
	conn.Close()

	cfg := Config{databaseURL: dsn}
	var out, errOut bytes.Buffer
	// No pidfile anywhere near the fake executable path: run with
	// a path that doesn't exist so no live-server check fires.
	if code := runRefresh(cfg, filepath.Join(t.TempDir(), pidfile.Name), &out, &errOut); code != 0 {
		t.Fatalf("runRefresh code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "were not touched") {
		t.Fatalf("output %q missing the earned-data note", out.String())
	}

	conn2, pool2, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()
	defer pool2.Close()

	// Every consulted cache rewound to the epoch.
	if got := queryStamp(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("character_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM character_snapshots WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("character_snapshots.fetched_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM characters WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("characters.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM global_snapshots WHERE kind = 'incursions'`); got != rfc3339(cacheEpoch) {
		t.Fatalf("global_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM guide_prices_meta WHERE id = 1`); got != rfc3339(cacheEpoch) {
		t.Fatalf("guide_prices_meta.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT attempted_at FROM snapshot_fetch_state WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("snapshot_fetch_state.attempted_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT attempted_at FROM market_fetch_state WHERE kind = 'history_10000002_34'`); got != rfc3339(cacheEpoch) {
		t.Fatalf("market_fetch_state.attempted_at = %q, want epoch", got)
	}
	// Pilot records: ready rewound, missing settled, pending queued — as they were.
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 777`); got != rfc3339(cacheEpoch) {
		t.Fatalf("pilot ready fetched_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 778`); got != testFreshStamp {
		t.Fatalf("pilot missing fetched_at = %q, want untouched", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 779`); got != "" {
		t.Fatalf("pilot pending fetched_at = %q, want untouched empty", got)
	}
	// Type descriptions re-asked (empty stamp), text preserved meanwhile.
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM type_details WHERE type_id = 34`); got != "" {
		t.Fatalf("type_details.fetched_at = %q, want empty (re-ask)", got)
	}
	if got := queryString(t, conn2, `SELECT description FROM type_details WHERE type_id = 34`); got != "A mineral." {
		t.Fatalf("type_details.description = %q, want preserved", got)
	}
	// Structure names: settled rows rewound, pending row as it was, names kept.
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("structure resolved resolved_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000002`); got != rfc3339(cacheEpoch) {
		t.Fatalf("structure missing resolved_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000003`); got != "" {
		t.Fatalf("structure pending resolved_at = %q, want untouched empty", got)
	}
	if got := queryString(t, conn2, `SELECT name FROM structure_names WHERE structure_id = 60000001`); got != "Jita IV-4" {
		t.Fatalf("structure name = %q, want preserved", got)
	}
	// SDE re-import marker reset (anything but the current version).
	if got := queryString(t, conn2, `SELECT value FROM sde_meta WHERE key = 'sde_import_version'`); got == "6" || got == "" {
		t.Fatalf("sde_import_version = %q, want a reset marker", got)
	}

	// Earned data byte-for-byte intact.
	if got := queryString(t, conn2, `SELECT access_token FROM characters WHERE character_id = 9001`); got != "access" {
		t.Fatalf("access_token = %q, want untouched", got)
	}
	if got := queryString(t, conn2, `SELECT payload FROM character_snapshots WHERE character_id = 9001`); got != `{"balance":1}` {
		t.Fatalf("snapshot payload = %q, want preserved", got)
	}
	var balance, netWorth float64
	if err := conn2.QueryRow(`SELECT balance, net_worth FROM wallet_history WHERE character_id = 9001`).Scan(&balance, &netWorth); err != nil {
		t.Fatal(err)
	}
	if balance != 123.45 || netWorth != 999.5 {
		t.Fatalf("wallet_history = %v/%v, want untouched", balance, netWorth)
	}
	var average float64
	if err := conn2.QueryRow(`SELECT average FROM market_history WHERE type_id = 34`).Scan(&average); err != nil {
		t.Fatal(err)
	}
	if average != 5.5 {
		t.Fatalf("market_history average = %v, want untouched", average)
	}
	if got := queryString(t, conn2, `SELECT config FROM widget_configs WHERE user_id = 1 AND widget_id = 'orders'`); got != `{"scope":"all"}` {
		t.Fatalf("widget_configs = %q, want untouched", got)
	}
	if got := queryString(t, conn2, `SELECT name FROM skill_plans WHERE id = 10`); got != "My Plan" {
		t.Fatalf("skill_plans = %q, want untouched", got)
	}
	var watchCount int
	if err := conn2.QueryRow(`SELECT COUNT(*) FROM market_watchlist WHERE user_id = 1`).Scan(&watchCount); err != nil {
		t.Fatal(err)
	}
	if watchCount != 1 {
		t.Fatalf("market_watchlist rows = %d, want untouched 1", watchCount)
	}
}

func TestRefreshRefusesWhileServerRuns(t *testing.T) {
	dsn, conn := seedRefreshDB(t)
	conn.Close()

	sleeper := pidfiletest.StartSleep(t)
	pidPath := filepath.Join(t.TempDir(), pidfile.Name)
	writeTestFile(t, pidPath, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runRefresh(Config{databaseURL: dsn}, pidPath, &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want refusal (1); stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "still running") {
		t.Fatalf("stderr %q missing the still-running explanation", errOut.String())
	}

	// The refusal happened before the database was touched.
	conn2, pool2, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()
	defer pool2.Close()
	if got := queryStamp(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != testFreshStamp {
		t.Fatalf("cache stamp = %q after refusal, want untouched fresh stamp", got)
	}
}

func TestRefreshMissingDatabase(t *testing.T) {
	// A DSN naming a database that was never created: the
	// refresh must refuse and must not create anything.
	missing := pgtest.DSNFor(t, "evetest_missing")
	var out, errOut bytes.Buffer
	if code := runRefresh(Config{databaseURL: missing}, "", &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want 1", code)
	}
	if _, _, err := store.Open(context.Background(), missing); err == nil {
		t.Fatal("runRefresh created a database where none existed")
	}
}
