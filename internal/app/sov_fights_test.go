package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"net/http"
)

// TestSovFightKills: a contest becomes a fight whose kills are the
// public player kills the defender took part in, in its constellation
// from an hour before the timer, each marked with the defender's side;
// a kill outside the constellation or before the window, or between
// third parties, stays out; a kill landing at
// ingest joins the open fight; the tracker shows the fight's figures
// and its panel lists the fight's kills only.
func TestSovFightKills(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := app.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const defender = 99000001
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000060, 'Delve')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, constellation_id, security) VALUES
		(30004759, '1DQ1-A', 10000060, 20000696, -0.38), (30004760, 'T5ZI-S', 10000060, 20000696, -0.4),
		(30004770, 'PNQY-Y', 10000060, 20000697, -0.3)`)
	exec(`INSERT INTO sov_campaigns (campaign_id, system_id, constellation_id, structure_id, event_type, defender_id, defender_score, attackers_score, start_time, updated_at)
		VALUES (7, 30004759, 20000696, 1, 'ihub_defense', $1, 0.4, 0.6, $2, $3)`, defender, now.Add(-30*time.Minute), now)
	kill := func(id, system int64, at time.Time, victimAlliance int64, value float64) {
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, region_id, victim_character_id, victim_alliance_id, ship_type_id, source, public, ingested_at, value_ours)
			VALUES ($1, $2, $3, 10000060, $6, $4, 587, 'zkb', true, $2, $5)`, id, at, system, victimAlliance, value, id+1000)
	}
	kill(1, 30004760, now.Add(-20*time.Minute), defender, 3e8) // the defender loses a ship next door
	kill(2, 30004759, now.Add(-50*time.Minute), 99000009, 1e8) // the defender kills one, in the lead hour
	exec(`INSERT INTO kill_involvement (killmail_id, character_id, corporation_id, alliance_id) VALUES (2, 5001, 98000001, $1)`, defender)
	kill(3, 30004759, now.Add(-2*time.Hour), defender, 1e8)    // before the window
	kill(4, 30004770, now.Add(-10*time.Minute), defender, 1e8) // another constellation
	kill(6, 30004760, now.Add(-15*time.Minute), 99000009, 1e8) // third parties, the defender on neither side

	app.syncSovFights(ctx)
	sides := map[int64]int16{}
	rows, err := app.pool.Query(ctx, `SELECT killmail_id, side FROM sov_fight_kills WHERE campaign_id = 7`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var side int16
		if err := rows.Scan(&id, &side); err != nil {
			t.Fatal(err)
		}
		sides[id] = side
	}
	rows.Close()
	if len(sides) != 2 || sides[1] != 1 || sides[2] != 2 {
		t.Fatalf("fight kills %v; want kill 1 as a defender loss and kill 2 as a defender kill, nothing else (not the third-party kill 6)", sides)
	}

	// A kill at ingest joins the open fight.
	k, err := app.queries.GetKill(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	k.KillmailID, k.KillmailTime, k.VictimAllianceID = 5, now, 99000009
	kill(5, 30004759, now, 99000009, 2e8)
	app.fileSovFightKill(ctx, k, []int64{defender})
	app.sweepSovFights(ctx)
	var kills, lost, scored int64
	if err := app.pool.QueryRow(ctx, `SELECT kills, defender_lost, defender_killed FROM sov_fights WHERE campaign_id = 7`).Scan(&kills, &lost, &scored); err != nil {
		t.Fatal(err)
	}
	if kills != 3 || lost != 1 || scored != 2 {
		t.Fatalf("fight figures: %d kills, defender lost %d, scored %d; want 3, 1, 2", kills, lost, scored)
	}

	exec(`INSERT INTO place_stats_meta (kind, fetched_at, cached_until) VALUES ('sovereignty', $1, $1) ON CONFLICT DO NOTHING`, now)
	code, body := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/intel/sov/")
	if code != http.StatusOK {
		t.Fatalf("tracker: %d", code)
	}
	for _, want := range []string{`<b data-fight-kills>3</b>`, `href="/intel/kills/?fight=7" data-fight-open="7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("tracker lacks %q", want)
		}
	}
	code, body = getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/intel/sov/fight/7/")
	if code != http.StatusOK {
		t.Fatalf("fight panel: %d", code)
	}
	for _, want := range []string{`data-kill-id="5"`, `data-kill-id="2"`, `data-kill-id="1"`, `Defender lost`, `Defender scored`} {
		if !strings.Contains(body, want) {
			t.Errorf("fight panel lacks %q", want)
		}
	}
	if strings.Contains(body, `data-kill-id="3"`) || strings.Contains(body, `data-kill-id="4"`) || strings.Contains(body, `data-kill-id="6"`) {
		t.Error("fight panel shows a kill outside the fight")
	}
}
