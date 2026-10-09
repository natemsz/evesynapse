package app

// Notifications as the user meets them: the top-bar icon and its
// short list on every page, the /notifications/ page, and the two
// actions that change what is unread.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// add stores one notification for the fixture's account.
func (f *notifyFixture) add(userID, characterID int64, kind, title, target string) {
	f.t.Helper()
	if _, err := f.q.InsertNotification(f.ctx, db.InsertNotificationParams{
		UserID: userID, CharacterID: characterID, Kind: kind, Title: title, Url: target, CreatedAt: notifyT0,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// post submits a form as the fixture's user and returns the status
// and where it redirects to.
func (f *notifyFixture) post(cookie *http.Cookie, path string, form url.Values) (int, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Location")
}

// unread counts the account's unread notifications by kind.
func (f *notifyFixture) unread(userID int64) map[string]int64 {
	f.t.Helper()
	rows, err := f.q.ListUnreadNotificationSummary(f.ctx, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]int64{}
	for _, row := range rows {
		out[row.Kind] = row.Unread
	}
	return out
}

// The pictures: what marks each icon in the page.
const (
	iconMail    = `<path d="M4 7.2l8 6 8-6"/>`
	iconPlanet  = `<circle class="notify-dot"`
	iconGeneric = `<path d="M10 20.5a2 2 0 0 0 4 0"/>`
)

func TestNotifyIconFollowsWhatIsUnread(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	page := func() string {
		t.Helper()
		code, body := getPage(t, f.app, cookie, "/notifications/")
		if code != http.StatusOK {
			t.Fatalf("GET /notifications/ = %d", code)
		}
		return body
	}
	only := func(body, want string) {
		t.Helper()
		for _, icon := range []string{iconMail, iconPlanet, iconGeneric} {
			if has := strings.Contains(body, icon); has != (icon == want) {
				t.Fatalf("icon %q present=%v, want only %q in the top bar", icon, has, want)
			}
		}
	}

	// Nothing unread: the generic icon, dimmed, no count, and a list
	// that says so.
	body := page()
	only(body, iconGeneric)
	mustContain(t, "/notifications/", body,
		`<details class="notify" id="notify-menu">`, `aria-label="Notifications: nothing new"`,
		`<p class="notify-empty">Nothing new.</p>`, `<a class="notify-all" href="/notifications/">All notifications</a>`)
	if strings.Contains(body, "notify-count") || strings.Contains(body, "has-unread") {
		t.Fatal("a count or the unread state is shown with nothing unread")
	}

	// Only mail unread: the letter, with the count and the newest
	// mail's text. What a stranger wrote in a subject line is text,
	// never markup.
	f.add(f.userID, f.ch.CharacterID, notifyMail, "Fixture Ceo has new mail: first", "/mail/?character=90000001")
	f.add(f.userID, f.ch.CharacterID, notifyMail, `Fixture Ceo has new mail: <script>alert(1)</script>`, "/mail/?character=90000001")
	body = page()
	only(body, iconMail)
	mustContain(t, "/notifications/", body,
		`<details class="notify has-unread" id="notify-menu">`, `aria-label="Notifications: 2 unread"`,
		`<span class="notify-count" aria-hidden="true">2</span>`,
		`<span class="notify-row-label">2 new mails</span>`,
		`<span class="notify-row-latest">Fixture Ceo has new mail: &lt;script&gt;alert(1)&lt;/script&gt;</span>`,
		`<input type="hidden" name="kind" value="mail">`)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("a mail subject reached the page as markup")
	}

	// Two kinds unread: the generic icon stands for both, one row each.
	f.add(f.userID, f.ch.CharacterID, notifyPI, "Fixture Ceo: extractors have stopped on Jita I", "/planets/?character=90000001")
	body = page()
	only(body, iconGeneric)
	mustContain(t, "/notifications/", body,
		`<span class="notify-count" aria-hidden="true">3</span>`,
		`<span class="notify-row-label">2 new mails</span>`,
		`<span class="notify-row-label">1 planet with stopped extractors</span>`)

	// Following the mail row marks mail read and goes to the mail.
	code, where := f.post(cookie, "/notifications/open", url.Values{"kind": {notifyMail}})
	if code != http.StatusSeeOther || where != "/mail/?character=90000001" {
		t.Fatalf("open mail: %d to %q, want 303 to the mail page", code, where)
	}
	if got := f.unread(f.userID); got[notifyMail] != 0 || got[notifyPI] != 1 {
		t.Fatalf("after opening mail, unread = %v; want mail cleared and the planet left", got)
	}
	// Only the planet left: its own icon.
	only(page(), iconPlanet)

	// A kind that does not exist changes nothing and lands on the list.
	code, where = f.post(cookie, "/notifications/open", url.Values{"kind": {"no-such-kind"}})
	if code != http.StatusSeeOther || where != "/notifications/" || f.unread(f.userID)[notifyPI] != 1 {
		t.Fatalf("open of an unknown kind: %d to %q, unread %v", code, where, f.unread(f.userID))
	}
}

func TestNotificationsPageAndMarkRead(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Alt")

	code, body := getPage(t, f.app, cookie, "/notifications/")
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	mustContain(t, "/notifications/", body, "Nothing yet.")

	f.add(f.userID, f.ch.CharacterID, notifySkill, "Fixture Ceo finished training Mechanics V", "/character/?character=90000001")
	f.add(f.userID, f.ch.CharacterID, notifyKillmail, "Fixture Ceo has a new killmail", "/killmails/?character=90000001")
	f.add(other.ID, fixtureCharB, notifyMail, "Somebody else's mail", "/mail/?character=90000002")

	_, body = getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "/notifications/", body,
		`<a href="/character/?character=90000001">Fixture Ceo finished training Mechanics V</a> <small>new</small>`,
		`<td>Skill complete</td>`, `<td>New killmail</td>`, `Oct 8, 12:00 UTC`,
		`<small>2 unread</small>`, `<button type="submit">Mark all read</button>`)
	if strings.Contains(body, "Somebody else") {
		t.Fatal("another account's notification is shown")
	}

	if code, where := f.post(cookie, "/notifications/read", url.Values{}); code != http.StatusSeeOther || where != "/notifications/" {
		t.Fatalf("mark all read: %d to %q", code, where)
	}
	if got := f.unread(f.userID); len(got) != 0 {
		t.Fatalf("after mark all read, unread = %v", got)
	}
	if got := f.unread(other.ID); got[notifyMail] != 1 {
		t.Fatalf("marking one account's notifications read touched another's: %v", got)
	}
	_, body = getPage(t, f.app, cookie, "/notifications/")
	if strings.Contains(body, "<small>new</small>") || strings.Contains(body, "Mark all read") {
		t.Fatal("read notifications are still marked new, or the button is still offered")
	}
	mustContain(t, "/notifications/", body, "Fixture Ceo finished training Mechanics V")

	// Signed out, none of it is reachable.
	for _, path := range []string{"/notifications/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("GET %s signed out = 200", path)
		}
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("%d outbound call(s) from the notification pages", got)
	}
}

// TestNotifyTargetStaysOnTheSite: a notification only ever leads to
// one of the app's own pages.
func TestNotifyTargetStaysOnTheSite(t *testing.T) {
	for in, want := range map[string]string{
		"/mail/?character=1":   "/mail/?character=1",
		"/market/":             "/market/",
		"":                     "/notifications/",
		"https://evil.example": "/notifications/",
		"//evil.example/x":     "/notifications/",
		"/\\evil.example":      "/notifications/",
		"javascript:alert(1)":  "/notifications/",
		"mail/":                "/notifications/",
	} {
		if got := notifyTarget(in); got != want {
			t.Errorf("notifyTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNotificationSettings: one switch per kind, saved with the
// other per-user settings, and honoured by the worker's next pass.
// Beside each kind the page lists the account's characters, each with
// a switch of its own.
func TestNotificationSettings(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	// A second character that has granted mail but not killmails.
	alt := seedCharacter(t, f.q, f.userID, fixtureCharB, "Fixture Alt")
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET scopes = $1 WHERE character_id = $2`,
		"esi-mail.read_mail.v1", alt.CharacterID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET scopes = $1 WHERE character_id = $2`,
		"esi-mail.read_mail.v1 esi-killmails.read_killmails.v1", f.ch.CharacterID); err != nil {
		t.Fatal(err)
	}

	code, body := getPage(t, f.app, cookie, "/notifications/settings")
	if code != http.StatusOK {
		t.Fatalf("GET settings = %d", code)
	}
	// Everything is on to begin with.
	for _, kind := range notifyKinds {
		mustContain(t, "/notifications/settings", body,
			`<input type="checkbox" name="kind" value="`+kind.ID+`" checked> `+kind.Title)
	}
	mustContain(t, "/notifications/settings", body,
		`placeholder="On for 2 of 2 characters"`,                    // mail: both
		`placeholder="On for 1 of 2 characters (1 without access)"`, // killmails: the alt has not granted it
		`<input type="checkbox" name="char.mail" value="90000002" checked> <span class="charselector-name">Fixture Alt</span>`,
		`data-name="Fixture Alt"><span class="charselector-name">Fixture Alt</span> <span class="charselector-tags">no access`,
		"Your account") // the watch list needs no character
	if !strings.Contains(body, `/auth/eve?`) && !strings.Contains(body, "Sign in again") {
		t.Fatal("no way offered to grant the missing access")
	}

	// Switch off mail and killmails: the form sends the ones left on.
	var keep []string
	for _, kind := range notifyKinds {
		if kind.ID != notifyMail && kind.ID != notifyKillmail {
			keep = append(keep, kind.ID)
		}
	}
	if code, where := f.post(cookie, "/notifications/settings", url.Values{"kind": append(keep, "no-such-kind")}); code != http.StatusSeeOther || where != "/notifications/settings" {
		t.Fatalf("save: %d to %q", code, where)
	}
	prefs := f.app.notifyPrefsFor(f.ctx, f.userID)
	if len(prefs.Off) != 2 || !prefs.off(notifyMail) || !prefs.off(notifyKillmail) {
		t.Fatalf("stored settings switch off %q, want mail and killmail", prefs.Off)
	}
	_, body = getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "/notifications/settings", body,
		"Notification settings saved.",
		`<input type="checkbox" name="kind" value="mail"> New mail`,
		`<input type="checkbox" name="kind" value="skill" checked> Skill complete`)

	// Another account's settings are its own.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := f.app.notifyPrefsFor(f.ctx, other.ID); len(p.Off) != 0 {
		t.Fatalf("a second account inherited settings: %q", p.Off)
	}
}

// TestNotificationSettingsPerCharacter: under each kind every
// character has its own switch. A kind switched off for one character
// stays on for the others; a character that could not be ticked (no
// access) is left as it was; the worker honours it; and a character
// linked later starts with everything on.
func TestNotificationSettingsPerCharacter(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	alt := seedCharacter(t, f.q, f.userID, fixtureCharB, "Fixture Alt")
	scopes := func(characterID int64, granted string) {
		t.Helper()
		if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET scopes = $1 WHERE character_id = $2`, granted, characterID); err != nil {
			t.Fatal(err)
		}
	}
	scopes(f.ch.CharacterID, "esi-mail.read_mail.v1 esi-killmails.read_killmails.v1")
	scopes(alt.CharacterID, "esi-mail.read_mail.v1") // no killmail access
	a, b := "90000001", "90000002"

	// Every kind on; mail only for the first character. The alt's
	// killmail box cannot be ticked, so the form does not carry it.
	form := url.Values{}
	for _, kind := range notifyKinds {
		form.Add("kind", kind.ID)
		if !kind.Account {
			form.Add("char."+kind.ID, a)
			if kind.ID != notifyMail && kind.ID != notifyKillmail {
				form.Add("char."+kind.ID, b)
			}
		}
	}
	form.Add("char."+notifyMail, "95000009") // not this account's: ignored
	form.Add("char."+notifyWatch, a)         // an account kind has no characters
	if code, _ := f.post(cookie, "/notifications/settings", form); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	prefs := f.app.notifyPrefsFor(f.ctx, f.userID)
	if len(prefs.Off) != 0 {
		t.Fatalf("kinds switched off: %q, want none", prefs.Off)
	}
	if !prefs.offFor(notifyMail, alt.CharacterID) || prefs.offFor(notifyMail, f.ch.CharacterID) {
		t.Fatalf("mail is off for %v, want the alt only", prefs.OffFor[notifyMail])
	}
	if prefs.offFor(notifyKillmail, alt.CharacterID) {
		t.Fatal("killmails were switched off for a character that could not be ticked")
	}
	if len(prefs.OffFor) != 1 {
		t.Fatalf("stored per-character settings %v, want only mail for the alt", prefs.OffFor)
	}

	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings after saving", body,
		`placeholder="On for 1 of 2 characters"`,
		`<input type="checkbox" name="char.mail" value="90000001" checked> <span class="charselector-name">Fixture Ceo</span>`,
		`<input type="checkbox" name="char.mail" value="90000002"> <span class="charselector-name">Fixture Alt</span>`,
		`<input type="checkbox" name="kind" value="mail" checked> New mail`)

	// The worker: mail to both characters, announced for one.
	now := notifyT0
	seedSnapshot(t, f.q, f.ch.CharacterID, esi.SnapMail, esi.MailHeaders{})
	seedSnapshot(t, f.q, alt.CharacterID, esi.SnapMail, esi.MailHeaders{})
	f.pass(now)
	later := now.Add(10 * time.Minute)
	seedSnapshot(t, f.q, f.ch.CharacterID, esi.SnapMail, esi.MailHeaders{{MailID: 1, From: 555, Subject: "For the main", Timestamp: rfc(later)}})
	seedSnapshot(t, f.q, alt.CharacterID, esi.SnapMail, esi.MailHeaders{{MailID: 2, From: 555, Subject: "For the alt", Timestamp: rfc(later)}})
	f.pass(later)
	f.wantTitles("mail with the alt switched off", "For the main")

	// Switching the alt back on starts from then, not from the mail
	// that arrived meanwhile.
	form.Add("char."+notifyMail, b)
	f.post(cookie, "/notifications/settings", form)
	if prefs = f.app.notifyPrefsFor(f.ctx, f.userID); len(prefs.OffFor) != 0 {
		t.Fatalf("after ticking the alt again: %v", prefs.OffFor)
	}
	f.pass(later.Add(10 * time.Minute))
	f.wantTitles("after switching the alt back on", "For the main")

	// A character linked afterwards has everything on.
	third := seedCharacter(t, f.q, f.userID, 90000003, "Fixture Third")
	if f.app.notifyPrefsFor(f.ctx, f.userID).offFor(notifyMail, third.CharacterID) {
		t.Fatal("a newly linked character started with mail off")
	}
}

// TestNotifyNewOps: an op planned for a corporation the account has a
// character in is announced once, to that character; the ops already
// on the calendar when it is first read are not news, and neither is
// an op the account planned itself, a cancelled one, or one for a
// corporation it has no character in.
func TestNotifyNewOps(t *testing.T) {
	f := newNotifyFixture(t)
	const corp, elsewhere = int64(98000001), int64(98000002)
	join := func(characterID, corporationID int64) {
		t.Helper()
		if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
			CharacterID: characterID, CorporationID: corporationID, UpdatedAt: notifyT0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(corporationID, by int64, title string, start time.Time) int64 {
		t.Helper()
		id, err := f.q.CreateOp(f.ctx, db.CreateOpParams{
			CorporationID: corporationID, Title: title, StartsAt: start, DurationMinutes: 60,
			FcCharacterID: by, CreatedByCharacter: by, CreatedAt: notifyT0,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	join(f.ch.CharacterID, corp)
	now := notifyT0
	const director = int64(95000001) // somebody else in the corporation

	// Already planned when the account's calendar is first read.
	plan(corp, director, "Planned before", now.Add(48*time.Hour))
	if n := f.pass(now); n != 0 {
		t.Fatalf("first pass announced %d, want 0: %q", n, f.titles())
	}

	later := now.Add(10 * time.Minute)
	fresh := plan(corp, director, "Structure bash", time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC))
	plan(corp, f.ch.CharacterID, "My own op", now.Add(72*time.Hour))
	plan(elsewhere, director, "Not my corporation", now.Add(72*time.Hour))
	plan(corp, director, "Long past", now.Add(-24*time.Hour))
	plan(corp, director, "Next year", now.Add(400*24*time.Hour))
	cancelled := plan(corp, director, "Called off", now.Add(72*time.Hour))
	if err := f.q.SetOpCancelled(f.ctx, db.SetOpCancelledParams{ID: cancelled, CancelledAt: timeSet(later)}); err != nil {
		t.Fatal(err)
	}
	if n := f.pass(later); n != 1 {
		t.Fatalf("announced %d, want the one new op: %q", n, f.titles())
	}
	f.wantTitles("a new op", "Structure bash, Oct 12 19:00")
	rows, err := f.q.ListNotifications(f.ctx, db.ListNotificationsParams{UserID: f.userID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Kind != notifyOp || rows[0].Url != opURL(fresh) || rows[0].CharacterID != f.ch.CharacterID {
		t.Fatalf("stored as %+v, want kind op, the op page, and the character in that corporation", rows[0])
	}
	// Once only.
	if n := f.pass(later.Add(10 * time.Minute)); n != 0 {
		t.Fatalf("announced again: %q", f.titles())
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("the pass asked ESI %d time(s)", got)
	}

	// Switched off for the only character in the corporation: quiet,
	// and still quiet about that op after switching back on.
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	form := url.Values{}
	for _, kind := range notifyKinds {
		form.Add("kind", kind.ID)
		if !kind.Account && kind.ID != notifyOp {
			form.Add("char."+kind.ID, "90000001")
		}
	}
	f.post(cookie, "/notifications/settings", form)
	plan(corp, director, "While switched off", now.Add(96*time.Hour))
	if n := f.pass(later.Add(20 * time.Minute)); n != 0 {
		t.Fatalf("announced an op for a character that switched ops off: %q", f.titles())
	}
	form.Add("char."+notifyOp, "90000001")
	f.post(cookie, "/notifications/settings", form)
	plan(corp, director, "After switching on", now.Add(120*time.Hour))
	f.pass(later.Add(30 * time.Minute))
	f.wantTitles("ops after switching back on", "Structure bash", "After switching on")

	// The settings page lists it and the bell words it.
	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings", body, `value="op" checked> New op on the calendar`, `value="calendar" checked> New in-game calendar event`)
	mustContain(t, "bell", f.badge(cookie, "").Body.String(), "2 new ops")
}

// TestNotifyOpReminder: an account that signed up to an op, as coming
// or maybe, is reminded once when its start is within the lead the
// account chose. Nobody else is: not an account that did not sign up,
// nor one that answered "not coming". A cancelled op is not reminded
// of, and a moved one is again. The lead is chosen on the settings
// page.
func TestNotifyOpReminder(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	const corp = int64(98000001)
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: f.ch.CharacterID, CorporationID: corp, UpdatedAt: notifyT0,
	}); err != nil {
		t.Fatal(err)
	}
	plan := func(title string, start time.Time) int64 {
		t.Helper()
		id, err := f.q.CreateOp(f.ctx, db.CreateOpParams{
			CorporationID: corp, Title: title, StartsAt: start, DurationMinutes: 60,
			FcCharacterID: f.ch.CharacterID, CreatedByCharacter: f.ch.CharacterID, CreatedAt: notifyT0,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	answer := func(op int64, response string) {
		t.Helper()
		if code, _ := f.post(cookie, opURL(op)+"/signup", url.Values{"character": {"90000001"}, "response": {response}}); code != http.StatusSeeOther {
			t.Fatalf("sign-up %q: %d", response, code)
		}
	}
	// Noon tomorrow, by the real clock: signing up is refused for an op
	// that is over, and the sign-up page goes by the real time.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(36 * time.Hour)
	start := now.Add(2 * time.Hour)

	// The account's own op (so no "new op"), two hours away, and
	// another at the same time that it does not sign up to. The
	// default lead is 30 minutes.
	op := plan("Home defence", start)
	plan("Not signed up to", start)
	f.pass(now)
	if n := f.pass(start.Add(-25 * time.Minute)); n != 0 {
		t.Fatalf("reminded of ops the account has not signed up to: %q", f.titles())
	}
	// Signing up inside the lead brings the reminder at the next look.
	answer(op, "yes")
	if n := f.pass(start.Add(-24 * time.Minute)); n != 1 {
		t.Fatalf("after signing up inside the lead: %d reminder(s) %q, want 1", n, f.titles())
	}

	// Signed up well ahead: at the lead, and not before.
	start = start.Add(24 * time.Hour)
	op = plan("Home defence", start)
	answer(op, "yes")
	if n := f.pass(start.Add(-31 * time.Minute)); n != 0 {
		t.Fatalf("reminded 31 minutes before with a 30 minute lead: %q", f.titles())
	}
	if n := f.pass(start.Add(-30 * time.Minute)); n != 1 {
		t.Fatalf("at 30 minutes before: %d reminder(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("the reminders", "starts at 14:00 EVE time: Home defence", "starts at 14:00 EVE time: Home defence")
	rows, err := f.q.ListNotifications(f.ctx, db.ListNotificationsParams{UserID: f.userID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Kind != notifyOpReminder || rows[0].Url != opURL(op) || rows[0].CharacterID != f.ch.CharacterID {
		t.Fatalf("stored as %+v, want an op reminder linking to the op, for the character that signed up", rows[0])
	}
	// Once, and not after it has started.
	if n := f.pass(start.Add(-10 * time.Minute)); n != 0 {
		t.Fatalf("reminded twice: %q", f.titles())
	}
	if n := f.pass(start.Add(time.Minute)); n != 0 {
		t.Fatalf("reminded after the start: %q", f.titles())
	}

	// Moved to a later time: reminded of again, at the new time.
	moved := start.Add(3 * time.Hour)
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE ops SET starts_at = $1 WHERE id = $2`, moved, op); err != nil {
		t.Fatal(err)
	}
	if n := f.pass(moved.Add(-20 * time.Minute)); n != 1 {
		t.Fatalf("after the op was moved: %d reminder(s), want 1: %q", n, f.titles())
	}

	// A longer lead, chosen on the settings page.
	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings", body,
		`value="op_reminder" checked> Op starting soon</label>`,
		`<select name="op_reminder_minutes"`, `<option value="30" selected>30 minutes</option>`, `<option value="120">2 hours</option>`)
	form := url.Values{"op_reminder_minutes": {"120"}}
	for _, kind := range notifyKinds {
		form.Add("kind", kind.ID)
		if !kind.Account {
			form.Add("char."+kind.ID, "90000001")
		}
	}
	f.post(cookie, "/notifications/settings", form)
	if got := f.app.notifyPrefsFor(f.ctx, f.userID).reminderLead(); got != 2*time.Hour {
		t.Fatalf("lead after saving 120: %v", got)
	}
	_, body = getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings after saving", body, `<option value="120" selected>2 hours</option>`)
	// A lead that is not offered is not stored.
	form.Set("op_reminder_minutes", "7")
	f.post(cookie, "/notifications/settings", form)
	if got := f.app.notifyPrefsFor(f.ctx, f.userID).reminderLead(); got != 2*time.Hour {
		t.Fatalf("lead after posting 7: %v, want the 2 hours kept", got)
	}

	day := moved.Add(24 * time.Hour)
	answer(plan("Roam", day), "maybe")
	if n := f.pass(day.Add(-119 * time.Minute)); n != 1 {
		t.Fatalf("with a 2 hour lead, 119 minutes before: %d reminder(s), want 1: %q", n, f.titles())
	}

	// "Not coming": no reminder. Changing the answer brings it back.
	third := plan("Fleet nobody wants", day.Add(24*time.Hour))
	answer(third, "no")
	at := day.Add(24*time.Hour - time.Hour)
	if n := f.pass(at); n != 0 {
		t.Fatalf("reminded of an op the account said no to: %q", f.titles())
	}
	answer(third, "maybe")
	if n := f.pass(at.Add(time.Minute)); n != 1 {
		t.Fatalf("after changing the answer to maybe: %d reminder(s), want 1", n)
	}

	// Cancelled: none.
	fourth := plan("Called off", day.Add(48*time.Hour))
	answer(fourth, "yes")
	if err := f.q.SetOpCancelled(f.ctx, db.SetOpCancelledParams{ID: fourth, CancelledAt: timeSet(at)}); err != nil {
		t.Fatal(err)
	}
	if n := f.pass(day.Add(48*time.Hour - time.Hour)); n != 0 {
		t.Fatalf("reminded of a cancelled op: %q", f.titles())
	}

	// Switched off for the character: none.
	form.Del("char." + notifyOpReminder)
	f.post(cookie, "/notifications/settings", form)
	answer(plan("Quiet", day.Add(72*time.Hour)), "yes")
	if n := f.pass(day.Add(72*time.Hour - time.Hour)); n != 0 {
		t.Fatalf("reminded a character that switched reminders off: %q", f.titles())
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("the pass asked ESI %d time(s)", got)
	}
}
