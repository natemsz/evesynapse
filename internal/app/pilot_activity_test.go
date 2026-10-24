package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestPilotActivityAggregates folds a fixture set of kills, in
// zKillboard's history shape (the ESI killmail with its zkb
// metadata), into one pilot's activity profile and checks the
// stored aggregates: a wrong total here would misstate a pilot's
// record on their page, silently. Folding the same kills again
// must count nothing twice, and the live stream's fold must pass
// over pilots who have no profile.
func TestPilotActivityAggregates(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 1.0), (30002187, 'Amarr', 10000043, 1.0) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (626, 'Vexor'), (587, 'Rifter'), (22456, 'Sabre') ON CONFLICT DO NOTHING`)

	const pilot = int64(91000001)
	page := `[
	  {
	    "killmail_id": 101, "killmail_time": "2026-10-20T21:15:00Z", "solar_system_id": 30000142,
	    "victim": {"character_id": 92000001, "corporation_id": 98000001, "alliance_id": 99000001, "ship_type_id": 587, "damage_taken": 1200, "items": []},
	    "attackers": [
	      {"character_id": 91000001, "corporation_id": 98000002, "alliance_id": 99000002, "ship_type_id": 626, "weapon_type_id": 2456, "damage_done": 900, "final_blow": true, "security_status": 0.4},
	      {"character_id": 92000002, "corporation_id": 98000002, "alliance_id": 99000002, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 200, "final_blow": false, "security_status": 1.2},
	      {"character_id": 92000003, "corporation_id": 98000003, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 100, "final_blow": false, "security_status": -0.5}
	    ],
	    "zkb": {"hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "totalValue": 15200000.5, "points": 12, "npc": false, "solo": false, "awox": false, "labels": ["loc:highsec"]}
	  },
	  {
	    "killmail_id": 102, "killmail_time": "2026-10-21T03:05:00Z", "solar_system_id": 30002187,
	    "victim": {"character_id": 91000001, "corporation_id": 98000002, "alliance_id": 99000002, "ship_type_id": 22456, "damage_taken": 3400, "items": []},
	    "attackers": [
	      {"character_id": 92000004, "corporation_id": 98000004, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 2000, "final_blow": true, "security_status": 0.1},
	      {"character_id": 92000005, "corporation_id": 98000004, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 1400, "final_blow": false, "security_status": 0.2}
	    ],
	    "zkb": {"hash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "totalValue": 48100000, "points": 20, "npc": false, "solo": false, "awox": false, "labels": ["loc:highsec"]}
	  },
	  {
	    "killmail_id": 103, "killmail_time": "2026-10-22T21:45:00Z", "solar_system_id": 30000142,
	    "victim": {"character_id": 92000006, "corporation_id": 98000006, "alliance_id": 0, "ship_type_id": 587, "damage_taken": 900, "items": []},
	    "attackers": [
	      {"character_id": 92000007, "corporation_id": 98000007, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 500, "final_blow": true, "security_status": 0.0},
	      {"character_id": 91000001, "corporation_id": 98000002, "alliance_id": 99000002, "ship_type_id": 626, "weapon_type_id": 2456, "damage_done": 150, "final_blow": false, "security_status": 0.4},
	      {"character_id": 92000008, "corporation_id": 98000007, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 100, "final_blow": false, "security_status": 0.3},
	      {"character_id": 92000009, "corporation_id": 98000007, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 90, "final_blow": false, "security_status": 0.3},
	      {"character_id": 92000010, "corporation_id": 98000007, "alliance_id": 0, "ship_type_id": 587, "weapon_type_id": 2488, "damage_done": 60, "final_blow": false, "security_status": 0.3}
	    ],
	    "zkb": {"hash": "cccccccccccccccccccccccccccccccccccccccc", "totalValue": 12100000, "points": 9, "npc": false, "solo": false, "awox": false, "labels": ["loc:highsec"]}
	  }
	]`
	var entries []zkillHistoryEntry
	if err := json.Unmarshal([]byte(page), &entries); err != nil {
		t.Fatalf("fixture page does not parse as the history API's shape: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("fixture page: %d entries, want 3", len(entries))
	}

	if err := q.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{CharacterID: pilot, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		applied, err := app.applyPilotKill(ctx, pilot, e.killmailFull, false)
		if err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Fatalf("kill %d was not folded in", e.KillmailID)
		}
	}
	// A second pass (a retried backfill, the same kill arriving on
	// the stream) folds nothing again.
	for _, e := range entries {
		applied, err := app.applyPilotKill(ctx, pilot, e.killmailFull, false)
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatalf("kill %d was folded in twice", e.KillmailID)
		}
	}

	prof, err := q.GetPilotActivityProfile(ctx, pilot)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Kills != 2 || prof.Losses != 1 || prof.FinalBlows != 1 {
		t.Fatalf("profile counts: kills %d, losses %d, final blows %d; want 2, 1, 1", prof.Kills, prof.Losses, prof.FinalBlows)
	}
	if prof.GangTotal != 8 || prof.GangSamples != 2 {
		t.Fatalf("gang: total %d over %d samples; want 8 over 2", prof.GangTotal, prof.GangSamples)
	}
	if prof.UsualShipTypeID != 626 {
		t.Fatalf("usual ship %d; want 626 (Vexor)", prof.UsualShipTypeID)
	}
	if !prof.LastActiveAt.Valid || !prof.LastActiveAt.Time.Equal(time.Date(2026, 10, 22, 21, 45, 0, 0, time.UTC)) {
		t.Fatalf("last active %v; want 2026-10-22 21:45 UTC", prof.LastActiveAt)
	}

	ships, err := q.ListPilotActivityShips(ctx, db.ListPilotActivityShipsParams{CharacterID: pilot, MaxRows: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(ships) != 2 {
		t.Fatalf("ship rows: %d; want 2", len(ships))
	}
	byShip := map[int64]db.ListPilotActivityShipsRow{}
	for _, s := range ships {
		byShip[s.ShipTypeID] = s
	}
	if s := byShip[626]; s.Flown != 2 || s.FinalBlows != 1 || s.Lost != 0 {
		t.Fatalf("Vexor row: %+v; want flown 2, final blows 1, lost 0", s)
	}
	if s := byShip[22456]; s.Flown != 0 || s.Lost != 1 {
		t.Fatalf("Sabre row: %+v; want lost 1", s)
	}

	systems, err := q.ListPilotActivitySystems(ctx, db.ListPilotActivitySystemsParams{CharacterID: pilot, MaxRows: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(systems) != 2 || systems[0].SystemID != 30000142 || systems[0].Events != 2 || systems[1].Events != 1 {
		t.Fatalf("system rows: %+v; want Jita 2, Amarr 1", systems)
	}

	hours, err := q.ListPilotActivityHours(ctx, pilot)
	if err != nil {
		t.Fatal(err)
	}
	byHour := map[int16]int64{}
	for _, h := range hours {
		byHour[h.Hour] = h.Events
	}
	if byHour[21] != 2 || byHour[3] != 1 || len(byHour) != 2 {
		t.Fatalf("hour rows: %v; want 21:00 x2, 03:00 x1", byHour)
	}

	// The dossier's read of the same stored rows.
	v := app.loadPilotActivity(ctx, pilot, app.newKillNames(ctx), time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	if v == nil || !v.Any {
		t.Fatalf("activity view: %+v; want a populated view", v)
	}
	if v.UsualShip != "Vexor" || v.Gang != "4" || v.Kills != 2 || v.Losses != 1 {
		t.Fatalf("activity view: usual %q, gang %q, kills %d, losses %d; want Vexor, 4, 2, 1", v.UsualShip, v.Gang, v.Kills, v.Losses)
	}

	// The stream's fold passes over a pilot with no profile, and
	// maintains one who has.
	stranger := int64(92000099)
	if applied, err := app.applyPilotKill(ctx, stranger, entries[0].killmailFull, true); err != nil || applied {
		t.Fatalf("stream fold for a profileless pilot: applied %v, err %v; want neither", applied, err)
	}
	if _, err := q.GetPilotActivityProfile(ctx, stranger); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stream fold made a profile for a stranger: %v", err)
	}
	const victim = int64(92000001)
	if err := q.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{CharacterID: victim, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if applied, err := app.applyPilotKill(ctx, victim, entries[0].killmailFull, true); err != nil || !applied {
		t.Fatalf("stream fold for the victim: applied %v, err %v; want a fold", applied, err)
	}
	vp, err := q.GetPilotActivityProfile(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}
	if vp.Losses != 1 || vp.Kills != 0 {
		t.Fatalf("victim profile: losses %d, kills %d; want 1, 0", vp.Losses, vp.Kills)
	}
}
