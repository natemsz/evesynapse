package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestOnlyAManagerRoleHandsOnFleetPlanning: a director outside
// OPS_MANAGER_ROLES cannot use the corporation's settings to give
// fleet planning to a role, and a holder of the manager role can.
func TestOnlyAManagerRoleHandsOnFleetPlanning(t *testing.T) {
	f := newOpsFixture(t)
	key := db.GetOwnerPermissionParams{OwnerKind: ownerCorporation, OwnerID: opsCorp, Permission: permFleets}
	save := url.Values{"corporation": {strconv.FormatInt(opsCorp, 10)}, "fleets_who": {"eve_role:Accountant"}}

	f.app.cfg.opsManagerRoles = parseRoleList("Personnel_Manager")
	postForm(t, f.app, f.director, "/corporations/settings/save", save)
	if rule, err := f.q.GetOwnerPermission(f.ctx, key); err == nil {
		t.Fatalf("a director outside the manager roles handed on fleets: %+v", rule)
	}

	f.app.cfg.opsManagerRoles = parseRoleList("Director")
	postForm(t, f.app, f.director, "/corporations/settings/save", save)
	if rule, err := f.q.GetOwnerPermission(f.ctx, key); err != nil || rule.Ref != "Accountant" {
		t.Fatalf("a holder of the manager role could not hand on fleets: %+v (%v)", rule, err)
	}
}

// TestPrivateFleetStaysOffPublicBoards: a private fleet's pilots rank
// only on the board its own members see. A public fleet counts toward
// its group, names its pilots only if its planner turned names on, shows
// its totals, and keeps its pilot-by-pilot report closed to strangers.
func TestPrivateFleetStaysOffPublicBoards(t *testing.T) {
	f := newOpsFixture(t)
	id := f.create(time.Now().UTC().Add(48*time.Hour).Truncate(time.Minute), "Closed strat op")
	start := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Minute)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.app.db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE ops SET starts_at = $1 WHERE id = $2`, start, id)
	for _, pilot := range []int64{fixtureCharA, fixtureCharB, opsCharC, 90000011, 90000012} {
		exec(`INSERT INTO op_attendance (op_id, character_id, ship_type_id, source, first_seen_at, last_seen_at)
		      VALUES ($1, $2, 587, 'fleet', $3, $4)`, id, pilot, start, start.Add(40*time.Minute))
	}
	exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, damage_taken, source, public, ingested_at, value_ours)
	      VALUES (7001, $1, 30000142, 95000001, 98009999, 587, 1000, 'test', TRUE, $1, 200000000)`, start.Add(10*time.Minute))
	exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, ship_type_id, damage_done, final_blow)
	      VALUES (7001, 0, $1, $2, 587, 1000, TRUE)`, opsCharC, opsCorp)
	f.app.refreshFleetReports(f.ctx)

	anonymous := func(path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	pilot := strconv.FormatInt(opsCharC, 10)
	ownBoard := "/fleets/leaderboard/?owner=corporation:" + strconv.FormatInt(opsCorp, 10)
	if _, body := anonymous("/fleets/leaderboard/"); strings.Contains(body, pilot) {
		t.Error("the public board ranks a pilot from a private fleet")
	}
	if code, body := anonymous(ownBoard); code != http.StatusNotFound || strings.Contains(body, pilot) {
		t.Errorf("a corporation with only private fleets has a public board: %d", code)
	}
	if _, _, body := f.get(f.other, ownBoard); strings.Contains(body, pilot) {
		t.Error("an outsider sees the private fleet on the corporation's board")
	}
	if _, _, body := f.get(f.member, ownBoard); !strings.Contains(body, pilot) {
		t.Error("a member does not see their own group's private fleet counted")
	}

	op, err := f.q.GetOp(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(names bool) {
		t.Helper()
		if err := f.q.UpdateFleetAccess(f.ctx, db.UpdateFleetAccessParams{ID: id, FleetType: op.FleetType, Visibility: fleetPublic, JoinGate: op.JoinGate, RosterPublic: names}); err != nil {
			t.Fatal(err)
		}
		if err := f.app.refreshFleetLeaders(f.ctx, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	publish(false)
	if _, body := anonymous("/fleets/leaderboard/"); strings.Contains(body, pilot) {
		t.Error("the public board names a pilot of a fleet with names turned off")
	}
	if code, body := anonymous(ownBoard); code != http.StatusOK || strings.Contains(body, pilot) {
		t.Errorf("a group's board with names turned off: %d", code)
	}
	if _, body := anonymous("/fleets/leaderboard/?board=groups"); !strings.Contains(body, strconv.FormatInt(opsCorp, 10)) {
		t.Error("the group board leaves out a public fleet with names turned off")
	}
	publish(true)
	if _, body := anonymous("/fleets/leaderboard/"); !strings.Contains(body, pilot) {
		t.Error("the public board leaves out a public fleet's pilot")
	}
	if code, body := anonymous(opURL(id)); code != http.StatusOK || !strings.Contains(body, "Fleet totals") {
		t.Errorf("a public fleet's totals: %d", code)
	}
	if code, body := anonymous(fleetReportPath(id)); code != http.StatusNotFound || strings.Contains(body, pilot) {
		t.Errorf("a stranger opened the pilot-by-pilot report: %d", code)
	}
}

// TestOutsiderCannotReadWhoIsOnARunningFleet: a public sign-up list stops
// naming pilots to outsiders while the fleet runs, stays open to the
// owner's members, and names them again once the fleet has closed.
func TestOutsiderCannotReadWhoIsOnARunningFleet(t *testing.T) {
	f := newOpsFixture(t)
	id := f.create(time.Now().UTC().Add(48*time.Hour).Truncate(time.Minute), "Open roam")
	setStart := func(start time.Time) {
		t.Helper()
		if _, err := f.app.db.ExecContext(f.ctx, `UPDATE ops SET starts_at = $1, visibility = 'public', roster_public = TRUE WHERE id = $2`, start, id); err != nil {
			t.Fatal(err)
		}
	}
	setStart(time.Now().UTC().Add(-10 * time.Minute))
	if err := f.q.UpsertOpSignup(f.ctx, db.UpsertOpSignupParams{
		OpID: id, CharacterID: opsCharC, Response: opYes, Ship: "Tengu", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	const pilot = "Fixture Member"
	if _, _, body := f.get(f.other, opURL(id)); strings.Contains(body, pilot) {
		t.Error("an outsider read the sign-up list of a running fleet")
	}
	if _, _, body := f.get(f.member, opURL(id)); !strings.Contains(body, pilot) {
		t.Error("the owner's member cannot read the sign-up list of a running fleet")
	}
	setStart(time.Now().UTC().Add(-6 * time.Hour))
	if _, _, body := f.get(f.other, opURL(id)); !strings.Contains(body, pilot) {
		t.Error("a closed fleet's public sign-up list stayed hidden")
	}
}

// TestAnonymousViewerGetsNoDetailsOfARestrictedFleet: without standing,
// a private fleet answers the same as one that does not exist, by its
// address, its share link and its calendar file. A link-only fleet
// opens by its link, with no pilot names and no form-up.
func TestAnonymousViewerGetsNoDetailsOfARestrictedFleet(t *testing.T) {
	f := newOpsFixture(t)
	id := f.create(time.Now().UTC().Add(48*time.Hour).Truncate(time.Minute), "Secret strat op")
	op, err := f.q.GetOp(f.ctx, id)
	if err != nil || !op.ShareToken.Valid {
		t.Fatalf("stored fleet: %+v (%v)", op, err)
	}
	link := "/f/" + op.ShareToken.String + "/"
	anonymous := func(path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	secrets := []string{"Secret strat op", "Jita 4-4", "Bring probes.", "Shield Lokis", "Fixture Ceo"}

	missingCode, missing := anonymous("/f/no-such-link/")
	if missingCode != http.StatusNotFound || !strings.Contains(missing, "Fleet not available") {
		t.Fatalf("a link that does not exist: %d", missingCode)
	}
	for _, path := range []string{opURL(id), opURL(id) + "edit", link} {
		code, body := anonymous(path)
		if code != missingCode || !strings.Contains(body, "Fleet not available") {
			t.Errorf("GET %s: %d, want the answer for a fleet that does not exist", path, code)
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s shows %q", path, secret)
			}
		}
	}
	if code, _ := anonymous(link + "fleet.ics"); code != http.StatusNotFound {
		t.Errorf("the calendar file of a private fleet: %d", code)
	}
	if _, body := anonymous("/fleets/"); strings.Contains(body, "Secret strat op") {
		t.Error("the board lists a private fleet")
	}

	if err := f.q.UpdateFleetAccess(f.ctx, db.UpdateFleetAccessParams{
		ID: id, FleetType: op.FleetType, Visibility: fleetUnlisted, JoinGate: joinByRule,
	}); err != nil {
		t.Fatal(err)
	}
	code, body := anonymous(link)
	if code != http.StatusOK || !strings.Contains(body, "Secret strat op") {
		t.Fatalf("a link-only fleet by its link: %d", code)
	}
	for _, secret := range []string{"Jita 4-4", "Fixture Ceo"} {
		if strings.Contains(body, secret) {
			t.Errorf("the share page shows %q to an anonymous viewer", secret)
		}
	}
	if code, _ := anonymous(opURL(id)); code != http.StatusNotFound {
		t.Errorf("a link-only fleet by its address, with no link: %d", code)
	}
}

// TestHomeFleetsWidgetShowsOnlyFleetsThePageShows: the home's Fleets
// widget lists a corporation's fleet to its member and never to an
// outsider the Fleets page would not show it to.
func TestHomeFleetsWidgetShowsOnlyFleetsThePageShows(t *testing.T) {
	f := newOpsFixture(t)
	f.create(time.Now().UTC().Add(5*time.Hour).Truncate(time.Minute), "Closed strat op")
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE users SET home_layout = '[{"id":"fleets","w":1,"h":2}]'`); err != nil {
		t.Fatal(err)
	}
	if _, _, body := f.get(f.member, "/"); !strings.Contains(body, "Closed strat op") {
		t.Error("a member's Fleets widget leaves out their corporation's fleet")
	}
	if _, _, body := f.get(f.other, "/"); strings.Contains(body, "Closed strat op") {
		t.Error("an outsider's Fleets widget lists a fleet the Fleets page hides from them")
	}
	if _, _, body := f.get(f.other, "/home/widget/fleets"); strings.Contains(body, "Closed strat op") {
		t.Error("the outsider's one-card render lists the hidden fleet")
	}
}
