package tests

// The intel workbench's report door (/intel/workbench/{id}/):
// a channel report opened up — its stored scan reading broken
// down, its roster with the pilots' settled records, the diff
// against the channel's previous report in the system, and the
// system's gates. A signed-in account with no access to the
// report's channel gets a 404, not the page.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"evesynapse/internal/apptest"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
)

const wbScanReading = `{"kind":"dscan","system":30004759,` +
	`"dscan":{"items":4,"ships":3,"ships_grid":3,` +
	`"types":[{"id":22456,"name":"Sabre","class":"Interdictor","n":2,"grid":2},{"id":11987,"name":"Guardian","class":"Logistics","n":1,"grid":1}],` +
	`"classes":[{"name":"Interdictor","n":2,"grid":2},{"name":"Logistics","n":1,"grid":1}],` +
	`"deployables":[{"id":17967,"name":"Mobile Warp Disruptor","class":"Mobile Warp Disruptor","n":1,"grid":1}]},` +
	`"local":{"pilots":[` +
	`{"id":2124000401,"name":"Wb Pilot One","corp":98000999,"alliance":98000001},` +
	`{"id":2124000402,"name":"Wb Pilot Two","corp":98000777}],` +
	`"alliances":[` +
	`{"id":98000001,"name":"Fixture Alliance","n":1,"corps":[{"id":98000999,"name":"Fixture Corp","n":1}]},` +
	`{"id":98000777,"name":"Fixture Corp Two","n":1,"corp":true}]}}`

const wbPrevReading = `{"kind":"dscan","system":30004759,` +
	`"dscan":{"items":1,"ships":1,"ships_grid":1,` +
	`"types":[{"id":22456,"name":"Sabre","class":"Interdictor","n":1,"grid":1}],` +
	`"classes":[{"name":"Interdictor","n":1,"grid":1}]},` +
	`"local":{"pilots":[{"id":2124000401,"name":"Wb Pilot One","corp":98000999,"alliance":98000001}],` +
	`"alliances":[{"id":98000001,"name":"Fixture Alliance","n":1,"corps":[{"id":98000999,"name":"Fixture Corp","n":1}]}]}}`

const wbRawPaste = "22456\tAlpha Sabre\tSabre\t850 m\n" +
	"22456\tBeta Sabre\tSabre\t12 km\n" +
	"11987\tLogi One\tGuardian\t9 km\n" +
	"17967\tGate Bubble\tMobile Warp Disruptor\t-\n"

func TestWorkbenchReportPage(t *testing.T) {
	rig := apptest.Build(t, &apptest.CountingTransport{})
	q := rig.Queries()
	conn := rig.DB()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	exec(`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship')`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (420, 'Interdictor', 6), (832, 'Logistics', 6)`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES
		(22456, 'Sabre', 420, 0, 1, 'Interdictor hull.'), (11987, 'Guardian', 832, 0, 1, 'Logistics hull.')`)
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000060, 'Catch')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30004759, '1DQ1-A', 10000060, -0.38), (30004760, 'WB-GATE', 10000060, -0.5)`)
	exec(`INSERT INTO sde_system_jumps (from_system_id, to_system_id) VALUES (30004759, 30004760)`)
	exec(`INSERT INTO system_activity_stats (system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
		ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
		ship_kills_day, pod_kills_day, npc_kills_day, jumps_day, hours_covered, updated_at)
		VALUES (30004759, 27, 14, 62, 148, 0, 0, 0, 0, 0, 0, 0, 0, 24, $1)`, now)
	exec(`INSERT INTO system_activity_stats (system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
		ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
		ship_kills_day, pod_kills_day, npc_kills_day, jumps_day, hours_covered, updated_at)
		VALUES (30004760, 3, 1, 9, 42, 0, 0, 0, 0, 0, 0, 0, 0, 24, $1)`, now)

	owner, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apptest.SeedCharacter(t, q, owner.ID, 90000401, "Wb Owner")
	apptest.SeedCharacter(t, q, outsider.ID, 90000402, "Wb Outsider")
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: owner.ID, Name: "Wb Fixture", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}

	// Settled pilot records: one veteran who changed corps
	// three days ago, one 72-day-old alt.
	joinStart := now.AddDate(0, 0, -3)
	bornOne := now.AddDate(-6, -4, 0)
	payloadOne := fmt.Sprintf(`{"profile":{"name":"Wb Pilot One","birthday":%q,"security_status":-1.25,"corporation_id":98000999,"alliance_id":98000001},`+
		`"corp":{"name":"Fixture Corp","ticker":"FIXC"},"alliance":{"name":"Fixture Alliance","ticker":"FIXA"},`+
		`"history":[{"corp_id":98000666,"corp_name":"Old Fixture Corp","start":%q,"end":%q},{"corp_id":98000999,"corp_name":"Fixture Corp","start":%q,"end":""}]}`,
		apptest.RFC(bornOne), apptest.RFC(bornOne.AddDate(0, 0, 1)), apptest.RFC(joinStart), apptest.RFC(joinStart))
	bornTwo := now.AddDate(0, -2, -10)
	payloadTwo := fmt.Sprintf(`{"profile":{"name":"Wb Pilot Two","birthday":%q,"security_status":0.4,"corporation_id":98000777},`+
		`"corp":{"name":"Fixture Corp Two","ticker":"FIX2"},"alliance":{"name":"","ticker":""},`+
		`"history":[{"corp_id":98000777,"corp_name":"Fixture Corp Two","start":%q,"end":""}]}`,
		apptest.RFC(bornTwo), apptest.RFC(bornTwo))
	exec(`INSERT INTO pilot_records (character_id, payload, state, fetched_at) VALUES (2124000401, $1, 'ready', $2)`, payloadOne, apptest.RFC(now))
	exec(`INSERT INTO pilot_records (character_id, payload, state, fetched_at) VALUES (2124000402, $1, 'ready', $2)`, payloadTwo, apptest.RFC(now))

	// The scans the two reports stand on, then the reports:
	// an older one (one Sabre, one pilot) and the current one.
	for _, s := range []struct{ id, result string }{
		{"wbscan000001", wbPrevReading},
		{"wbscan000002", wbScanReading},
	} {
		if err := q.InsertScan(ctx, db.InsertScanParams{ID: s.id, Kind: "dscan", SystemID: 30004759,
			UserID: owner.ID, GroupID: s.id, Raw: wbRawPaste, Result: s.result, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	insertReport := func(scanID, text string, seenAt time.Time) int64 {
		t.Helper()
		id, err := q.InsertIntelReport(ctx, db.InsertIntelReportParams{
			ChannelID: cid, SystemID: 30004759, AuthorUserID: owner.ID, AuthorCharacterID: 90000401,
			Reporter: "Wb Owner", Text: text, Pilots: text, ScanID: scanID, Source: "scan",
			SeenAt: seenAt, CreatedAt: seenAt})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	_ = insertReport("wbscan000001", "A Sabre on the gate in 1DQ1-A.", now.Add(-time.Hour))
	curID := insertReport("wbscan000002", "Sabres and a Guardian on the gate in 1DQ1-A.", now.Add(-5*time.Minute))
	analysis, err := marshalAnalysis()
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE intel_reports SET analysis = $1 WHERE id = $2`, analysis, curID)

	ownerCookie := apptest.SessionCookie(t, rig, owner.ID, 90000401, "Wb Owner")
	path := fmt.Sprintf("/intel/workbench/%d/", curID)
	code, body := apptest.GetPage(t, rig, ownerCookie, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", path, code)
	}
	apptest.MustContain(t, path, body,
		"1DQ1-A", "Catch",
		"Sabre", "Guardian", "Interdictors / HICs", "Mobile Warp Disruptor",
		"Wb Pilot One", "Wb Pilot Two", "Fixture Alliance", "Fixture Corp [FIXC]",
		"2 corps in history", "Young alt",
		"+ Added:", "Joined",
		"WB-GATE", "148",
		"Paste a scan",
	)

	outCookie := apptest.SessionCookie(t, rig, outsider.ID, 90000402, "Wb Outsider")
	if code, _ := apptest.GetPage(t, rig, outCookie, path); code != http.StatusNotFound {
		t.Errorf("GET %s as a non-member: status %d, want 404", path, code)
	}
	if code, _ := apptest.GetPage(t, rig, ownerCookie, "/intel/workbench/999999/"); code != http.StatusNotFound {
		t.Errorf("GET missing report: status %d, want 404", code)
	}
}

func marshalAnalysis() (string, error) {
	a := intelanalyze.Analysis{Kind: "scan", Counts: &intelanalyze.StandingCounts{Neutral: 2}}
	b, err := json.Marshal(a)
	return string(b), err
}
