package app

// Ops and the unified calendar: who may create an op (an in-game
// corporation role, read from ESI's stored answer), who can see one
// (members of its corporation, nobody else), signing up with a ship
// and a role, cancelling, and the month grid that shows ops and
// in-game events together.

import (
	"context"
	"fmt"
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
		"title": {title}, "corporation": {fmt.Sprint(opsCorp)}, "fc": {fmt.Sprint(fixtureCharA)},
		"starts_at": {start.UTC().Format(opDateTimeLayout)}, "duration": {"90"},
		"doctrine": {"Shield Lokis"}, "form_up": {"Jita 4-4"}, "description": {"Bring probes."},
	}
}

// create makes an op as the director and returns its id.
func (f *opsFixture) create(start time.Time, title string) int64 {
	f.t.Helper()
	code, where := f.post(f.director, "/ops/save", opForm(start, title))
	var id int64
	if _, err := fmt.Sscanf(where, "/ops/%d", &id); code != http.StatusSeeOther || err != nil {
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
	if code, where, _ := f.get(f.member, "/ops/new"); code != http.StatusSeeOther || where != "/calendar/" {
		t.Fatalf("member GET /ops/new: %d to %q, want a redirect to the calendar", code, where)
	}
	form := opForm(start, "Sneaky op")
	form.Set("fc", fmt.Sprint(opsCharC))
	if code, _ := f.post(f.member, "/ops/save", form); code == http.StatusSeeOther || f.ops() != 0 {
		t.Fatalf("a member without the role created an op (%d, %d stored)", code, f.ops())
	}
	_, _, body := f.get(f.member, "/calendar/")
	if strings.Contains(body, `href="/ops/new`) {
		t.Fatal("a member without the role is offered New op on the calendar")
	}

	// A director of another corporation cannot create one for this one.
	if code, _ := f.post(f.other, "/ops/save", opForm(start, "Hostile takeover")); code == http.StatusSeeOther || f.ops() != 0 {
		t.Fatalf("an outsider created an op for this corporation (%d, %d stored)", code, f.ops())
	}

	// The director can, and the form is offered.
	code, _, body := f.get(f.director, "/ops/new?date=2026-11-07")
	if code != http.StatusOK {
		t.Fatalf("director GET /ops/new = %d", code)
	}
	mustContain(t, "/ops/new", body, `name="starts_at" value="2026-11-07T19:00"`,
		`<option value="90000001" selected>Fixture Ceo</option>`, `<option value="90000002">Fixture Alt</option>`)
	id := f.create(start, "Roam")
	_, _, body = f.get(f.director, opURL(id))
	mustContain(t, "op page", body, ">Roam<", start.Format("Mon Jan 2, 15:04")+" EVE", "1h 30m", "Shield Lokis", "Jita 4-4", "Bring probes.", "Fixture Ceo",
		`href="/ops/`+fmt.Sprint(id)+`/edit"`, "Cancel op")

	// What the form sends is checked, whatever the page offered.
	for name, change := range map[string]func(url.Values){
		"no title":                       func(v url.Values) { v.Set("title", "  ") },
		"a title far too long":           func(v url.Values) { v.Set("title", strings.Repeat("x", opTitleMax+1)) },
		"a start that is not a time":     func(v url.Values) { v.Set("starts_at", "next tuesday") },
		"a start years away":             func(v url.Values) { v.Set("starts_at", "2099-01-01T19:00") },
		"a length of no time":            func(v url.Values) { v.Set("duration", "0") },
		"an FC from another account":     func(v url.Values) { v.Set("fc", fmt.Sprint(opsCharC)) },
		"a corporation it cannot manage": func(v url.Values) { v.Set("corporation", fmt.Sprint(opsOtherCorp)) },
	} {
		before := f.ops()
		v := opForm(start, "Checked")
		change(v)
		if code, _ := f.post(f.director, "/ops/save", v); code == http.StatusSeeOther || f.ops() != before {
			t.Errorf("%s: accepted (%d)", name, code)
		}
	}

	// Changing an op: the manager can, a member cannot, and an op
	// stays with its corporation whatever the form says.
	edit := opForm(start.Add(time.Hour), "Roam, later")
	edit.Set("id", fmt.Sprint(id))
	edit.Set("corporation", fmt.Sprint(opsOtherCorp))
	if code, _ := f.post(f.member, "/ops/save", edit); code == http.StatusSeeOther {
		_, _, body = f.get(f.director, opURL(id))
		if strings.Contains(body, "Roam, later") {
			t.Fatal("a member without the role changed an op")
		}
	}
	if code, where := f.post(f.director, "/ops/save", edit); code != http.StatusSeeOther || where != opURL(id) {
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
	month := start.Format(calMonthLayout)

	if code, _, body := f.get(f.member, opURL(id)); code != http.StatusOK || !strings.Contains(body, "Members only") {
		t.Fatalf("a member cannot open their corporation's op (%d)", code)
	}
	_, _, body := f.get(f.member, "/calendar/?month="+month)
	mustContain(t, "member calendar", body, `class="cal-entry cal-op" href="`+opURL(id)+`"`, "Members only")

	// Someone in another corporation gets the same answer as for an
	// op that does not exist, and never sees it on their calendar.
	for _, path := range []string{opURL(id), opURL(id) + "/edit", opURL(999999)} {
		if code, where, body := f.get(f.other, path); code != http.StatusSeeOther || where != "/calendar/" || strings.Contains(body, "Members only") {
			t.Fatalf("outsider GET %s: %d to %q", path, code, where)
		}
	}
	_, _, body = f.get(f.other, "/calendar/?month="+month)
	if strings.Contains(body, "Members only") {
		t.Fatal("an outsider's calendar shows another corporation's op")
	}
	if code, _ := f.post(f.other, opURL(id)+"/signup", url.Values{"character": {fmt.Sprint(opsCharD)}, "response": {"yes"}}); code != http.StatusSeeOther {
		t.Fatalf("outsider sign-up = %d", code)
	}
	if code, _ := f.post(f.other, opURL(id)+"/cancel", url.Values{}); code != http.StatusSeeOther {
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
		if code, _ := f.post(cookie, opURL(id)+"/signup", v); code != http.StatusSeeOther {
			t.Fatalf("sign-up = %d", code)
		}
	}

	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}, "ship": {"Guardian"}, "fleet_role": {"Logistics"}, "note": {"20 minutes late"}})
	signup(f.director, url.Values{"character": {fmt.Sprint(fixtureCharA)}, "response": {"yes"}, "ship": {"Loki"}, "fleet_role": {"Command"}})
	signup(f.director, url.Values{"character": {fmt.Sprint(fixtureCharB)}, "response": {"maybe"}, "ship": {"Sabre"}, "fleet_role": {"Tackle"}})

	_, _, body := f.get(f.member, opURL(id))
	mustContain(t, "op page", body,
		"<h2>Coming <small>· 2</small></h2>", "1 Logistics · 1 Command",
		"<td>Guardian</td><td>Logistics</td><td>20 minutes late</td>",
		"<h2>Maybe <small>· 1</small></h2>", "<td>Sabre</td><td>Tackle</td>",
		// The form shows the member's own answer back.
		`value="Guardian"`, `<option value="Logistics" selected>`, "This character has answered: yes.")
	if strings.Contains(body, "Cancel op") {
		t.Fatal("a member without the role is offered Cancel op")
	}
	// On the calendar, the member's own answer sits on the entry.
	_, _, body = f.get(f.member, "/calendar/?month="+start.Format(calMonthLayout))
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
	if f.post(f.member, opURL(id)+"/cancel", url.Values{}); func() bool { op, _ := f.q.GetOp(f.ctx, id); return op.CancelledAt.Valid }() {
		t.Fatal("a member without the role cancelled the op")
	}
	f.post(f.director, opURL(id)+"/cancel", url.Values{})
	_, _, body = f.get(f.member, opURL(id))
	mustContain(t, "cancelled op", body, "This op was cancelled.", "Sign-ups are closed: the op was cancelled.")
	before, _ := f.q.ListOpSignups(f.ctx, id)
	signup(f.member, url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"no"}})
	after, _ := f.q.ListOpSignups(f.ctx, id)
	for i := range before {
		if before[i].Response != after[i].Response {
			t.Fatal("a sign-up to a cancelled op was recorded")
		}
	}
	_, _, body = f.get(f.member, "/calendar/?month="+start.Format(calMonthLayout))
	mustContain(t, "calendar", body, `class="cal-entry cal-op cal-cancelled"`)
	// And the manager can bring it back.
	f.post(f.director, opURL(id)+"/cancel", url.Values{})
	if op, _ := f.q.GetOp(f.ctx, id); op.CancelledAt.Valid {
		t.Fatal("the op was not brought back")
	}

	// An op that is over takes no more sign-ups.
	past := f.create(time.Now().UTC().Add(-5*time.Hour).Truncate(time.Minute), "Yesterday's news")
	f.post(f.member, opURL(past)+"/signup", url.Values{"character": {fmt.Sprint(opsCharC)}, "response": {"yes"}})
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

	code, _, body := f.get(f.director, "/calendar/?character=90000001&month="+month.Format(calMonthLayout))
	if code != http.StatusOK {
		t.Fatalf("GET /calendar/ = %d", code)
	}
	mustContain(t, "/calendar/", body,
		`<strong class="cal-title">`+month.Format("January 2006")+`</strong>`,
		"month="+month.AddDate(0, -1, 0).Format(calMonthLayout), "month="+month.AddDate(0, 1, 0).Format(calMonthLayout),
		`<a class="btn cal-new" href="/ops/new">New op</a>`,
		`href="/ops/new?date=`+opAt.Format("2006-01-02")+`"`,
		`<a class="cal-entry cal-op" href="`+opURL(id)+`"><span class="cal-time">19:00</span> Fleet night</a>`,
		`event=501"><span class="cal-time">21:00</span> Corp meeting <small>accepted</small></a>`,
		// An event not answered yet carries no label.
		`<span class="cal-time">18:00</span> Moon pull</a>`,
	)
	if strings.Contains(body, `cal-time">`+rfc(month.AddDate(1, 0, 0))[11:16]+`</span> Next year's thing`) {
		t.Fatal("an event from another month is on this month's grid")
	}
	// The op and the meeting share a day, the op first (19:00, 21:00).
	day := body[strings.Index(body, `href="/ops/new?date=`+opAt.Format("2006-01-02")+`"`):]
	day = day[:strings.Index(day, "</td>")]
	if op, ev := strings.Index(day, "Fleet night"), strings.Index(day, "Corp meeting"); op < 0 || ev < 0 || op > ev {
		t.Fatalf("the 10th should hold the op then the meeting: %q", day)
	}
	if strings.Contains(day, "Moon pull") {
		t.Fatal("an event on the 15th is drawn on the 10th")
	}

	// The grid is whole weeks from Monday.
	from, to := calGridRange(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if from.Format("Mon 2006-01-02") != "Mon 2026-09-28" || to.Format("Mon 2006-01-02") != "Mon 2026-11-02" {
		t.Fatalf("October 2026 grid runs %s to %s", from.Format("Mon 2006-01-02"), to.Format("Mon 2006-01-02"))
	}
	if got := parseCalMonth("nonsense", time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)).Format(calMonthLayout); got != "2026-10" {
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
