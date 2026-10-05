package app

// End-to-end proof for the -migrate-pg copy: a fixture SQLite
// database (a representative subset of the real schema with
// known rows) is migrated into a fresh embedded Postgres, and
// the test verifies per-table row counts, a refresh-token spot
// check, the identity-sequence rewind (the next live insert must
// not collide with a copied id), the non-empty-target refusal
// and its -force re-run, and that the SQLite file is byte-for-
// byte untouched afterwards.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/pgtest"
)

const fixtureSchema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, created_at TEXT NOT NULL DEFAULT (datetime('now')), home_layout TEXT NOT NULL DEFAULT '', last_briefing_at TEXT NOT NULL DEFAULT '');
CREATE TABLE characters (character_id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE, name TEXT NOT NULL DEFAULT '', access_token TEXT NOT NULL DEFAULT '', refresh_token TEXT NOT NULL DEFAULT '', token_expiry TEXT, scopes TEXT NOT NULL DEFAULT '', cached_until TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now')), owner_hash TEXT NOT NULL DEFAULT '', tags TEXT NOT NULL DEFAULT '', link_state TEXT NOT NULL DEFAULT 'ok', link_state_at TEXT);
CREATE TABLE skill_plans (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE, character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE, name TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE local_fittings (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '', ship_type_id INTEGER NOT NULL DEFAULT 0, items_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '');
CREATE TABLE order_lifecycle (character_id INTEGER NOT NULL, order_id INTEGER NOT NULL, type_id INTEGER NOT NULL, location_id INTEGER NOT NULL, region_id INTEGER NOT NULL, is_buy_order INTEGER NOT NULL DEFAULT 0, listed_price REAL NOT NULL DEFAULT 0, volume_total INTEGER NOT NULL DEFAULT 0, volume_remain_last INTEGER NOT NULL DEFAULT 0, first_seen_at TEXT NOT NULL DEFAULT '', last_seen_at TEXT NOT NULL DEFAULT '', closed_at TEXT NOT NULL DEFAULT '', close_kind TEXT NOT NULL DEFAULT '', outbid_events INTEGER NOT NULL DEFAULT 0, beaten_now INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (character_id, order_id));
CREATE TABLE market_history (region_id INTEGER NOT NULL, type_id INTEGER NOT NULL, date TEXT NOT NULL, average REAL NOT NULL DEFAULT 0, highest REAL NOT NULL DEFAULT 0, lowest REAL NOT NULL DEFAULT 0, volume INTEGER NOT NULL DEFAULT 0, order_count INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (region_id, type_id, date));
CREATE TABLE widget_configs (user_id INTEGER NOT NULL, widget_id TEXT NOT NULL, config TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL, PRIMARY KEY (user_id, widget_id));
`

func seedFixtureSQLite(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer w.Close()
	if _, err := w.Exec(fixtureSchema); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	stmts := []string{
		`INSERT INTO users (id, created_at) VALUES (1, '2026-01-01T00:00:00'), (2, '2026-01-02T00:00:00')`,
		`INSERT INTO characters (character_id, user_id, name, access_token, refresh_token, token_expiry, created_at, updated_at)
		 VALUES (90000001, 1, 'Alpha', 'a-access', 'tok-alpha', '2999-01-01T00:00:00Z', '2026-01-01T00:00:00', '2026-01-01T00:00:00'),
		        (90000002, 1, 'Beta', 'b-access', 'tok-beta', NULL, '2026-01-01T00:00:00', '2026-01-01T00:00:00'),
		        (90000003, 2, 'Gamma', 'g-access', 'tok-gamma', NULL, '2026-01-02T00:00:00', '2026-01-02T00:00:00')`,
		`INSERT INTO skill_plans (id, user_id, character_id, name, created_at) VALUES (10, 1, 90000001, 'Plan A', '2026-01-03T00:00:00')`,
		`INSERT INTO local_fittings (id, user_id, name, ship_type_id) VALUES (7, 1, 'Fit A', 587)`,
		`INSERT INTO order_lifecycle (character_id, order_id, type_id, location_id, region_id, listed_price, first_seen_at, last_seen_at)
		 VALUES (90000001, 101, 34, 60003760, 10000002, 5.5, '2026-01-01T00:00:00', '2026-01-02T00:00:00'),
		        (90000001, 102, 35, 60003760, 10000002, 9.25, '2026-01-01T00:00:00', '')`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
		 VALUES (10000002, 34, '2026-10-01', 5.5, 6.0, 5.0, 1000, 42)`,
		`INSERT INTO widget_configs (user_id, widget_id, config, updated_at) VALUES (1, 'orders', '{"scope":"all"}', '2026-01-01T00:00:00')`,
	}
	for _, s := range stmts {
		if _, err := w.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func TestMigrateSQLiteToPGEndToEnd(t *testing.T) {
	path := seedFixtureSQLite(t)
	before := fileHash(t, path)
	ctx := context.Background()

	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()

	dst, pool, err := openDB(ctx, pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer dst.Close()
	defer pool.Close()

	report, err := migrateSQLiteToPG(ctx, src, dst, false)
	if err != nil {
		t.Fatalf("migrate: %v\nreport:\n%s", err, strings.Join(report, "\n"))
	}

	want := map[string]int64{
		"users": 2, "characters": 3, "skill_plans": 1,
		"local_fittings": 1, "order_lifecycle": 2,
		"market_history": 1, "widget_configs": 1,
	}
	for table, n := range want {
		var got int64
		if err := dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != n {
			t.Errorf("%s: %d rows in Postgres, want %d", table, got, n)
		}
	}
	// Float fidelity across the copy.
	var price float64
	if err := dst.QueryRowContext(ctx,
		`SELECT listed_price FROM order_lifecycle WHERE character_id = 90000001 AND order_id = 101`).Scan(&price); err != nil {
		t.Fatalf("read copied price: %v", err)
	}
	if price != 5.5 {
		t.Errorf("listed_price = %v, want 5.5", price)
	}
	// Refresh token spot check (also verified inside the copy).
	var token string
	if err := dst.QueryRowContext(ctx,
		`SELECT refresh_token FROM characters WHERE character_id = 90000002`).Scan(&token); err != nil {
		t.Fatalf("read copied token: %v", err)
	}
	if token != "tok-beta" {
		t.Errorf("refresh_token = %q, want tok-beta", token)
	}
	// NULL fidelity: Beta's token_expiry was NULL.
	var expiry sql.NullString
	if err := dst.QueryRowContext(ctx,
		`SELECT token_expiry FROM characters WHERE character_id = 90000002`).Scan(&expiry); err != nil {
		t.Fatalf("read copied expiry: %v", err)
	}
	if expiry.Valid {
		t.Errorf("token_expiry = %q, want NULL", expiry.String)
	}
	// Identity rewind: the next live inserts must land past the
	// copied ids (2, 10, 7).
	q := db.New(dst)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user after migration: %v", err)
	}
	if user.ID <= 2 {
		t.Errorf("new user id = %d, want > 2 (sequence not rewound)", user.ID)
	}
	plan, err := q.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
		UserID: 1, CharacterID: 90000001, Name: "Plan B", CreatedAt: "2026-01-04T00:00:00",
	})
	if err != nil {
		t.Fatalf("create skill plan after migration: %v", err)
	}
	if plan.ID <= 10 {
		t.Errorf("new skill plan id = %d, want > 10 (sequence not rewound)", plan.ID)
	}

	// A second run into the now non-empty target refuses...
	if _, err := migrateSQLiteToPG(ctx, src, dst, false); err == nil ||
		!strings.Contains(err.Error(), "already holds") {
		t.Fatalf("second migrate err = %v, want the non-empty refusal", err)
	}
	// ...and -force re-copies cleanly.
	if _, err := migrateSQLiteToPG(ctx, src, dst, true); err != nil {
		t.Fatalf("forced re-migrate: %v", err)
	}
	var users int64
	if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("recount users: %v", err)
	}
	if users != 2 {
		t.Errorf("users after forced re-copy = %d, want 2", users)
	}

	// The SQLite file was never written.
	if after := fileHash(t, path); after != before {
		t.Error("SQLite fixture changed during migration")
	}
}
