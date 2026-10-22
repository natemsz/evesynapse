package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/intelanalyze"
)

// TestIntelSituations: two scouts on one camp are one situation, with
// its danger worked out from the kind, the Sabre, the pilot's kills
// this week and the second scout, shown on the channel page and the
// map. With the owner's standings on, a corpmate the owner has set
// at -10 is hostile and an outsider set at +10 is friendly.
func TestIntelSituations(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (541, 'Interdictor', 6) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, published) VALUES (22456, 'Sabre', 541, 1) ON CONFLICT DO NOTHING`)
	const corp = int64(98000401)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000401, "Channel Owner")
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: 90000401, CorporationID: corp, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, app, u.ID, 90000401, "Channel Owner")
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	base := "/intel/channels/" + strconv.FormatInt(cid, 10) + "/"
	now := time.Now().UTC()
	name := func(lower string, id int64, proper string, corpID int64) {
		t.Helper()
		_ = q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: lower, CharacterID: id, Name: proper, CheckedAt: now})
		_ = q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: id, CorporationID: corpID, CheckedAt: now})
	}
	name("hostile one", 91000401, "Hostile One", 98000999)
	name("mate pilot", 91000402, "Mate Pilot", corp)
	name("blue guy", 91000403, "Blue Guy", 98000888)
	// Hostile One has been busy: six PvP kills this week.
	for k := int64(1); k <= 6; k++ {
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, source, public, ingested_at) VALUES ($1, $2, 30004759, 'stream', true, now())`, 7000+k, now.Add(-time.Duration(k)*time.Hour))
		exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, ship_type_id, final_blow) VALUES ($1, 0, 91000401, 22456, true)`, 7000+k)
	}
	post := func(path string, form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	post(base+"report", url.Values{"lines": {"Scout A > 1DQ1-A  Hostile One"}})
	post(base+"report", url.Values{"lines": {"Scout B > 1DQ1-A  Hostile One  sabre gate camp"}})
	sits := app.channelSituations(ctx, cid)
	if len(sits) != 1 {
		t.Fatalf("%d situations, want the two reports as one", len(sits))
	}
	s := sits[0]
	if s.Kind != intelanalyze.KindCamp || s.Reporters != 2 || s.Level != "High" || len(s.Pilots) != 1 || s.Pilots[0].Kills != 6 {
		t.Errorf("the camp: %+v", s)
	}
	_, body := getPage(t, app, cookie, base)
	for _, want := range []string{"Right now", "Danger: High", "2 scouts", "6 kills this week"} {
		if !strings.Contains(body, want) {
			t.Errorf("the channel page lacks %q", want)
		}
	}
	_, raw := getPage(t, app, cookie, "/map/intel.json")
	var mapIntel struct {
		Situations []situationView `json:"situations"`
	}
	if err := json.Unmarshal([]byte(raw), &mapIntel); err != nil || len(mapIntel.Situations) != 1 || mapIntel.Situations[0].Level != "High" {
		t.Errorf("the map's situations: %v %s", err, raw)
	}

	// The owner's standings: off, a corpmate is friendly; on, the
	// owner's -10 makes them hostile and +10 makes an outsider friendly.
	seedSnapshot(t, q, 90000401, esi.SnapContacts, esi.Contacts{
		{ContactID: 91000402, ContactType: "character", Standing: -10},
		{ContactID: 98000888, ContactType: "corporation", Standing: 10},
	})
	pilotsOf := func() []intelanalyze.Pilot {
		t.Helper()
		rows, _ := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 1})
		var a intelanalyze.Analysis
		_ = json.Unmarshal([]byte(rows[0].Analysis), &a)
		return a.Pilots
	}
	post(base+"report", url.Values{"lines": {"Scout A > 1DQ1-A  Mate Pilot  Blue Guy"}})
	if p := pilotsOf(); len(p) != 2 || !p[0].Friendly || p[1].Friendly {
		t.Errorf("standings off: the corpmate friendly, the outsider not: %+v", p)
	}
	post(base+"edit", url.Values{"name": {"Home"}, "standings": {"1"}})
	post(base+"report", url.Values{"lines": {"Scout B > 1DQ1-A  Mate Pilot  Blue Guy"}})
	if p := pilotsOf(); len(p) != 2 || p[0].Friendly || !p[0].Red || !p[1].Friendly {
		t.Errorf("standings on: -10 hostile, +10 friendly: %+v", p)
	}
}
