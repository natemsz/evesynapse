package app

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestKillLeaders: a period's ranks add up its days' totals, the 24
// hours rank the rolling window, and padding never counts: a loss
// under 1M ISK, a ship its own pilot helped kill, and a kill inside
// one player corporation.
func TestKillLeaders(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := app.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, constellation_id, security) VALUES (30004759, '1DQ1-A', 10000060, 20000696, -0.38)`)
	kill := func(id int64, at time.Time, victim, victimCorp int64, value float64, attackers ...[2]int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, region_id, victim_character_id, victim_corporation_id, victim_alliance_id, ship_type_id, attacker_count, source, public, ingested_at, value_ours)
			VALUES ($1, $2, 30004759, 10000060, $3, $4, 99000009, 587, $5, 'zkill', true, $2, $6)`, id, at, victim, victimCorp, len(attackers), value)
		for i, a := range attackers {
			exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow) VALUES ($1, $2, $3, $4, 99000001, 626, $5)`,
				id, i, a[0], a[1], i == 0)
		}
	}
	const pilot, corp = 91000001, 98000001
	kill(1, now.Add(-time.Hour), 92000001, 98000009, 5e8, [2]int64{pilot, corp})
	kill(2, today.AddDate(0, 0, -3).Add(12*time.Hour), 92000002, 98000009, 2e8, [2]int64{pilot, corp})
	kill(3, now.Add(-2*time.Hour), 92000003, 98000009, 5e5, [2]int64{pilot, corp})                               // padding: an empty hull
	kill(4, now.Add(-3*time.Hour), 92000004, 98000009, 3e8, [2]int64{pilot, corp}, [2]int64{92000004, 98000009}) // padding: its own pilot shot it
	kill(5, now.Add(-4*time.Hour), 92000005, corp, 3e8, [2]int64{pilot, corp})                                   // padding: inside one corporation

	week, day := killLeaderPeriods[1], killLeaderPeriods[0]
	for d := 0; d < 7; d++ {
		from := today.AddDate(0, 0, -d)
		until := from.Add(24 * time.Hour)
		if d == 0 {
			until = now
		}
		if _, err := app.totalKillLeaders(ctx, from, from, until); err != nil {
			t.Fatal(err)
		}
	}
	days := make([]time.Time, 7)
	for d := range days {
		days[d] = today.AddDate(0, 0, -d)
	}
	if err := app.rankKillLeaders(ctx, week, days, days[6], now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := app.totalKillLeaders(ctx, killLeadersRolling, now.Add(-24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := app.rankKillLeaders(ctx, day, []time.Time{killLeadersRolling}, now.Add(-24*time.Hour), now, 0); err != nil {
		t.Fatal(err)
	}

	board := func(period, kind string, id int64, b string) []db.ListKillLeadersRow {
		t.Helper()
		rows, err := app.queries.ListKillLeaders(ctx, db.ListKillLeadersParams{Period: period, ScopeKind: kind, ScopeID: id, Board: b, MaxRows: 10})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := board("week", "", 0, "character"); len(rows) != 1 || rows[0].EntityID != pilot || rows[0].Kills != 2 || rows[0].Isk != 7e8 {
		t.Fatalf("week's pilots: %+v; want the pilot with 2 kills, 700M ISK (padding left out)", rows)
	}
	if rows := board("week", "corporation", corp, "character"); len(rows) != 1 || rows[0].Kills != 2 {
		t.Fatalf("week's corporation members: %+v; want the pilot with 2 kills", rows)
	}
	if rows := board("week", "", 0, "kill"); len(rows) != 2 || rows[0].EntityID != 1 {
		t.Fatalf("week's biggest kills: %+v; want kills 1 then 2", rows)
	}
	if rows := board("day", "", 0, "character"); len(rows) != 1 || rows[0].Kills != 1 || rows[0].Isk != 5e8 {
		t.Fatalf("24 hours' pilots: %+v; want the pilot with 1 kill, 500M ISK", rows)
	}
}
