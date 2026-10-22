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
	"evesynapse/internal/intelanalyze"
)

// TestIntelAnalysis: a report is read as it is stored. Pilots named
// are linked and told friend from foe by the channel's own side (its
// owner's corporation here), a Sabre makes it a bubble, a report
// naming only a corpmate is friendly, and a name nobody has looked up
// waits until the worker's pass has it, then is read again.
func TestIntelAnalysis(t *testing.T) {
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
	const corp = int64(98000301)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000301, "Channel Owner")
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: 90000301, CorporationID: corp, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, app, u.ID, 90000301, "Channel Owner")
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	name := func(lower string, id int64, proper string, corpID int64) {
		t.Helper()
		if err := q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: lower, CharacterID: id, Name: proper, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: id, CorporationID: corpID, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	name("hostile one", 91000301, "Hostile One", 98000999)
	name("mate pilot", 91000302, "Mate Pilot", corp)

	paste := func(lines string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/intel/channels/"+strconv.FormatInt(cid, 10)+"/report", strings.NewReader(url.Values{"lines": {lines}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	latest := func() (db.ListIntelReportsRow, intelanalyze.Analysis) {
		t.Helper()
		rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 1})
		if err != nil || len(rows) != 1 {
			t.Fatalf("latest report: %v", err)
		}
		var a intelanalyze.Analysis
		_ = json.Unmarshal([]byte(rows[0].Analysis), &a)
		return rows[0], a
	}

	paste("Scout > 1DQ1-A  Hostile One  Mate Pilot  sabre")
	r, a := latest()
	if r.Kind != intelanalyze.KindBubble || len(a.Pilots) != 2 || len(a.Ships) != 1 || a.Ships[0].TypeID != 22456 {
		t.Fatalf("a sabre with two pilots: kind %q, %+v", r.Kind, a)
	}
	if a.Pilots[0].Friendly || !a.Pilots[1].Friendly {
		t.Errorf("friend and foe: %+v", a.Pilots)
	}
	_, body := getPage(t, app, cookie, "/intel/channels/"+strconv.FormatInt(cid, 10)+"/")
	for _, want := range []string{`href="/pilot/?character=91000301"`, `class="ch-tag friendly"`, ">Bubble<", ">Sabre<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the feed lacks %s", want)
		}
	}

	paste("Scout > 1DQ1-A  Mate Pilot")
	if r, _ := latest(); r.Kind != intelanalyze.KindFriendly {
		t.Errorf("only a corpmate named: kind %q, want friendly", r.Kind)
	}

	// A name nobody has looked up: pending, then read again once known.
	paste("Scout > 1DQ1-A  Brand New")
	pending, err := q.ListPendingIntelReports(ctx, db.ListPendingIntelReportsParams{Since: now.Add(-time.Hour), MaxRows: 10})
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending reports: %d %v", len(pending), err)
	}
	name("brand new", 91000303, "Brand New", 98000999)
	if limited := app.drainIntelAnalysis(ctx); limited {
		t.Fatal("the pass hit a limit")
	}
	r, a = latest()
	if r.Kind != intelanalyze.KindSighting || len(a.Pilots) != 1 || a.Pilots[0].ID != 91000303 {
		t.Errorf("read again once known: kind %q, %+v", r.Kind, a)
	}
	if left, _ := q.ListPendingIntelReports(ctx, db.ListPendingIntelReportsParams{Since: now.Add(-time.Hour), MaxRows: 10}); len(left) != 0 {
		t.Errorf("%d reports still pending", len(left))
	}
}
