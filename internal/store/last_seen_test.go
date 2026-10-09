package store

import (
	"context"
	"testing"
	"time"

	"evesynapse/internal/pgtest"
)

// TestLastSeenStep: step 023 gives the accounts that already exist a
// last-seen time just over a day back (dormant, not asleep), and a new
// account starts as seen now.
func TestLastSeenStep(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	old := openBeforeSchemaStep(t, dsn, 23)
	if _, err := old.ExecContext(ctx, `INSERT INTO users DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	conn, pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); conn.Close() })
	if _, err := conn.ExecContext(ctx, `INSERT INTO users DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT last_seen_at FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ages []time.Duration
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		ages = append(ages, time.Since(at))
	}
	if len(ages) != 2 {
		t.Fatalf("%d accounts, want 2", len(ages))
	}
	if ages[0] < 24*time.Hour || ages[0] > 26*time.Hour {
		t.Errorf("an account from before the step was last seen %v ago, want about 25 hours", ages[0])
	}
	if ages[1] > time.Minute || ages[1] < -time.Minute {
		t.Errorf("a new account was last seen %v ago, want now", ages[1])
	}
}
