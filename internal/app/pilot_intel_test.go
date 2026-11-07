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
)

// TestPilotIntel: a pilot's page shows their public kills (private
// ones never), what they fly and where; a signed-in reader also sees
// their channels' reports naming the pilot, adds the pilot to a
// channel's known hostiles, and finds them on the channel page and
// the map's watched layer; a read-only member cannot change the list.
func TestPilotIntel(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	const hostile = int64(91000001)
	now := time.Now().UTC()
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30002187, 'Amarr', 10000043, 1.0) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (22456, 'Sabre'), (587, 'Rifter') ON CONFLICT DO NOTHING`)
	kill := func(id int64, at time.Time, victim int64, ship int64, public bool, attackers ...int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, ship_type_id, source, public, ingested_at, value_zkill)
			VALUES ($1, $2, 30002187, $3, $4, 'stream', $5, now(), 10000000)`, id, at, victim, ship, public)
		for i, a := range attackers {
			exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, ship_type_id, final_blow) VALUES ($1, $2, $3, 22456, $4)`, id, i, a, i == 0)
			exec(`INSERT INTO kill_involvement (killmail_id, character_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, a)
		}
	}
	kill(1, now.Add(-2*time.Hour), 92000001, 587, true, hostile, 91000002)
	kill(2, now.Add(-1*time.Hour), hostile, 22456, true, 92000002)
	kill(3, now.Add(-30*time.Minute), 92000003, 587, false, hostile) // private: never shown

	// A visitor sees the public record.
	_, body := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/pilot/?character="+strconv.FormatInt(hostile, 10))
	for _, want := range []string{"Activity", `<span class="pi-v">1</span><span class="pi-l">kills`, "Sabre", "Amarr", "Flies with"} {
		if !strings.Contains(body, want) {
			t.Errorf("public pilot intel lacks %q", want)
		}
	}
	if strings.Contains(body, "Your intel") {
		t.Errorf("a visitor gets no private intel")
	}

	// With a stored activity profile, the same section reads the
	// profile's aggregates instead: the whole recorded record,
	// usual hull and gang size included, labelled with that scope.
	exec(`INSERT INTO pilot_activity_profiles (character_id, backfilled_at, updated_at, kills, losses, final_blows, gang_total, gang_samples, usual_ship_type_id, last_active_at)
		VALUES ($1, $2, $3, 137, 42, 98, 480, 137, 22456, $3)`, hostile, now.Add(-24*time.Hour), now.Add(-2*time.Hour))
	exec(`INSERT INTO pilot_activity_ships (character_id, ship_type_id, flown, final_blows, lost) VALUES ($1, 22456, 41, 27, 12)`, hostile)
	exec(`INSERT INTO pilot_activity_systems (character_id, system_id, events, last_at) VALUES ($1, 30002187, 88, $2)`, hostile, now.Add(-2*time.Hour))
	exec(`INSERT INTO pilot_activity_hours (character_id, hour, events) VALUES ($1, 21, 5)`, hostile)
	_, body = getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/pilot/?character="+strconv.FormatInt(hostile, 10))
	for _, want := range []string{"every recorded kill and loss", `<span class="pi-v">137</span><span class="pi-l">kills`, `<span class="pi-v">42</span><span class="pi-l">losses`,
		`<span class="pi-v">10m</span><span class="pi-l">ISK destroyed</span>`,
		`<span class="pi-v">Sabre</span><span class="pi-l">usual hull</span>`, `<span class="pi-v">3.5</span><span class="pi-l">typical gang size</span>`,
		`<span class="pi-l">last active</span>`, "41 flown · 27 final blows · 12 lost"} {
		if !strings.Contains(body, want) {
			t.Errorf("profile-backed pilot intel lacks %q", want)
		}
	}

	// A signed-in reader with a channel naming the pilot.
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000501, "Intel Reader")
	_ = q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: 90000501, CorporationID: 98000501, UpdatedAt: now})
	cookie := sessionCookie(t, app, u.ID, 90000501, "Intel Reader")
	cid, _ := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home intel", CreatedAt: now})
	app.esi.StoreCharacterName(hostile, "Bad Guy")
	if _, err := q.InsertIntelReport(ctx, db.InsertIntelReportParams{ChannelID: cid, SystemID: 30002187, AuthorUserID: u.ID, Reporter: "Scout",
		Text: "Bad Guy in a Sabre", Hostiles: 1, Source: "form", SeenAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	page := "/pilot/?character=" + strconv.FormatInt(hostile, 10)
	_, body = getPage(t, app, cookie, page)
	for _, want := range []string{"Your intel", "Named in your channels", "Bad Guy in a Sabre", "Add to known hostiles"} {
		if !strings.Contains(body, want) {
			t.Errorf("reader's pilot intel lacks %q", want)
		}
	}
	post := func(c *http.Cookie, form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/pilot/watch", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c)
		app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	post(cookie, url.Values{"character": {strconv.FormatInt(hostile, 10)}, "channel": {strconv.FormatInt(cid, 10)}, "note": {"Sabre camper"}})
	if _, body = getPage(t, app, cookie, page); !strings.Contains(body, "Known hostile in") || !strings.Contains(body, "Sabre camper") {
		t.Errorf("the pilot is on the channel's known hostiles")
	}
	if _, body = getPage(t, app, cookie, "/intel/channels/"+strconv.FormatInt(cid, 10)+"/"); !strings.Contains(body, "Known hostiles") || !strings.Contains(body, "Bad Guy") || !strings.Contains(body, "Amarr") {
		t.Errorf("the channel page lists the hostile, last seen in Amarr")
	}
	_, body = getPage(t, app, cookie, "/map/mine.json?fresh=1")
	var d minePayload
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Watched) != 1 || d.Watched[0].Sys != 30002187 || d.Watched[0].Note != "Sabre camper" {
		t.Errorf("watched on the map: %+v", d.Watched)
	}

	// A read-only member cannot change the list.
	m, _ := q.CreateUser(ctx)
	seedCharacter(t, q, m.ID, 90000502, "Read Only")
	_ = q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: 90000502, CorporationID: 98000501, UpdatedAt: now})
	_ = q.UpsertIntelChannelGrant(ctx, db.UpsertIntelChannelGrantParams{ChannelID: cid, Kind: "corporation", TargetID: 98000501, CanPost: false, CreatedAt: now})
	post(sessionCookie(t, app, m.ID, 90000502, "Read Only"), url.Values{"character": {strconv.FormatInt(hostile, 10)}, "channel": {strconv.FormatInt(cid, 10)}, "remove": {"1"}})
	if rows, _ := q.ListIntelWatch(ctx, []int64{cid}); len(rows) != 1 {
		t.Errorf("a read-only member took a hostile off")
	}
}

// TestPilotIntelLiveFromZKill pins the on-demand lookup: a pilot
// with no stored activity gets their stats read live from
// zKillboard at page time, priced with EveSynapse's own appraisal,
// and nothing is stored. A pilot with stored activity never
// triggers the live fetch.
func TestPilotIntelLiveFromZKill(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter'), (22456, 'Sabre') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES (10000002, 587, 400000, now()) ON CONFLICT DO NOTHING`)

	const stranger = int64(91000005)
	const stored = int64(92000005)
	killAt := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	lossAt := now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	hist := newFakeZkillHistory(map[string]string{
		"/characterID/91000005/": `[` +
			`{"killmail_id":800011,"killmail_time":"` + killAt + `","solar_system_id":30000142,` +
			`"victim":{"character_id":92000001,"corporation_id":98000009,"ship_type_id":587,"damage_taken":900,"items":[]},` +
			`"attackers":[{"character_id":91000005,"corporation_id":98000002,"ship_type_id":22456,"damage_done":900,"final_blow":true,"weapon_type_id":2488}],` +
			`"zkb":{"locationID":0,"hash":"hash800011","totalValue":10.0,"points":1,"npc":false,"solo":true,"awox":false}},` +
			`{"killmail_id":800012,"killmail_time":"` + lossAt + `","solar_system_id":30000142,` +
			`"victim":{"character_id":91000005,"corporation_id":98000002,"ship_type_id":587,"damage_taken":700,"items":[]},` +
			`"attackers":[{"character_id":92000002,"corporation_id":98000004,"ship_type_id":587,"damage_done":700,"final_blow":true,"weapon_type_id":2488}],` +
			`"zkb":{"locationID":0,"hash":"hash800012","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}]`,
		"/characterID/92000005/": `[` +
			`{"killmail_id":800013,"killmail_time":"` + killAt + `","solar_system_id":30000142,` +
			`"victim":{"character_id":92000003,"corporation_id":98000009,"ship_type_id":587,"damage_taken":900,"items":[]},` +
			`"attackers":[{"character_id":92000005,"corporation_id":98000002,"ship_type_id":22456,"damage_done":900,"final_blow":true,"weapon_type_id":2488}],` +
			`"zkb":{"locationID":0,"hash":"hash800013","totalValue":10.0,"points":1,"npc":false,"solo":true,"awox":false}}]`,
	})
	srv := httptest.NewServer(hist.handler())
	defer srv.Close()
	app.cfg.zkillAPIBaseURL = srv.URL + "/"
	visitor := &http.Cookie{Name: "nobody", Value: "x"}

	// The stranger has no stored activity: the page reads them
	// live from zKillboard.
	_, body := getPage(t, app, visitor, "/pilot/?character="+strconv.FormatInt(stranger, 10))
	for _, want := range []string{"from zKill, live", "Sabre", "Jita", "ISK destroyed"} {
		if !strings.Contains(body, want) {
			t.Errorf("live pilot page lacks %q", want)
		}
	}
	if got := hist.hitCount("/characterID/91000005/"); got != 1 {
		t.Errorf("stranger history fetched %d times, want 1", got)
	}
	// Nothing was stored: no kill rows, no profile.
	var n int
	if err := app.db.QueryRowContext(ctx, `SELECT count(*) FROM kills`).Scan(&n); err != nil {
		t.Fatalf("count kills: %v", err)
	}
	if n != 0 {
		t.Errorf("live lookup stored %d kills, want 0", n)
	}
	if _, err := q.GetPilotActivityProfile(ctx, stranger); err == nil {
		t.Error("live lookup claimed a profile, want none")
	}

	// A pilot with stored activity never triggers the live fetch.
	exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, ship_type_id, source, public, ingested_at, value_zkill)
		VALUES (900001, $1, 30000142, 92000003, 587, 'stream', true, now(), 10000000)`, now.Add(-2*time.Hour))
	exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, ship_type_id, final_blow) VALUES (900001, 0, $1, 22456, true)`, stored)
	exec(`INSERT INTO kill_involvement (killmail_id, character_id) VALUES (900001, $1) ON CONFLICT DO NOTHING`, stored)
	_, body2 := getPage(t, app, visitor, "/pilot/?character="+strconv.FormatInt(stored, 10))
	if strings.Contains(body2, "from zKill, live") {
		t.Error("stored pilot page marked live from zKill, want stored")
	}
	if got := hist.hitCount("/characterID/92000005/"); got != 0 {
		t.Errorf("stored pilot history fetched %d times, want 0", got)
	}
}
