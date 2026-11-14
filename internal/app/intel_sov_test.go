package app

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestSovTracker: the sovereignty pass stores each claim's detail and,
// replacing the holder set, logs every system that changed holder
// with both holders; the campaigns mirror stores the live contests;
// the tracker page shows the contest, the claim with the holder it
// was taken from, and the summary tiles.
func TestSovTracker(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000060, 'Delve')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38), (30004760, 'T5ZI-S', 10000060, -0.4), (30004761, 'NOL-M9', 10000060, -0.35), (30004762, 'F-TE1T', 10000060, -0.3), (30004763, 'Y-2ANO', 10000060, -0.3)`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := func(kind, payload, etag string, at time.Time) {
		t.Helper()
		if err := app.queries.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
			Kind: kind, Payload: payload, FetchedAt: at, CachedUntil: at.Add(5 * time.Minute), Etag: etag,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(system, alliance int64, since time.Time) string {
		return `{"solar_system_id":` + strconv.FormatInt(system, 10) + `,"claim":{"alliance":{"alliance_id":` + strconv.FormatInt(alliance, 10) +
			`,"corporation_id":98000001,"claimed_since":"` + since.Format(time.RFC3339) + `","sovereignty_hub":{"id":1,"vulnerability_window":{"start":"` +
			now.Add(-time.Hour).Format(time.RFC3339) + `","end":"` + now.Add(2*time.Hour).Format(time.RFC3339) +
			`"}},"is_capital_system":true,"development":{"activity_defense_multiplier":1.4,"military_level":1,"industrial_level":2,"strategic_level":3}}}}`
	}

	// First pass: the first stored set logs nothing.
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004761, 99000003, now.Add(-30*24*time.Hour))+`,`+claim(30004763, 99000003, now.Add(-30*24*time.Hour))+`]}`, "a", now.Add(-10*time.Minute))
	if !app.refreshPlaceSovereignty(ctx) {
		t.Fatal("first sovereignty pass stored nothing")
	}
	var adm float64
	var capital bool
	var vulnEnd sql.NullTime
	if err := app.db.QueryRowContext(ctx, `SELECT adm, is_capital, vuln_end FROM system_sovereignty WHERE system_id = 30004759`).Scan(&adm, &capital, &vulnEnd); err != nil {
		t.Fatal(err)
	}
	if adm != 1.4 || !capital || !vulnEnd.Valid {
		t.Fatalf("claim detail: adm %v capital %v vuln_end %v; want 1.4, true, set", adm, capital, vulnEnd)
	}

	// Second pass: T5ZI-S changes hands.
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000002, now.Add(-2*time.Hour))+`,`+claim(30004761, 99000003, now.Add(-30*24*time.Hour))+`,`+claim(30004763, 99000003, now.Add(-30*24*time.Hour))+`]}`, "b", now.Add(-5*time.Minute))
	if !app.refreshPlaceSovereignty(ctx) {
		t.Fatal("second sovereignty pass stored nothing")
	}
	var system, oldA, newA int64
	var n int
	if err := app.db.QueryRowContext(ctx, `SELECT count(*), max(system_id), max(old_alliance_id), max(new_alliance_id) FROM sov_changes`).Scan(&n, &system, &oldA, &newA); err != nil {
		t.Fatal(err)
	}
	if n != 1 || system != 30004760 || oldA != 99000001 || newA != 99000002 {
		t.Fatalf("change log: %d rows, system %d, %d to %d; want 1 row, 30004760, 99000001 to 99000002", n, system, oldA, newA)
	}

	// Third pass: NOL-M9's holder drops it; the system sits unclaimed.
	// Y-2ANO changes hands.
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000002, now.Add(-2*time.Hour))+`,`+claim(30004763, 99000002, now.Add(-3*time.Hour))+`]}`, "d", now.Add(-4*time.Minute))
	if !app.refreshPlaceSovereignty(ctx) {
		t.Fatal("third sovereignty pass stored nothing")
	}

	// Fourth pass: the second alliance claims NOL-M9. The claim row
	// records no previous holder; the previous holder is the alliance
	// from the drop. Y-2ANO's new holder drops it.
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000002, now.Add(-2*time.Hour))+`,`+claim(30004761, 99000002, now.Add(-30*time.Minute))+`]}`, "e", now.Add(-3*time.Minute))
	if !app.refreshPlaceSovereignty(ctx) {
		t.Fatal("fourth sovereignty pass stored nothing")
	}

	// Fifth pass: Y-2ANO's holder, which dropped it in the fourth
	// pass, claims it back. A reclaim has no previous holder.
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000002, now.Add(-2*time.Hour))+`,`+claim(30004761, 99000002, now.Add(-30*time.Minute))+`,`+claim(30004763, 99000002, now.Add(-20*time.Minute))+`]}`, "f", now.Add(-2*time.Minute))
	if !app.refreshPlaceSovereignty(ctx) {
		t.Fatal("fifth sovereignty pass stored nothing")
	}

	// A claim the change log predates has no previous holder.
	if _, err := app.db.ExecContext(ctx, `INSERT INTO system_sovereignty (system_id, alliance_id, corporation_id, faction_id, faction_name, claimed_since, updated_at) VALUES (30004762, 99000002, 98000001, 0, '', $1, $1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	claims, err := app.queries.ListSovRecentClaims(ctx, db.ListSovRecentClaimsParams{Since: now.Add(-24 * time.Hour), MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	prev := map[int64]int64{}
	logged := map[int64]bool{}
	for _, c := range claims {
		prev[c.SystemID] = c.PrevAllianceID
		logged[c.SystemID] = c.Logged
	}
	if prev[30004760] != 99000001 {
		t.Errorf("direct flip: T5ZI-S previous holder %d; want 99000001", prev[30004760])
	}
	if prev[30004761] != 99000003 {
		t.Errorf("drop then claim: NOL-M9 previous holder %d; want 99000003 (the drop)", prev[30004761])
	}
	if prev[30004763] != 0 {
		t.Errorf("reclaim after its own drop: Y-2ANO previous holder %d; want 0", prev[30004763])
	}
	if prev[30004762] != 0 || logged[30004762] {
		t.Errorf("predates the log: F-TE1T previous holder %d, logged %v; want 0, false", prev[30004762], logged[30004762])
	}
	if _, err := app.db.ExecContext(ctx, `DELETE FROM system_sovereignty WHERE system_id = 30004762`); err != nil {
		t.Fatal(err)
	}

	snapshot(esi.GlobalSovCampaigns, `[{"campaign_id":7,"solar_system_id":30004759,"constellation_id":1,"structure_id":2,"event_type":"ihub_defense","defender_id":99000001,"defender_score":0.3,"attackers_score":0.7,"start_time":"`+now.Add(-20*time.Minute).Format(time.RFC3339)+`"}]`, "c", now)
	if !app.refreshSovCampaigns(ctx) {
		t.Fatal("campaigns mirror stored nothing")
	}

	if _, err := app.db.ExecContext(ctx, `INSERT INTO entity_names (kind, id, name) VALUES ('alliance', 99000001, 'Old Holders'), ('alliance', 99000002, 'New Holders'), ('alliance', 99000003, 'First Keepers')`); err != nil {
		t.Fatal(err)
	}
	code, body := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/intel/sov/")
	if code != http.StatusOK {
		t.Fatalf("GET /intel/sov/: %d", code)
	}
	for _, want := range []string{
		`<span class="l-stat-label">Live contests</span><span class="l-stat-value">1</span>`,
		`<span class="l-stat-label">Changed hands, 24h</span><span class="l-stat-value">3</span>`,
		`value="30" aria-label="Defender 30 percent, attackers 70 percent"`,
		"1DQ1-A", "T5ZI-S", "NOL-M9", "New Holders", "Old Holders", "First Keepers", "Unclaimed", "Capital",
		`</span><a class="map-jump" href="/map/?system=30004759"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tracker page lacks %q", want)
		}
	}
}
