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
)

// TestIntelChannels: a channel is its owner's alone until granted; a
// corporation grant lets its members read, and post once it says so;
// an alliance grant reaches a member corporation through its record.
// Reports come from the form and from pasted chat lines, show in the
// feed and on the map's intel feed, and only the owner manages access.
func TestIntelChannels(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30000142, 'Jita', 10000002, 0.946), (30004759, '1DQ1-A', 10000060, -0.38) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	const corp, otherCorp, alliance = int64(98000101), int64(98000102), int64(99000101)
	member := func(charID int64, name string, corpID int64) (int64, *http.Cookie) {
		u, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedCharacter(t, q, u.ID, charID, name)
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: charID, CorporationID: corpID, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return u.ID, sessionCookie(t, app, u.ID, charID, name)
	}
	ownerID, owner := member(90000101, "Channel Owner", corp)
	_, mate := member(90000102, "Corp Mate", corp)
	_, stranger := member(90000103, "Other Pilot", otherCorp)

	send := func(c *http.Cookie, path string, form url.Values, script bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if script {
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	send(owner, "/intel/channels/create", url.Values{"name": {"Home intel"}}, false)
	chans, err := q.ListAccessibleChannels(ctx, db.ListAccessibleChannelsParams{UserID: ownerID, CharacterIds: []int64{}, CorporationIds: []int64{}, AllianceIds: []int64{}, GroupIds: []int64{}})
	if err != nil || len(chans) != 1 || !chans[0].CanPost {
		t.Fatalf("the owner's channel: %+v %v", chans, err)
	}
	base := "/intel/channels/" + strconv.FormatInt(chans[0].ID, 10) + "/"

	if code, _ := getPage(t, app, mate, base); code != http.StatusNotFound {
		t.Fatalf("an ungranted corp mate sees the channel: %d", code)
	}
	grant := func(kind string, target int64, post bool) {
		t.Helper()
		if err := q.UpsertIntelChannelGrant(ctx, db.UpsertIntelChannelGrantParams{ChannelID: chans[0].ID, Kind: kind, TargetID: target, CanPost: post, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	grant("corporation", corp, false)
	if code, body := getPage(t, app, mate, base); code != http.StatusOK || strings.Contains(body, `action="`+base+`report"`) {
		t.Fatalf("a read-only member reads without the report form: %d", code)
	}
	if rec := send(mate, base+"report", url.Values{"system": {"Jita"}, "hostiles": {"3"}}, true); rec.Code != http.StatusForbidden {
		t.Fatalf("a read-only member posted: %d %s", rec.Code, rec.Body.String())
	}
	grant("corporation", corp, true)
	if rec := send(mate, base+"report", url.Values{"system": {"jita"}, "hostiles": {"3"}, "text": {"Bad Guy"}}, true); rec.Code != http.StatusOK {
		t.Fatalf("a posting member's report: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(mate, base+"report", url.Values{"system": {"Nowhere"}}, true); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a report needs a real system: %d", rec.Code)
	}
	lines := "[ 11:20:05 ] Scout > 1DQ1-A  Hostile One +2\nScout > 1dq1-a clr\nScout > hello\n"
	if rec := send(owner, base+"report", url.Values{"lines": {lines}}, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"posted":2`) {
		t.Fatalf("pasted lines: %d %s", rec.Code, rec.Body.String())
	}

	var feed struct{ Reports []reportView }
	_, body := getPage(t, app, owner, base+"feed.json")
	if err := json.Unmarshal([]byte(body), &feed); err != nil || len(feed.Reports) != 3 {
		t.Fatalf("feed: %v %s", err, body)
	}
	bySource := map[string][]reportView{}
	for _, r := range feed.Reports {
		bySource[r.Source] = append(bySource[r.Source], r)
	}
	if f := bySource["scan"]; len(f) != 1 || f[0].SystemName != "Jita" || f[0].Hostiles != 3 || f[0].Reporter != "Corp Mate" || f[0].Text != "1 in local: 0 hostiles, 0 friendly, 1 neutral — no scan attached, ships unknown." {
		t.Errorf("the form report: %+v", f)
	}
	if c := bySource["chat"]; len(c) != 2 || c[1].Reporter != "Scout" || c[1].Hostiles != 2 || c[1].At >= c[0].At {
		t.Errorf("the pasted lines, the timestamped one seen earlier and listed after: %+v", c)
	}
	for i := 1; i < len(feed.Reports); i++ {
		if feed.Reports[i].At > feed.Reports[i-1].At {
			t.Errorf("the feed runs latest seen first")
		}
	}

	intel := func(c *http.Cookie) (channels, reports int) {
		t.Helper()
		var d struct {
			Channels []json.RawMessage
			Reports  []json.RawMessage
		}
		_, body := getPage(t, app, c, "/map/intel.json")
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("intel.json: %v %s", err, body)
		}
		return len(d.Channels), len(d.Reports)
	}
	if c, r := intel(mate); c != 1 || r < 2 {
		t.Errorf("the mate's map intel: %d channels, %d reports", c, r)
	}
	if c, r := intel(stranger); c != 0 || r != 0 {
		t.Errorf("a stranger's map intel: %d channels, %d reports", c, r)
	}

	// An alliance grant reaches the stranger's corporation through its record.
	payload, _ := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Other", AllianceID: alliance}})
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{CorporationID: otherCorp, Payload: string(payload), State: orgStateReady, FetchedAt: timeSet(time.Now())}); err != nil {
		t.Fatal(err)
	}
	grant("alliance", alliance, false)
	if c, _ := intel(stranger); c != 1 {
		t.Errorf("an alliance grant: the member corporation's pilot sees %d channels", c)
	}

	// Only the owner manages access.
	send(mate, base+"grants/remove", url.Values{"kind": {"alliance"}, "target": {strconv.FormatInt(alliance, 10)}}, false)
	if c, _ := intel(stranger); c != 1 {
		t.Errorf("a member removed a grant")
	}
	send(owner, base+"grants/remove", url.Values{"kind": {"alliance"}, "target": {strconv.FormatInt(alliance, 10)}}, false)
	if c, _ := intel(stranger); c != 0 {
		t.Errorf("the owner's removal did not take")
	}
}

// TestIntelFeedAccess: the cross-channel feed gathers only the
// channels its reader can reach, each report naming its channel; a
// channel nobody granted stays out of it entirely, and the page
// itself needs a signed-in account.
func TestIntelFeedAccess(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30000142, 'Jita', 10000002, 0.946), (30004759, '1DQ1-A', 10000060, -0.38) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	const corp, otherCorp = int64(98000101), int64(98000102)
	member := func(charID int64, name string, corpID int64) (int64, *http.Cookie) {
		u, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedCharacter(t, q, u.ID, charID, name)
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: charID, CorporationID: corpID, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return u.ID, sessionCookie(t, app, u.ID, charID, name)
	}
	ownerID, owner := member(90000101, "Channel Owner", corp)
	_, mate := member(90000102, "Corp Mate", corp)
	_, stranger := member(90000103, "Other Pilot", otherCorp)

	send := func(c *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	send(owner, "/intel/channels/create", url.Values{"name": {"Home intel"}})
	send(owner, "/intel/channels/create", url.Values{"name": {"Secret intel"}})
	chans, err := q.ListAccessibleChannels(ctx, db.ListAccessibleChannelsParams{UserID: ownerID, CharacterIds: []int64{}, CorporationIds: []int64{}, AllianceIds: []int64{}, GroupIds: []int64{}})
	if err != nil || len(chans) != 2 {
		t.Fatalf("the owner's channels: %+v %v", chans, err)
	}
	byName := map[string]int64{}
	for _, c := range chans {
		byName[c.Name] = c.ID
	}
	if err := q.UpsertIntelChannelGrant(ctx, db.UpsertIntelChannelGrantParams{ChannelID: byName["Home intel"], Kind: "corporation", TargetID: corp, CanPost: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	report := func(c *http.Cookie, channelID int64, text string) {
		t.Helper()
		path := "/intel/channels/" + strconv.FormatInt(channelID, 10) + "/report"
		if rec := send(c, path, url.Values{"system": {"Jita"}, "text": {text}}); rec.Code != http.StatusOK {
			t.Fatalf("report %q: %d %s", text, rec.Code, rec.Body.String())
		}
	}
	report(owner, byName["Home intel"], "Home Guy")
	report(owner, byName["Secret intel"], "Secret Guy")

	feed := func(c *http.Cookie) []reportView {
		t.Helper()
		var d struct{ Reports []reportView }
		_, body := getPage(t, app, c, "/intel/feed.json")
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("feed.json: %v %s", err, body)
		}
		return d.Reports
	}
	if rs := feed(mate); len(rs) != 1 || rs[0].Text != "1 in local: 0 hostiles, 0 friendly, 0 neutral, 1 unresolved — no scan attached, ships unknown." || rs[0].ChName != "Home intel" {
		t.Errorf("the mate's feed carries only the granted channel, named: %+v", rs)
	} else if len(rs[0].Split) != 1 || rs[0].Split[0].Class != "unclassified" || rs[0].Split[0].N != 1 {
		t.Errorf("the mate's feed report carries its stored standing split for the bar: %+v", rs[0].Split)
	}
	if rs := feed(owner); len(rs) != 2 {
		t.Errorf("the owner's feed carries both channels: %+v", rs)
	}
	if rs := feed(stranger); len(rs) != 0 {
		t.Errorf("a stranger's feed is empty: %+v", rs)
	}
	if code, body := getPage(t, app, mate, "/intel/feed/"); code != http.StatusOK || !strings.Contains(body, "1 in local: 0 hostiles, 0 friendly, 0 neutral, 1 unresolved — no scan attached, ships unknown.") || strings.Contains(body, "Secret Guy") || !strings.Contains(body, "section-tabs") {
		t.Errorf("the mate's feed page: %d, shows the granted channel's report only, under the intel section tabs", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/intel/feed/", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("the feed page signed out: %d, want the sign-in redirect", rec.Code)
	}
}
