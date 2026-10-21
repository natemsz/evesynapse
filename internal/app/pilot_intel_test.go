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
