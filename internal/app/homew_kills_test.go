package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestHomeKillValuesAreOurOwnAppraisalOnly: the home's Kills and Losses
// figures add up EveSynapse's own appraisal only. A kill with only an
// outside value counts as not valued yet and adds no ISK.
func TestHomeKillValuesAreOurOwnAppraisalOnly(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{730001, 730002} {
		ts := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
		raw := fmt.Sprintf(`{"killmail_id":%d,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":900,"items":[]},
"attackers":[{"character_id":9002,"corporation_id":98000002,"alliance_id":99000002,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`, id, ts)
		if _, err := app.ingestKill(ctx, json.RawMessage(raw), "h", nil, "esi", true, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`UPDATE kills SET value_ours = 200000000, value_zkill = 900000000 WHERE killmail_id = 730001`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE kills SET value_ours = NULL, value_zkill = 500000000 WHERE killmail_id = 730002`); err != nil {
		t.Fatal(err)
	}

	tot, _ := app.killsWindow(ctx, []int64{9001}, time.Now(), 7)
	if tot.Losses != 2 || tot.lst != 200000000 || tot.Unpriced != 1 {
		t.Fatalf("7-day totals: %d losses, %.0f ISK lost, %d not valued; want 2, 200000000 (ours only), 1", tot.Losses, tot.lst, tot.Unpriced)
	}
	rows, err := q.ListAccountLatestKills(ctx, db.ListAccountLatestKillsParams{CharacterIds: []int64{9001}, RowLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if want := map[int64]float64{730001: 200000000, 730002: 0}[r.KillmailID]; r.Isk != want {
			t.Fatalf("latest kill %d ISK = %.0f, want %.0f", r.KillmailID, r.Isk, want)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("latest kills = %d rows, want 2", len(rows))
	}
}
