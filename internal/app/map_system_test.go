package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestMapSystem: the map card's kills and camp for one system. The
// latest public kills come newest first, with the killboard link for
// the rest; a camp on now is named as the threat layer names it, with
// the campers' ships and pilots and their victims; a private kill
// never shows.
func TestMapSystem(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	const rancer = int64(30003504)
	now := time.Now().UTC()
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30003504, 'Rancer', 10000048, 0.4) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (22456, 'Sabre'), (587, 'Rifter'), (17738, 'Machariel') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_celestials (item_id, system_id, name) VALUES (50000001, 30003504, 'Stargate (Otou)') ON CONFLICT DO NOTHING`)
	kill := func(id int64, ago time.Duration, victimCorp int64, public bool, alliance int64, loc int64, attackers ...int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, location_id, source, public, ingested_at, value_zkill)
			VALUES ($1, $2, $3, $4, $5, 587, $7, 'stream', $6, now(), 10000000)`, id, now.Add(-ago), rancer, 92000000+id, victimCorp, public, loc)
		for i, a := range attackers {
			ship := int64(22456)
			if i == 1 {
				ship = 17738
			}
			exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow) VALUES ($1, $2, $3, 98000001, $4, $5, $6)`,
				id, i, a, alliance, ship, i == 0)
		}
	}
	// A camp: five kills over forty minutes on passers-by, the same
	// alliance on four, all four on the Otou gate. One private
	// kill, never shown.
	kill(10, 5*time.Minute, 1, true, 99000001, 50000001, 91000001, 91000002)
	kill(11, 15*time.Minute, 2, true, 99000001, 50000001, 91000001)
	kill(12, 25*time.Minute, 3, true, 99000001, 50000001, 91000003)
	kill(13, 35*time.Minute, 4, true, 99000002, 50000002, 91000009)
	kill(14, 40*time.Minute, 5, true, 99000001, 50000001, 91000001)
	kill(15, 2*time.Minute, 6, false, 99000001, 50000001, 91000001)
	app.esi.StoreCharacterName(91000001, "Camp Regular")

	code, body := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/map/system.json?system=30003504")
	if code != http.StatusOK {
		t.Fatalf("GET /map/system.json: %d %s", code, body)
	}
	var d mapSystemData
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(d.Kills) != 5 || d.Kills[0].ID != 10 || d.Kills[0].Ship != "Rifter" {
		t.Errorf("kills = %+v, want the five public ones, newest (10) first", d.Kills)
	}
	for _, k := range d.Kills {
		if k.ID == 15 {
			t.Errorf("a private kill is listed")
		}
	}
	if d.More != "/intel/kills/?system=30003504" {
		t.Errorf("more = %q", d.More)
	}
	c := d.Camp
	if c == nil {
		t.Fatalf("no camp in Rancer: %s", body)
	}
	if c.Kind != "alliance" || c.GroupID != 99000001 || c.Kills != 4 || c.Href != "/intel/kills/?alliance=99000001" {
		t.Errorf("camp = %+v", c)
	}
	if c.Pilots != 3 {
		t.Errorf("camp pilots = %d, want 3 (91000001-3)", c.Pilots)
	}
	// Composition counts distinct pilots per ship, not involvements:
	// 91000001 flew the Sabre on four of the camp's kills and
	// 91000003 on one, so the Sabre is x2, not x5; 91000002's
	// Machariel is x1.
	if len(c.Ships) != 2 || c.Ships[0].Name != "Sabre" || c.Ships[0].N != 2 || c.Ships[1].Name != "Machariel" || c.Ships[1].N != 1 {
		t.Errorf("camp ships = %+v, want Sabre x2 then Machariel x1 (distinct pilots per ship)", c.Ships)
	}
	if len(c.Victims) != 4 {
		t.Errorf("camp victims = %d, want the alliance's 4", len(c.Victims))
	}
	// The camp sits on one gate, and the pilot on three of its
	// four kills is named as its recurring crew.
	if c.Celestial != "Stargate (Otou)" || c.Spread {
		t.Errorf("camp place = %q, spread %v; want the Otou gate named", c.Celestial, c.Spread)
	}
	if len(c.Crew) != 1 || c.Crew[0] != "Camp Regular" {
		t.Errorf("camp crew = %v, want [Camp Regular]", c.Crew)
	}

	// A quiet system: no kills, no camp.
	_, body = getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/map/system.json?system=30000142")
	var quiet mapSystemData
	if err := json.Unmarshal([]byte(body), &quiet); err != nil || len(quiet.Kills) != 0 || quiet.Camp != nil {
		t.Errorf("a quiet system: %s", body)
	}
	if code, _ := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/map/system.json?system=nope"); code != http.StatusBadRequest {
		t.Errorf("a bad system: %d", code)
	}
}
