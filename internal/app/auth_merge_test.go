package app

import (
	"context"
	"testing"
	"time"
)

// TestAccountMergeRulesCoverEveryAccountColumn: every column that holds
// an account id (a foreign key to users, or a user id by name) has a
// merge rule, and every rule names a real column. A new account table
// without a rule would be silently left behind by a merge and deleted
// with the emptied account.
func TestAccountMergeRulesCoverEveryAccountColumn(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	rows, err := app.pool.Query(context.Background(), `
SELECT c.conrelid::regclass::text, a.attname FROM pg_constraint c
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
WHERE c.contype = 'f' AND c.confrelid = 'users'::regclass
UNION
SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema = 'public' AND table_name <> 'users' AND column_name LIKE '%user_id'`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	inSchema := map[string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan: %v", err)
		}
		inSchema[table+"."+column] = true
	}
	ruled := map[string]bool{}
	for _, r := range accountMergeRules {
		ruled[r.Table+"."+r.Column] = true
		if !inSchema[r.Table+"."+r.Column] {
			t.Errorf("merge rule for %s.%s names no account column in the schema", r.Table, r.Column)
		}
	}
	for col := range inSchema {
		if !ruled[col] {
			t.Errorf("%s holds an account id but has no merge rule in accountMergeRules", col)
		}
	}
}

// TestMergeAccountsMovesEverythingKeptAccountWins: a merge moves the
// source's characters and saved rows to the kept account, keeps the
// kept account's row where both have one, marks the source for
// removal in 30 days, and the removal deletes it once due.
func TestMergeAccountsMovesEverythingKeptAccountWins(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	kept, _ := q.CreateUser(ctx)
	from, _ := q.CreateUser(ctx)
	linkFor(t, app, kept.ID, fixtureCharA, "hash-a")
	linkFor(t, app, from.ID, fixtureCharB, "hash-b")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := app.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO market_watchlist (user_id, type_id, region_id, created_at) VALUES ($1, 34, 1, now()), ($2, 34, 1, now()), ($2, 35, 1, now())`, kept.ID, from.ID)
	exec(`INSERT INTO widget_configs (user_id, widget_id, config, updated_at) VALUES ($1, 'theme', 'kept', now()), ($2, 'theme', 'source', now()), ($2, 'skills', 'source', now())`, kept.ID, from.ID)

	res, err := app.mergeAccounts(ctx, from.ID, kept.ID)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Characters != 1 {
		t.Errorf("moved %d characters, want 1", res.Characters)
	}
	if ch, err := q.GetCharacter(ctx, fixtureCharB); err != nil || ch.UserID != kept.ID {
		t.Errorf("character B is on account %d (err %v), want %d", ch.UserID, err, kept.ID)
	}
	count := func(sql string, args ...any) (n int) {
		t.Helper()
		if err := app.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM market_watchlist WHERE user_id = $1`, kept.ID); n != 2 {
		t.Errorf("kept account has %d watchlist rows, want 2 (its own plus the source's other one)", n)
	}
	var theme string
	if err := app.pool.QueryRow(ctx, `SELECT config FROM widget_configs WHERE user_id = $1 AND widget_id = 'theme'`, kept.ID).Scan(&theme); err != nil || theme != "kept" {
		t.Errorf("kept account's theme is %q (err %v), want its own", theme, err)
	}
	if n := count(`SELECT count(*) FROM widget_configs WHERE user_id = $1 AND widget_id = 'skills'`, kept.ID); n != 1 {
		t.Error("the source's widget the kept account did not have was not moved")
	}

	src, err := q.GetUser(ctx, from.ID)
	if err != nil || !src.DeleteAfter.Valid || src.DeleteAfter.Time.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("source not marked for removal in 30 days: %+v (err %v)", src.DeleteAfter, err)
	}
	if n, _ := q.DeleteDueUsers(ctx, time.Now()); n != 0 {
		t.Errorf("removed %d accounts before they were due", n)
	}
	if n, _ := q.DeleteDueUsers(ctx, time.Now().Add(31*24*time.Hour)); n != 1 {
		t.Errorf("removed %d accounts once due, want 1", n)
	}
	if _, err := q.GetUser(ctx, kept.ID); err != nil {
		t.Errorf("the kept account went too: %v", err)
	}
}
