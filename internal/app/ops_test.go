package app

// Ops and the unified calendar: who may create an op (an in-game
// corporation role, read from ESI's stored answer), who can see one
// (members of its corporation, nobody else), signing up with a ship
// and a role, cancelling, and the month grid that shows ops and
// in-game events together.

import (
	"bytes"
	"context"
	"evesynapse/internal/app/calendar"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	opsCorp      = int64(98000001)
	opsOtherCorp = int64(98000777)
	opsCharC     = int64(90000003) // a second account's member of opsCorp
	opsCharD     = int64(90000004) // an account in another corporation
)

// opsFixture: one corporation with a director (Fixture Ceo) and an alt
// on the same account, a plain member on a second account, and an
// outsider in another corporation on a third.
type opsFixture struct {
	t                       *testing.T
	app                     *Application
	q                       *db.Queries
	ctx                     context.Context
	director, member, other *http.Cookie
	directorUser            int64
	calls                   *countingTransport
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	f := &opsFixture{t: t, app: app, q: q, ctx: ctx, calls: transport}
	account := func(charID int64, name string, corp int64, roles ...string) (int64, *http.Cookie) {
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f.join(user.ID, charID, name, corp, roles...)
		return user.ID, sessionCookie(t, app, user.ID, charID, name)
	}
	f.directorUser, f.director = account(fixtureCharA, "Fixture Ceo", opsCorp, "Director", "Accountant")
	f.join(f.directorUser, fixtureCharB, "Fixture Alt", opsCorp)
	_, f.member = account(opsCharC, "Fixture Member", opsCorp, "Accountant")
	_, f.other = account(opsCharD, "Fixture Outsider", opsOtherCorp, "Director")
	return f
}

// join links a character to an account, in a corporation, holding
// the given in-game roles.
func (f *opsFixture) join(userID, charID int64, name string, corp int64, roles ...string) {
	f.t.Helper()
	seedCharacter(f.t, f.q, userID, charID, name)
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: charID, CorporationID: corp, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		f.t.Fatal(err)
	}
	seedSnapshot(f.t, f.q, charID, esi.SnapCorpRoles, esi.CharacterRoles{Roles: roles})
}

func (f *opsFixture) post(cookie *http.Cookie, path string, form url.Values) (int, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Location")
}

func (f *opsFixture) get(cookie *http.Cookie, path string) (int, string, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Location"), rec.Body.String()
}

// opForm is a valid op form, starting at the given time.
func opForm(start time.Time, title string) url.Values {
	return url.Values{
		"title": {title}, "owner": {fmt.Sprintf("corporation:%d", opsCorp)}, "fc": {fmt.Sprint(fixtureCharA)},
		"starts_at": {start.UTC().Format(opDateTimeLayout)}, "duration": {"90"},
		"doctrine": {"Shield Lokis"}, "form_up": {"Jita 4-4"}, "description": {"Bring probes."},
	}
}

// create makes an op as the director and returns its id.
func (f *opsFixture) create(start time.Time, title string) int64 {
	f.t.Helper()
	code, where := f.post(f.director, "/fleets/save", opForm(start, title))
	var id int64
	if _, err := fmt.Sscanf(where, "/fleets/%d/", &id); code != http.StatusSeeOther || err != nil {
		f.t.Fatalf("create op: %d to %q", code, where)
	}
	return id
}

func (f *opsFixture) ops() int {
	f.t.Helper()
	var n int
	if err := f.app.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM ops`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestOnlyAManagerRoleCreatesOps(t *testing.T) {
	f := newOpsFixture(t)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)

	// A member without the role is not offered the form, and a form
	// posted anyway creates nothing.
	if code, where, _ := f.get(f.member, "/fleets/plan"); code != http.StatusSeeOther || where != "/fleets/" {
		t.Fatalf("member GET /fleets/plan: %d to %q, want a redirect to the fleets page", code, where)
	}
	form := opForm(start, "Sneaky op")
	form.Set("fc", fmt.Sprint(opsCharC))
	if f.post(f.member, "/fleets/save", form); f.ops() != 0 {
		t.Fatalf("a member without the role created a fleet (%d stored)", f.ops())
	}
	_, _, body := f.get(f.member, "/calendar/")
	if strings.Contains(body, `href="/fleets/plan`) {
		t.Fatal("a member without the role is offered planning a fleet on the calendar")
	}

	// A director of another corporation cannot create one for this one.
	if code, _ := f.post(f.other, "/fleets/save", opForm(start, "Hostile takeover")); code == http.StatusSeeOther || f.ops() != 0 {
		t.Fatalf("an outsider created an op for this corporation (%d, %d stored)", code, f.ops())
	}

	// The director can, and the form is offered.
	code, _, body := f.get(f.director, "/fleets/plan?date=2026-11-07")
	if code != http.StatusOK {
		t.Fatalf("director GET /fleets/plan = %d", code)
	}
	mustContain(t, "/fleets/plan", body, `name="starts_at" value="2026-11-07T19:00"`,
		`<option value="90000001" selected>Fixture Ceo</option>`, `<option value="90000002">Fixture Alt</option>`)
	id := f.create(start, "Roam")
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "op page", body, ">Roam<", start.Format("15:04"), start.Format("Mon Jan 2"), "1h 30m", "Shield Lokis", "Jita 4-4", "Bring probes.", "Fixture Ceo",
		`href="/fleets/`+fmt.Sprint(id)+`/edit"`, "Cancel fleet")

	// What the form sends is checked, whatever the page offered.
	for name, change := range map[string]func(url.Values){
		"no title":                       func(v url.Values) { v.Set("title", "  ") },
		"a title far too long":           func(v url.Values) { v.Set("title", strings.Repeat("x", opTitleMax+1)) },
		"a start that is not a time":     func(v url.Values) { v.Set("starts_at", "next tuesday") },
		"a start years away":             func(v url.Values) { v.Set("starts_at", "2099-01-01T19:00") },
		"a length of no time":            func(v url.Values) { v.Set("duration", "0") },
		"an FC from another account":     func(v url.Values) { v.Set("fc", fmt.Sprint(opsCharC)) },
		"a corporation it cannot manage": func(v url.Values) { v.Set("owner", fmt.Sprintf("corporation:%d", opsOtherCorp)) },
	} {
		before := f.ops()
		v := opForm(start, "Checked")
		change(v)
		if code, _ := f.post(f.director, "/fleets/save", v); code == http.StatusSeeOther || f.ops() != before {
			t.Errorf("%s: accepted (%d)", name, code)
		}
	}

	// Changing an op: the manager can, a member cannot, and an op
	// stays with its corporation whatever the form says.
	edit := opForm(start.Add(time.Hour), "Roam, later")
	edit.Set("id", fmt.Sprint(id))
	edit.Set("owner", fmt.Sprintf("corporation:%d", opsOtherCorp))
	if code, _ := f.post(f.member, "/fleets/save", edit); code == http.StatusSeeOther {
		_, _, body = f.get(f.director, opURL(id))
		if strings.Contains(body, "Roam, later") {
			t.Fatal("a member without the role changed an op")
		}
	}
	if code, where := f.post(f.director, "/fleets/save", edit); code != http.StatusSeeOther || where != opURL(id) {
		t.Fatalf("director edit: %d to %q", code, where)
	}
	op, err := f.q.GetOp(f.ctx, id)
	if err != nil || op.Title != "Roam, later" || op.CorporationID != opsCorp || !op.StartsAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("after the edit: %+v (%v)", op, err)
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("%d outbound call(s) from the ops pages", got)
	}
}

func TestOpsAreSeenOnlyByTheirCorporation(t *testing.T) {
	f := newOpsFixture(t)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	id := f.create(start, "Members only")
	month := start.Format(calendar.MonthLayout)

	if code, _, body := f.get(f.member, opURL(id)); code != http.StatusOK || !strings.Contains(body, "Members only") {
		t.Fatalf("a member cannot open their corporation's op (%d)", code)
	}
	_, _, body := f.get(f.member, "/calendar/?month="+month)
	mustContain(t, "member calendar", body, `class="cal-entry cal-op" href="`+opURL(id)+`"`, "Members only")

	// Someone in another corporation gets the same answer as for an
	// op that does not exist, and never sees it on their calendar.
	for _, path := range []string{opURL(id), opURL(id) + "edit", opURL(999999)} {
		if code, _, body := f.get(f.other, path); code != http.StatusNotFound || strings.Contains(body, "Members only") {
			t.Fatalf("outsider GET %s: %d", path, code)
		}
	}
	_, _, body = f.get(f.other, "/calendar/?month="+month)
	if strings.Contains(body, "Members only") {
		t.Fatal("an outsider's calendar shows another corporation's op")
	}
	if code, _ := f.post(f.other, opURL(id)+"signup", url.Values{"character": {fmt.Sprint(opsCharD)}, "response": {"yes"}}); code != http.StatusNotFound {
		t.Fatalf("outsider sign-up = %d", code)
	}
	if code, _ := f.post(f.other, opURL(id)+"cancel", url.Values{}); code != http.StatusNotFound {
		t.Fatalf("outsider cancel = %d", code)
	}
	signups, _ := f.q.ListOpSignups(f.ctx, id)
	op, _ := f.q.GetOp(f.ctx, id)
	if len(signups) != 0 || op.CancelledAt.Valid {
		t.Fatalf("an outsider changed the op: %d sign-up(s), cancelled=%v", len(signups), op.CancelledAt.Valid)
	}
}

func TestOpSignUpCancelAndReopen(t *testing.T) {
	f := newOpsFixture(t)
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	id := f.create(start, "Structure bash")
	signup := func(cookie *http.Cookie, v url.Values) {
		t.Helper()
		if code, _ := f.post(cookie, opURL(id)+"signup", v); code != http.StatusSeeOther {
			t.Fatalf("sign-up = %d", code)
		}
	}

	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}, "ship": {"Guardian"}, "fleet_role": {"Logistics"}, "note": {"20 minutes late"}})
	signup(f.director, url.Values{"character": {fmt.Sprint(fixtureCharA)}, "response": {"yes"}, "ship": {"Loki"}, "fleet_role": {"Command"}})
	signup(f.director, url.Values{"character": {fmt.Sprint(fixtureCharB)}, "response": {"maybe"}, "ship": {"Sabre"}, "fleet_role": {"Tackle"}})

	_, _, body := f.get(f.member, opURL(id))
	mustContain(t, "op page", body,
		`Coming <small>2</small>`, "1 Logistics, 1 Command",
		"<td>Guardian</td><td>Logistics</td><td>20 minutes late</td>",
		"<h3>Maybe</h3>", "<td>Sabre</td><td>Tackle</td>",
		// The form shows the member's own answer back.
		`value="Guardian"`, `<option value="Logistics" selected>`, "Update sign-up")
	if strings.Contains(body, "Cancel fleet") {
		t.Fatal("a member without the role is offered Cancel fleet")
	}
	// On the calendar, the member's own answer sits on the entry.
	_, _, body = f.get(f.member, "/calendar/?month="+start.Format(calendar.MonthLayout))
	mustContain(t, "calendar", body, "Structure bash <small>yes</small>")

	// Changing one's mind: not coming clears what was to be brought.
	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"no"}, "ship": {"Guardian"}, "fleet_role": {"Logistics"}})
	rows, _ := f.q.ListOpSignups(f.ctx, id)
	for _, row := range rows {
		if row.CharacterID == opsCharC && (row.Response != opNo || row.Ship != "" || row.FleetRole != "") {
			t.Fatalf("after answering no: %+v", row)
		}
	}

	// What is not a real answer is not stored: another account's
	// character, a response that does not exist, a made-up role.
	signup(f.member, url.Values{"character": {fmt.Sprint(fixtureCharA)}, "response": {"no"}})
	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"definitely"}})
	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}, "fleet_role": {"Admiral"}, "ship": {strings.Repeat("s", 500)}})
	rows, _ = f.q.ListOpSignups(f.ctx, id)
	for _, row := range rows {
		switch row.CharacterID {
		case fixtureCharA:
			if row.Response != opYes {
				t.Fatal("one account changed another's sign-up")
			}
		case opsCharC:
			if row.Response != opYes || row.FleetRole != "" || len(row.Ship) > opShortMax {
				t.Fatalf("member's last sign-up stored as %q, role %q, ship of %d bytes", row.Response, row.FleetRole, len(row.Ship))
			}
		}
	}

	// Cancelling is the manager's. A cancelled op closes its sign-ups
	// and stays on the calendar, struck through.
	if f.post(f.member, opURL(id)+"cancel", url.Values{}); func() bool { op, _ := f.q.GetOp(f.ctx, id); return op.CancelledAt.Valid }() {
		t.Fatal("a member without the role cancelled the op")
	}
	f.post(f.director, opURL(id)+"cancel", url.Values{})
	_, _, body = f.get(f.member, opURL(id))
	mustContain(t, "cancelled op", body, "This fleet was cancelled.", "Sign-ups are closed: the fleet was cancelled.")
	before, _ := f.q.ListOpSignups(f.ctx, id)
	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"no"}})
	after, _ := f.q.ListOpSignups(f.ctx, id)
	for i := range before {
		if before[i].Response != after[i].Response {
			t.Fatal("a sign-up to a cancelled op was recorded")
		}
	}
	_, _, body = f.get(f.member, "/calendar/?month="+start.Format(calendar.MonthLayout))
	mustContain(t, "calendar", body, `class="cal-entry cal-op cal-cancelled"`)
	// And the manager can bring it back.
	f.post(f.director, opURL(id)+"cancel", url.Values{})
	if op, _ := f.q.GetOp(f.ctx, id); op.CancelledAt.Valid {
		t.Fatal("the op was not brought back")
	}

	// An op that is over takes no more sign-ups.
	past := f.create(time.Now().UTC().Add(-5*time.Hour).Truncate(time.Minute), "Yesterday's news")
	f.post(f.member, opURL(past)+"signup", url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}})
	if rows, _ := f.q.ListOpSignups(f.ctx, past); len(rows) != 0 {
		t.Fatal("a sign-up to an op that is over was recorded")
	}
}

// TestCalendarShowsOpsAndInGameEventsTogether: one grid, both kinds,
// each on its own day, and the months either side one click away.
func TestCalendarShowsOpsAndInGameEventsTogether(t *testing.T) {
	f := newOpsFixture(t)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if now := time.Now().UTC(); month.Before(now.Add(-opMaxBehind)) || month.After(now.Add(opMaxAhead-40*24*time.Hour)) {
		month = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	}
	opAt := month.AddDate(0, 0, 9).Add(19 * time.Hour)     // the 10th, 19:00
	eventAt := month.AddDate(0, 0, 9).Add(21 * time.Hour)  // the 10th, 21:00
	otherAt := month.AddDate(0, 0, 14).Add(18 * time.Hour) // the 15th
	id := f.create(opAt, "Fleet night")
	seedSnapshot(t, f.q, fixtureCharA, esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 501, Title: "Corp meeting", EventDate: rfc(eventAt), EventResponse: "accepted"},
		{EventID: 502, Title: "Moon pull", EventDate: rfc(otherAt), EventResponse: "not_responded"},
		{EventID: 503, Title: "Next year's thing", EventDate: rfc(month.AddDate(1, 0, 0))},
	})

	code, _, body := f.get(f.director, "/calendar/?character=90000001&month="+month.Format(calendar.MonthLayout))
	if code != http.StatusOK {
		t.Fatalf("GET /calendar/ = %d", code)
	}
	mustContain(t, "/calendar/", body,
		`<strong class="cal-title">`+month.Format("January 2006")+`</strong>`,
		"month="+month.AddDate(0, -1, 0).Format(calendar.MonthLayout), "month="+month.AddDate(0, 1, 0).Format(calendar.MonthLayout),
		`<a class="btn cal-new" href="/fleets/plan">Plan a fleet</a>`,
		`href="/fleets/plan?date=`+opAt.Format("2006-01-02")+`"`,
		`<a class="cal-entry cal-op" href="`+opURL(id)+`"><span class="cal-time">19:00</span> Fleet night</a>`,
		`event=501"><span class="cal-time">21:00</span> Corp meeting <small>accepted</small></a>`,
		// An event not answered yet carries no label.
		`<span class="cal-time">18:00</span> Moon pull</a>`,
	)
	if strings.Contains(body, `cal-time">`+rfc(month.AddDate(1, 0, 0))[11:16]+`</span> Next year's thing`) {
		t.Fatal("an event from another month is on this month's grid")
	}
	// The op and the meeting share a day, the op first (19:00, 21:00).
	day := body[strings.Index(body, `href="/fleets/plan?date=`+opAt.Format("2006-01-02")+`"`):]
	day = day[:strings.Index(day, "</td>")]
	if op, ev := strings.Index(day, "Fleet night"), strings.Index(day, "Corp meeting"); op < 0 || ev < 0 || op > ev {
		t.Fatalf("the 10th should hold the op then the meeting: %q", day)
	}
	if strings.Contains(day, "Moon pull") {
		t.Fatal("an event on the 15th is drawn on the 10th")
	}

	// The grid is whole weeks from Monday.
	from, to := calendar.GridRange(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if from.Format("Mon 2006-01-02") != "Mon 2026-09-28" || to.Format("Mon 2006-01-02") != "Mon 2026-11-02" {
		t.Fatalf("October 2026 grid runs %s to %s", from.Format("Mon 2006-01-02"), to.Format("Mon 2006-01-02"))
	}
	if got := calendar.ParseMonth("nonsense", time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)).Format(calendar.MonthLayout); got != "2026-10" {
		t.Fatalf("an unreadable month gave %s, want the current one", got)
	}
}

// TestOpsManagerRoleIsConfigurable: Director by default, and whatever
// OPS_MANAGER_ROLES names instead.
func TestOpsManagerRoleIsConfigurable(t *testing.T) {
	var cfg Config
	if !cfg.holdsOpsManagerRole([]string{"Accountant", "Director"}) || cfg.holdsOpsManagerRole([]string{"Accountant"}) || cfg.holdsOpsManagerRole(nil) {
		t.Fatal("by default the manager role is Director and nothing else")
	}
	cfg.opsManagerRoles = parseRoleList(" Personnel_Manager , Fitting_Manager ,, ")
	if !cfg.holdsOpsManagerRole([]string{"personnel_manager"}) || cfg.holdsOpsManagerRole([]string{"Director"}) {
		t.Fatalf("with OPS_MANAGER_ROLES set, the roles are %q", cfg.opsManagerRoles)
	}
	if got := cfg.opsManagerRolesText(); got != "Personnel Manager or Fitting Manager" {
		t.Fatalf("roles read as %q", got)
	}
	if got := parseRoleList(""); len(got) != 1 || got[0] != "Director" {
		t.Fatalf("an empty setting gave %q", got)
	}
}

// fleetESI stands in for ESI's two fleet calls: the fleet a character
// is in, and that fleet's members. It records every path asked for.
type fleetESI struct {
	fleetStatus   int
	fleet         string
	membersStatus int
	members       string
	// coFleet is what the second commander (opsCharC) is in, if anything.
	coFleet string
	asked   []string
}

func (s *fleetESI) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	s.asked = append(s.asked, req.URL.Path)
	switch {
	case strings.HasSuffix(req.URL.Path, "/characters/90000001/fleet/"):
		return respond(s.fleetStatus, s.fleet)
	case strings.HasSuffix(req.URL.Path, "/characters/90000003/fleet/") && s.coFleet != "":
		return respond(http.StatusOK, s.coFleet)
	case strings.HasSuffix(req.URL.Path, "/fleets/777/members/"):
		return respond(s.membersStatus, s.members)
	}
	return respond(http.StatusNotFound, `{"error":"unexpected `+req.URL.Path+`"}`)
}

// newFleetFixture is the ops fixture over a stand-in ESI, with the
// director granted fleet access.
func newFleetFixture(t *testing.T, fleet *fleetESI) *opsFixture {
	t.Helper()
	app, _, q := buildCorpTestApp(t, fleet)
	ctx := context.Background()
	f := &opsFixture{t: t, app: app, q: q, ctx: ctx}
	account := func(charID int64, name string, corp int64, roles ...string) (int64, *http.Cookie) {
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f.join(user.ID, charID, name, corp, roles...)
		return user.ID, sessionCookie(t, app, user.ID, charID, name)
	}
	f.directorUser, f.director = account(fixtureCharA, "Fixture Ceo", opsCorp, "Director")
	_, f.member = account(opsCharC, "Fixture Member", opsCorp)
	_, f.other = account(opsCharD, "Fixture Outsider", opsOtherCorp, "Director")
	f.grantFleet(fixtureCharA, true)
	return f
}

func (f *opsFixture) grantFleet(charID int64, granted bool) {
	f.t.Helper()
	scopes := "esi-characters.read_corporation_roles.v1"
	if granted {
		scopes += " " + fleetScope
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET scopes = $1 WHERE character_id = $2`, scopes, charID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *opsFixture) attendance(id int64) map[int64]string {
	f.t.Helper()
	rows, err := f.q.ListOpAttendance(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[int64]string{}
	for _, row := range rows {
		out[row.CharacterID] = row.Source
	}
	return out
}

const fleetOfThree = `[{"character_id":90000001,"ship_type_id":29990},{"character_id":90000003,"ship_type_id":11987},{"character_id":95000001,"ship_type_id":587}]`

func TestOpAttendanceIsCapturedFromTheFleet(t *testing.T) {
	fleet := &fleetESI{
		fleetStatus: http.StatusOK, fleet: `{"fleet_id":777,"fleet_boss_id":90000001,"role":"fleet_commander"}`,
		membersStatus: http.StatusOK, members: fleetOfThree,
	}
	f := newFleetFixture(t, fleet)
	now := time.Now().UTC()

	// An op tomorrow: nothing is asked of ESI today.
	tomorrow := f.create(now.Add(24*time.Hour).Truncate(time.Minute), "Tomorrow")
	if stored, limited := f.app.captureOpAttendance(f.ctx, now); stored != 0 || limited || len(fleet.asked) != 0 {
		t.Fatalf("with no op running: %d recorded, ESI asked %v", stored, fleet.asked)
	}
	_, _, body := f.get(f.director, opURL(tomorrow))
	mustContain(t, "op not started", body, "Once the fleet starts, attendance is read from Fixture Ceo&#39;s fleet in EVE.")
	if strings.Contains(body, "Mark attendance by hand") {
		t.Fatal("hand-marking is offered before the op has started")
	}

	// An op running now: the fleet's three members are recorded,
	// whether or not they use EveSynapse or signed up.
	id := f.create(now.Add(-10*time.Minute).Truncate(time.Minute), "Running")
	if stored, limited := f.app.captureOpAttendance(f.ctx, now); stored != 3 || limited {
		t.Fatalf("recorded %d, limited=%v; want the fleet's 3", stored, limited)
	}
	got := f.attendance(id)
	if len(got) != 3 || got[fixtureCharA] != attendanceFleet || got[opsCharC] != attendanceFleet || got[95000001] != attendanceFleet {
		t.Fatalf("attendance %v, want the three fleet members", got)
	}
	if len(f.attendance(tomorrow)) != 0 {
		t.Fatal("tomorrow's op was given today's fleet")
	}
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "running op", body,
		`Attendance <small>3</small>`,
		"Reading Fixture Ceo&#39;s fleet in EVE every minute: 3 pilots seen so far.",
		"<td>Seen in the fleet</td>")

	// Someone leaves the fleet: they were there, and stay recorded.
	fleet.members = `[{"character_id":90000001,"ship_type_id":29990}]`
	f.app.captureOpAttendance(f.ctx, now.Add(time.Minute))
	if got := f.attendance(id); len(got) != 3 {
		t.Fatalf("after a pilot left the fleet, attendance is %v; they were there and should stay", got)
	}

	// The totals page counts it once the fleet is over, for members of
	// the corporation only.
	if _, _, body = f.get(f.member, "/fleets/history/"); strings.Contains(body, "Fixture Member</a></td>") {
		t.Fatal("the totals show who is in a running fleet")
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE ops SET starts_at = $1 WHERE id = $2`, now.Add(-48*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	_, _, body = f.get(f.member, "/fleets/history/")
	mustContain(t, "/fleets/history/", body, "Fixture Member</a></td><td class=\"num\">1</td><td class=\"num\">1</td>")
	if _, _, body = f.get(f.other, "/fleets/history/"); strings.Contains(body, "Fixture Member") {
		t.Fatal("another corporation's attendance is shown to an outsider")
	}
}

// TestOpAttendanceFallsBackToMarkingByHand: each reason the fleet
// cannot be read is recorded and said, and a manager can then mark
// who was there.
func TestOpAttendanceFallsBackToMarkingByHand(t *testing.T) {
	fleet := &fleetESI{fleetStatus: http.StatusNotFound, fleet: `{"error":"Character is not in a fleet"}`}
	f := newFleetFixture(t, fleet)
	now := time.Now().UTC()
	id := f.create(now.Add(-10*time.Minute).Truncate(time.Minute), "Running")
	status := func() string {
		t.Helper()
		op, err := f.q.GetOp(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return op.CaptureStatus
	}

	// Not in a fleet.
	f.app.captureOpAttendance(f.ctx, now)
	if status() != captureNoFleet {
		t.Fatalf("status %q, want %q", status(), captureNoFleet)
	}
	_, _, body := f.get(f.director, opURL(id))
	mustContain(t, "no fleet", body, "Fixture Ceo was not in a fleet when last checked", "Mark attendance by hand")

	// In a fleet, but not its boss: its members are not asked for.
	fleet.fleetStatus, fleet.fleet, fleet.asked = http.StatusOK, `{"fleet_id":777,"fleet_boss_id":95000009}`, nil
	f.app.captureOpAttendance(f.ctx, now)
	if status() != captureNotBoss || len(fleet.asked) != 1 {
		t.Fatalf("status %q after asking %v; want %q and only the fleet lookup", status(), fleet.asked, captureNotBoss)
	}
	// Boss when asked, not by the time the members were read.
	fleet.fleet, fleet.membersStatus, fleet.members = `{"fleet_id":777,"fleet_boss_id":90000001}`, http.StatusForbidden, `{"error":"forbidden"}`
	f.app.captureOpAttendance(f.ctx, now)
	if status() != captureNotBoss {
		t.Fatalf("status %q, want %q", status(), captureNotBoss)
	}

	// Fleet access not granted: ESI is not asked at all.
	f.grantFleet(fixtureCharA, false)
	fleet.asked = nil
	f.app.captureOpAttendance(f.ctx, now)
	if status() != captureNoScope || len(fleet.asked) != 0 {
		t.Fatalf("status %q after asking %v; want %q and no call", status(), fleet.asked, captureNoScope)
	}
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "no scope", body, "Fixture Ceo has not granted fleet access")
	if len(f.attendance(id)) != 0 {
		t.Fatalf("attendance recorded with no fleet readable: %v", f.attendance(id))
	}

	// Marking by hand. Only the manager, and only pilots on the
	// sign-up list. (An op in progress still takes sign-ups.)
	f.post(f.member, opURL(id)+"signup", url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}, "ship": {"Guardian"}})
	if rows, _ := f.q.ListOpSignups(f.ctx, id); len(rows) != 1 {
		t.Fatalf("%d sign-up(s) on the running op, want 1", len(rows))
	}
	f.post(f.member, opURL(id)+"attendance", url.Values{"present": {fmt.Sprint(opsCharC)}})
	if len(f.attendance(id)) != 0 {
		t.Fatal("a member without the role marked attendance")
	}
	f.post(f.director, opURL(id)+"attendance", url.Values{"present": {fmt.Sprint(opsCharC), "95000001", "junk"}})
	if got := f.attendance(id); len(got) != 1 || got[opsCharC] != attendanceManual {
		t.Fatalf("after marking: %v, want only the signed-up pilot, by hand", got)
	}
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "marked", body, "<td>Marked by hand</td>", `name="present" value="90000003" checked>`)

	// The fleet turns up after all and reports the same pilot: the
	// fleet's word replaces the hand mark, and saving the hand list
	// with nobody ticked cannot remove it.
	f.grantFleet(fixtureCharA, true)
	fleet.fleetStatus, fleet.fleet = http.StatusOK, `{"fleet_id":777,"fleet_boss_id":90000001}`
	fleet.membersStatus, fleet.members = http.StatusOK, `[{"character_id":90000003,"ship_type_id":11987}]`
	f.app.captureOpAttendance(f.ctx, now)
	if got := f.attendance(id); got[opsCharC] != attendanceFleet {
		t.Fatalf("after the fleet reported the pilot: %v", got)
	}
	f.post(f.director, opURL(id)+"attendance", url.Values{})
	if got := f.attendance(id); got[opsCharC] != attendanceFleet {
		t.Fatalf("an empty hand list removed what the fleet reported: %v", got)
	}
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "fleet after hand", body, `value="90000003" checked disabled> Fixture Member <small>from the fleet</small>`)

	// Before an op starts there is nothing to mark.
	later := f.create(now.Add(3*time.Hour).Truncate(time.Minute), "Later")
	f.post(f.member, opURL(later)+"signup", url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}})
	f.post(f.director, opURL(later)+"attendance", url.Values{"present": {fmt.Sprint(opsCharC)}})
	if len(f.attendance(later)) != 0 {
		t.Fatal("attendance was marked on an op that has not started")
	}
}

// TestLiveFleetShowsEachPilotWhatTheirRankShowsInGame: the fleet is read
// through whichever commander is boss while they are in the owner. Only
// pilots in the fleet see it: every member sees who is there, commanders
// see ships for the pilots under them, and the fleet commander and boss
// see everything. Running the fleet on EveSynapse adds no sight of it.
func TestLiveFleetShowsEachPilotWhatTheirRankShowsInGame(t *testing.T) {
	const boss = `{"fleet_id":777,"fleet_boss_id":90000003}`
	const joinerChar, bystanderChar, plannerChar = 90000077, 90000078, 90000079
	fleet := &fleetESI{
		fleetStatus: http.StatusOK, fleet: boss, coFleet: boss,
		membersStatus: http.StatusOK,
		members: fmt.Sprintf(`[
			{"character_id":%d,"ship_type_id":11987,"wing_id":-1,"squad_id":-1,"role":"fleet_commander"},
			{"character_id":%d,"ship_type_id":29990,"wing_id":10,"squad_id":100,"role":"squad_commander"},
			{"character_id":95000001,"ship_type_id":587,"wing_id":10,"squad_id":100,"role":"squad_member"},
			{"character_id":%d,"ship_type_id":638,"wing_id":10,"squad_id":200,"role":"squad_member"},
			{"character_id":%d,"ship_type_id":641,"wing_id":10,"squad_id":200,"role":"squad_member"}
		]`, opsCharC, fixtureCharA, joinerChar, opsCharD),
	}
	f := newFleetFixture(t, fleet)
	if _, err := f.app.db.ExecContext(f.ctx, `INSERT INTO sde_types (type_id, name) VALUES
		(11987, 'Guardian'), (29990, 'Loki'), (587, 'Rifter'), (638, 'Raven'), (641, 'Megathron')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := f.create(now.Add(-10*time.Minute).Truncate(time.Minute), "Running")
	f.grantFleet(opsCharC, true)
	if err := f.q.AddOpCommander(f.ctx, db.AddOpCommanderParams{OpID: id, CharacterID: opsCharC, AddedBy: fixtureCharA, AddedAt: now}); err != nil {
		t.Fatal(err)
	}
	if stored, limited := f.app.captureOpAttendance(f.ctx, now); stored != 5 || limited {
		t.Fatalf("recorded %d, limited=%v; want the 5 pilots read through the co-FC", stored, limited)
	}
	op, err := f.q.GetOp(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if op.CaptureStatus != captureRunning || op.CaptureBossID != opsCharC {
		t.Fatalf("status %q through %d, want %q through the co-FC", op.CaptureStatus, op.CaptureBossID, captureRunning)
	}
	account := func(charID int64, name string, roles ...string) *http.Cookie {
		t.Helper()
		user, err := f.q.CreateUser(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		f.join(user.ID, charID, name, opsCorp, roles...)
		return sessionCookie(t, f.app, user.ID, charID, name)
	}
	joiner := account(joinerChar, "Fixture Joiner")
	bystander := account(bystanderChar, "Fixture Bystander")
	planner := account(plannerChar, "Fixture Planner", "Director")

	path := fleetCompositionPath(id)
	const pilot = "Pilot #95000001"
	const pilotLink = "character=95000001"
	ship := func(typeID int) string { return fmt.Sprintf("/items/type/%d/", typeID) }
	// The edit forms live in the table view, which only commanders get.
	sees := func(who string, cookie *http.Cookie, want, hidden []string) {
		t.Helper()
		code, _, body := f.get(cookie, path+"?view=table")
		if code != http.StatusOK {
			t.Fatalf("%s: composition answered %d", who, code)
		}
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s does not see %q", who, s)
			}
		}
		for _, s := range hidden {
			if strings.Contains(body, s) {
				t.Errorf("%s sees %q", who, s)
			}
		}
	}
	sees("the boss", f.member,
		[]string{pilot, ship(11987), ship(29990), ship(587), ship(638), ship(641), "fl-hulls-title"}, nil)
	sees("a squad commander", f.director,
		[]string{pilot, ship(29990), ship(587), `id="fl-p-95000001"`},
		[]string{ship(11987), ship(638), ship(641), "fl-hulls-title", "fl-systems-title"})
	sees("a squad member", joiner,
		[]string{pilot, ship(638)},
		[]string{ship(11987), ship(29990), ship(587), ship(641), "fl-hulls-title", "fl-p-"})
	sees("a pilot in the fleet from outside the owner", f.other,
		[]string{pilot, ship(641)},
		[]string{ship(638), ship(587), "fl-hulls-title", "fl-p-"})
	if code, where, _ := f.get(f.other, opURL(id)); code != http.StatusSeeOther || where != path {
		t.Fatalf("overview for a pilot in the fleet who cannot see it: %d to %q", code, where)
	}
	sees("a planner outside the fleet", planner,
		[]string{"You are not in this fleet", "Attendance is being read from the fleet in EVE."},
		[]string{pilot, "/items/type/", "so far", "fl-p-", "Fixture Member"})
	if _, _, body := f.get(planner, opURL(id)); strings.Contains(body, "Seen in the fleet") || strings.Contains(body, "<small>from the fleet</small>") || strings.Contains(body, pilotLink) {
		t.Fatal("the attendance panel shows a running fleet to a planner outside it")
	}

	// Someone who could sign up but is not in the fleet.
	if code, where, _ := f.get(bystander, path); code != http.StatusSeeOther || where != opURL(id) {
		t.Fatalf("composition for a bystander: %d to %q", code, where)
	}
	if code, _, _ := f.get(bystander, path+"/live"); code != http.StatusForbidden {
		t.Fatalf("live fragment for a bystander answered %d", code)
	}
	if _, _, body := f.get(bystander, opURL(id)); strings.Contains(body, path) || strings.Contains(body, pilotLink) {
		t.Fatal("the overview offers a running fleet to a bystander")
	}
	if code, _, body := f.get(&http.Cookie{Name: "none", Value: "none"}, path); code != http.StatusNotFound || strings.Contains(body, pilot) {
		t.Fatalf("composition for an anonymous viewer answered %d", code)
	}

	// Only a commander in the fleet sets roles and tags.
	for name, cookie := range map[string]*http.Cookie{"member": joiner, "planner outside the fleet": planner} {
		f.post(cookie, opURL(id)+"pilot", url.Values{"character": {"95000001"}, "role": {"Logistics"}})
		if tags, err := f.q.ListOpMemberTags(f.ctx, id); err != nil || len(tags) != 0 {
			t.Fatalf("a %s assigned a role: %v, %v", name, tags, err)
		}
	}

	// A commander who leaves the owner is no longer read.
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: opsCharC, CorporationID: opsOtherCorp, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	fleet.asked = nil
	f.app.captureOpAttendance(f.ctx, now.Add(time.Minute))
	for _, asked := range fleet.asked {
		if strings.Contains(asked, "/characters/90000003/") || strings.Contains(asked, "/members/") {
			t.Fatalf("read %s through a commander who left the owner", asked)
		}
	}

	// Once the fleet is over, its last read shows to nobody. Who flew
	// stays with those running it and the pilots who were in it.
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE ops SET starts_at = $1 WHERE id = $2`, now.Add(-48*time.Hour), id); err != nil {
		t.Fatal(err)
	}
	if code, where, _ := f.get(joiner, path); code != http.StatusSeeOther || where != opURL(id) {
		t.Fatalf("composition after the fleet ended: %d to %q", code, where)
	}
	if _, _, body := f.get(joiner, opURL(id)); !strings.Contains(body, pilotLink) || !strings.Contains(body, ship(638)) || strings.Contains(body, ship(587)) {
		t.Fatal("a pilot who flew should see who was there and only their own ship")
	}
	if _, _, body := f.get(planner, opURL(id)); !strings.Contains(body, pilotLink) || !strings.Contains(body, ship(587)) {
		t.Fatal("a planner should keep the full attendance record")
	}
	if _, _, body := f.get(bystander, opURL(id)); strings.Contains(body, pilotLink) || strings.Contains(body, ship(587)) {
		t.Fatal("attendance of a past fleet is shown to someone who was not in it")
	}
}

// A game log uploaded from a fleet's Report tab counts in that fleet's
// report only for the uploader's pilot recorded on the fleet: a pilot
// the fleet has no record of, or another account's upload, adds nothing.
func TestFleetLogUploadCountsOnlyRecordedPilots(t *testing.T) {
	f := newOpsFixture(t)
	now := time.Now().UTC()
	id := f.create(now.Add(-3*time.Hour).Truncate(time.Minute), "Roam")
	if err := f.q.InsertManualAttendance(f.ctx, db.InsertManualAttendanceParams{OpID: id, CharacterID: fixtureCharA, SeenAt: now.Add(-150 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	upload := func(cookie *http.Cookie, listener string) string {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		stamp := now.Add(-150 * time.Minute).Format("2006.01.02 15:04:05")
		_ = mw.WriteField("paste", "  Gamelog\r\n  Listener: "+listener+"\r\n[ "+stamp+" ] (combat) <b>100</b> to <b>Bad Guy</b>[EVIL](Vexor) - Heavy Pulse Laser II - Hits\r\n")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/intel/battles/upload?fleet="+fmt.Sprint(id), &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, req)
		return rec.Header().Get("Location")
	}
	counted := func(userID int64, chars ...int64) []int64 {
		t.Helper()
		on, err := f.q.ListFleetBattleConsentFor(f.ctx, db.ListFleetBattleConsentForParams{OpID: id, UserID: userID, CharacterIds: chars})
		if err != nil {
			t.Fatal(err)
		}
		return on
	}

	if back := upload(f.director, "Fixture Alt"); back != fleetReportPath(id) {
		t.Fatalf("an upload from the fleet went back to %q, want its report", back)
	}
	if on := counted(f.directorUser, fixtureCharA, fixtureCharB); len(on) != 0 {
		t.Fatalf("a log of a pilot not recorded on the fleet counts in its report: %v", on)
	}
	upload(f.director, "Fixture Ceo")
	if on := counted(f.directorUser, fixtureCharA, fixtureCharB); len(on) != 1 || on[0] != fixtureCharA {
		t.Fatalf("logs counting after the recorded pilot's upload: %v, want only that pilot", on)
	}
	var member int64
	if err := f.app.db.QueryRowContext(f.ctx, `SELECT user_id FROM characters WHERE character_id = $1`, opsCharC).Scan(&member); err != nil {
		t.Fatal(err)
	}
	upload(f.member, "Fixture Member")
	if on := counted(member, opsCharC); len(on) != 0 {
		t.Fatalf("a member not on the fleet counts a log in its report: %v", on)
	}
}
