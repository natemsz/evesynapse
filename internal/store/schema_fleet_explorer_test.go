package store

import (
	"context"
	"testing"

	"evesynapse/internal/pgtest"
)

// TestFleetExplorerBackfillQueuesOldReports: on upgrade, step 173
// queues for a rebuild every fleet with a battle that has no explorer,
// and leaves alone a fleet whose battles all have one.
func TestFleetExplorerBackfillQueuesOldReports(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	newFleet := func(title string) int64 {
		t.Helper()
		var id int64
		if err := conn.QueryRow(`INSERT INTO ops (corporation_id, title, starts_at) VALUES (1, $1, now() - interval '2 days') RETURNING id`, title).Scan(&id); err != nil {
			t.Fatalf("create fleet: %v", err)
		}
		exec(`INSERT INTO fleet_report_state (op_id, computed_at) VALUES ($1, now())`, id)
		return id
	}
	battle := func(op int64, seq int) int64 {
		t.Helper()
		var id int64
		if err := conn.QueryRow(`INSERT INTO fleet_battles (op_id, seq, started_at, ended_at, computed_at)
			VALUES ($1, $2, now() - interval '2 days', now() - interval '2 days', now()) RETURNING id`, op, seq).Scan(&id); err != nil {
			t.Fatalf("create battle: %v", err)
		}
		return id
	}
	old := newFleet("Built before the explorer")
	battle(old, 0)
	mixed := newFleet("One battle with an explorer, one without")
	exec(`INSERT INTO fleet_battle_detail (fleet_battle_id, payload) VALUES ($1, '{}')`, battle(mixed, 0))
	battle(mixed, 1)
	done := newFleet("Every battle has its explorer")
	exec(`INSERT INTO fleet_battle_detail (fleet_battle_id, payload) VALUES ($1, '{}')`, battle(done, 0))
	exec(`DELETE FROM schema_migrations WHERE version = 173`)
	conn.Close()
	pool.Close()

	conn, pool, err = Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	queued := func(op int64) bool {
		t.Helper()
		var dirty bool
		if err := conn.QueryRow(`SELECT dirty_seq IS NOT NULL FROM fleet_report_state WHERE op_id = $1`, op).Scan(&dirty); err != nil {
			t.Fatalf("read fleet %d: %v", op, err)
		}
		return dirty
	}
	if !queued(old) {
		t.Error("a fleet whose battle has no explorer is not queued for a rebuild")
	}
	if !queued(mixed) {
		t.Error("a fleet with one battle lacking an explorer is not queued for a rebuild")
	}
	if queued(done) {
		t.Error("a fleet whose battles all have an explorer is queued for a rebuild")
	}
}
