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

	db "evesynapse/internal/db/sqlc"
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
// Beside each kind the page says how many characters can produce it.
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
		"2 of 2 characters", // mail: both
		"1 of 2 characters", // killmails: the alt has not granted it
		"Fixture Alt has not granted this.",
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
