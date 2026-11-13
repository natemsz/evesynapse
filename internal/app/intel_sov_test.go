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
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38), (30004760, 'T5ZI-S', 10000060, -0.4)`,
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
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000001, now.Add(-30*24*time.Hour))+`]}`, "a", now.Add(-10*time.Minute))
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
	snapshot(esi.GlobalSovMap, `{"solar_systems":[`+claim(30004759, 99000001, now.Add(-30*24*time.Hour))+`,`+claim(30004760, 99000002, now.Add(-2*time.Hour))+`]}`, "b", now.Add(-5*time.Minute))
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

	snapshot(esi.GlobalSovCampaigns, `[{"campaign_id":7,"solar_system_id":30004759,"constellation_id":1,"structure_id":2,"event_type":"ihub_defense","defender_id":99000001,"defender_score":0.3,"attackers_score":0.7,"start_time":"`+now.Add(-20*time.Minute).Format(time.RFC3339)+`"}]`, "c", now)
	if !app.refreshSovCampaigns(ctx) {
		t.Fatal("campaigns mirror stored nothing")
	}

	if _, err := app.db.ExecContext(ctx, `INSERT INTO entity_names (kind, id, name) VALUES ('alliance', 99000001, 'Old Holders'), ('alliance', 99000002, 'New Holders')`); err != nil {
		t.Fatal(err)
	}
	code, body := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/intel/sov/")
	if code != http.StatusOK {
		t.Fatalf("GET /intel/sov/: %d", code)
	}
	for _, want := range []string{
		`<span class="l-stat-label">Live contests</span><span class="l-stat-value">1</span>`,
		`<span class="l-stat-label">Changed hands, 24h</span><span class="l-stat-value">1</span>`,
		`value="30" aria-label="Defender 30 percent, attackers 70 percent"`,
		"1DQ1-A", "T5ZI-S", "New Holders", "Old Holders", "Capital",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tracker page lacks %q", want)
		}
	}
}
