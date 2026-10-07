package store

// Tests for the schema bootstrap (schema.go): every step is recorded
// in schema_migrations, a database from before that record existed
// is adopted without re-running anything, and a step that fails
// leaves nothing behind.

import (
	"context"
	"database/sql"
	"testing"

	"evesynapse/internal/pgtest"
)

func recordedSchemaSteps(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

func publicTableCount(t *testing.T, conn *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`, table,
	).Scan(&n); err != nil {
		t.Fatalf("look up table %s: %v", table, err)
	}
	return n
}

// TestSchemaStepsRecordedOnce: a fresh database records every
// step, and opening it again records nothing more.
func TestSchemaStepsRecordedOnce(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	want := len(schemaSteps())
	for pass := 1; pass <= 2; pass++ {
		conn, pool, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		got := recordedSchemaSteps(t, conn)
		conn.Close()
		pool.Close()
		if got != want {
			t.Fatalf("open %d: %d steps recorded, want %d", pass, got, want)
		}
	}
}

// TestSchemaAdoptsDatabaseWithoutRecord: a database created before
// schema_migrations existed has its tables but no record of them.
// Opening it must record the steps whose objects are already there
// without running them again (re-running the baseline would fail
// on its first CREATE TABLE), and apply only the step it is
// actually missing.
func TestSchemaAdoptsDatabaseWithoutRecord(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Rewind to an install that stopped one step short and never
	// had the record: drop the record and the newest step's table.
	for _, stmt := range []string{`DROP TABLE schema_migrations`, `DROP TABLE clone_names`} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn.Close()
	pool.Close()

	conn, pool, err = Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen without a record: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	if got, want := recordedSchemaSteps(t, conn), len(schemaSteps()); got != want {
		t.Fatalf("%d steps recorded after adoption, want %d", got, want)
	}
	if publicTableCount(t, conn, "clone_names") != 1 {
		t.Fatal("the missing step's table was not created on reopen")
	}
}

// TestSchemaStepIsAtomic: a step is all-or-nothing. When one of
// its statements fails, the ones before it roll back with it and
// the step is not recorded, so the next start runs it again from
// the top instead of mistaking a half-built schema for a finished
// one.
func TestSchemaStepIsAtomic(t *testing.T) {
	ctx := context.Background()
	conn, pool, err := Open(ctx, pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()
	defer conn.Close()

	broken := schemaStep{
		version: 9001,
		name:    "atomic_probe",
		script: `CREATE TABLE schema_atomic_probe (id BIGINT PRIMARY KEY);
CREATE TABLE schema_atomic_probe (id BIGINT PRIMARY KEY);`,
	}
	if err := applySchemaStep(ctx, conn, broken); err == nil {
		t.Fatal("a step with a failing statement reported success")
	}
	if publicTableCount(t, conn, "schema_atomic_probe") != 0 {
		t.Fatal("the failed step left its first table behind")
	}
	var recorded int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 9001`).Scan(&recorded); err != nil {
		t.Fatalf("count record: %v", err)
	}
	if recorded != 0 {
		t.Fatal("the failed step was recorded as applied")
	}

	// Repaired, the same step applies and is recorded; applying it
	// again is a no-op (running it twice would fail on the table).
	fixed := broken
	fixed.script = `CREATE TABLE schema_atomic_probe (id BIGINT PRIMARY KEY);`
	for pass := 1; pass <= 2; pass++ {
		if err := applySchemaStep(ctx, conn, fixed); err != nil {
			t.Fatalf("apply %d of the repaired step: %v", pass, err)
		}
	}
	if publicTableCount(t, conn, "schema_atomic_probe") != 1 {
		t.Fatal("the repaired step did not create its table")
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 9001`).Scan(&recorded); err != nil {
		t.Fatalf("count record: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("repaired step recorded %d times, want 1", recorded)
	}
}

// userOwnedTables are the per-user tables step 007 ties to users.
var userOwnedTables = []struct {
	name   string
	insert string // inserts one row for user $1
}{
	{"local_fittings", `INSERT INTO local_fittings (user_id, name) VALUES ($1, 'fit')`},
	{"restock_targets", `INSERT INTO restock_targets (user_id, type_id) VALUES ($1, 34)`},
	{"clone_names", `INSERT INTO clone_names (user_id, character_id, clone_id) VALUES ($1, 90000001, 1)`},
	{"wallet_history", `INSERT INTO wallet_history (user_id, character_id, day, balance, sampled_at) VALUES ($1, 0, '2026-10-01', 0, '2026-10-01T00:00:00Z')`},
}

func rowsForUser(t *testing.T, conn *sql.DB, table string, userID int64) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return n
}

// TestSchemaUserForeignKeys: a row in a per-user table needs a user
// that exists, and goes when that user does.
func TestSchemaUserForeignKeys(t *testing.T) {
	ctx := context.Background()
	conn, pool, err := Open(ctx, pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()
	defer conn.Close()

	const nobody = int64(424242)
	var userID int64
	if err := conn.QueryRow(`INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, table := range userOwnedTables {
		if _, err := conn.Exec(table.insert, nobody); err == nil {
			t.Errorf("%s accepted a row for a user that does not exist", table.name)
		}
		if _, err := conn.Exec(table.insert, userID); err != nil {
			t.Fatalf("%s: insert for a real user: %v", table.name, err)
		}
	}
	if _, err := conn.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	for _, table := range userOwnedTables {
		if n := rowsForUser(t, conn, table.name, userID); n != 0 {
			t.Errorf("%s kept %d row(s) of a deleted user", table.name, n)
		}
	}
}

// TestSchemaForeignKeyStepCleansUpOnUpgrade: a database from before
// step 007 may hold rows whose user is gone. The step removes those,
// keeps everything else, and leaves the constraint in place.
func TestSchemaForeignKeyStepCleansUpOnUpgrade(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Rewind to before the step: no constraints, no record of it.
	for _, table := range userOwnedTables {
		if _, err := conn.Exec(`ALTER TABLE ` + table.name + ` DROP CONSTRAINT ` + table.name + `_user_id_fkey`); err != nil {
			t.Fatalf("drop %s constraint: %v", table.name, err)
		}
	}
	if _, err := conn.Exec(`DELETE FROM schema_migrations WHERE version = 7`); err != nil {
		t.Fatalf("forget step 7: %v", err)
	}
	const nobody = int64(424242)
	var userID int64
	if err := conn.QueryRow(`INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, table := range userOwnedTables {
		for _, owner := range []int64{nobody, userID} {
			if _, err := conn.Exec(table.insert, owner); err != nil {
				t.Fatalf("%s: seed row for user %d: %v", table.name, owner, err)
			}
		}
	}
	conn.Close()
	pool.Close()

	conn, pool, err = Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	for _, table := range userOwnedTables {
		if n := rowsForUser(t, conn, table.name, nobody); n != 0 {
			t.Errorf("%s still holds %d row(s) of a user that does not exist", table.name, n)
		}
		if n := rowsForUser(t, conn, table.name, userID); n != 1 {
			t.Errorf("%s holds %d row(s) of the real user, want 1", table.name, n)
		}
		if _, err := conn.Exec(table.insert, nobody); err == nil {
			t.Errorf("%s accepted an ownerless row after the step", table.name)
		}
	}
}
