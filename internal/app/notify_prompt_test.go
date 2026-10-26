package app

// The first-run notification prompt: the card that drops from the
// bell once for an account that has not answered, what its answer
// stores, and how the channels it sets gate what the worker sends.

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

// The card's two faces in the page: rendered open (no hidden) and
// rendered closed.
const (
	promptOpen   = `aria-label="Notification choices">`
	promptClosed = `aria-label="Notification choices" hidden>`
)

// session mints a fresh signed-in session for the fixture's
// account, as a new browser would arrive with.
func (f *notifyFixture) session() *http.Cookie {
	f.t.Helper()
	return sessionCookie(f.t, f.app, f.userID, f.ch.CharacterID, "Fixture Ceo")
}

func (f *notifyFixture) prefs() notifyPrefs {
	f.t.Helper()
	return f.app.notifyPrefsFor(f.ctx, f.userID)
}

func TestNotifyPromptDropsOnceForTheUnanswered(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := f.session()

	_, body := getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "first view", body, promptOpen, "Do you want to turn on notifications?")
	if strings.Contains(body, promptClosed) {
		t.Fatal("the first view rendered the prompt both open and closed")
	}

	// The same visit does not drop it again on the next page.
	_, body = getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "second page of the visit", body, promptClosed)

	// A new visit (a new session) is shown it again: the account
	// has not answered yet.
	cookie = f.session()
	_, body = getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "a new visit", body, promptOpen)

	// "Not now" is an answer: stored, and the card never drops
	// unasked again, in any session.
	code, _ := f.post(cookie, "/notifications/prompt", url.Values{"action": {"dismiss"}})
	if code != http.StatusSeeOther {
		t.Fatalf("dismiss = %d, want 303", code)
	}
	if prefs := f.prefs(); !prefs.PromptAnswered || len(prefs.OffChannels) != 0 {
		t.Fatalf("after dismiss: answered=%v channels off=%v, want answered and no channel change", prefs.PromptAnswered, prefs.OffChannels)
	}
	_, body = getPage(t, f.app, f.session(), "/notifications/")
	mustContain(t, "after dismiss", body, promptClosed)
}

func TestNotifyPromptAnswerStoresTheChannels(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := f.session()

	// Push off on this server: the browser channel is not offered,
	// and an answer stores only what was asked. In-app left
	// unticked is off.
	code, _ := f.post(cookie, "/notifications/prompt", url.Values{
		"action": {"save"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("answer = %d, want 303", code)
	}
	prefs := f.prefs()
	if !prefs.PromptAnswered {
		t.Fatal("the answer was not remembered")
	}
	if !prefs.channelOff(notifyChannelInApp) {
		t.Fatal("in-app was left unticked but is not off")
	}
	if prefs.channelOff(notifyChannelDesktop) {
		t.Fatal("the browser channel was never offered, but the answer switched it off")
	}
	// Signed out, the endpoint answers nothing: a bare redirect.
	req := httptest.NewRequest(http.MethodPost, "/notifications/prompt",
		strings.NewReader(url.Values{"action": {"save"}, "inapp": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Fatalf("signed-out answer = %d, want a redirect", rec.Code)
	}

	// The settings page shows the same state, and its own save can
	// change it back — one store, two writers.
	_, body := getPage(t, f.app, cookie, "/settings")
	mustContain(t, "settings after the answer", body,
		`<input type="checkbox" name="channel_inapp" value="1">`,
		"In EveSynapse")
	code, _ = f.post(cookie, "/notifications/settings", url.Values{
		"channels_shown": {"1"},
		"channel_inapp":  {"1"},
		"kind_watch":     {"1"}, "kind_skill": {"1"}, "kind_mail": {"1"}, "kind_calendar": {"1"},
		"kind_killmail": {"1"}, "kind_pi": {"1"}, "kind_industry": {"1"}, "kind_op": {"1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("settings save = %d, want 303", code)
	}
	if prefs := f.prefs(); prefs.channelOff(notifyChannelInApp) || !prefs.PromptAnswered {
		t.Fatalf("after the settings save: in-app off=%v answered=%v, want on and still answered",
			prefs.channelOff(notifyChannelInApp), prefs.PromptAnswered)
	}
}

func TestNotifyPromptDiscordSwitchWritesTheLink(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := f.session()
	if err := f.q.UpsertDiscordLink(f.ctx, db.UpsertDiscordLinkParams{
		UserID: f.userID, DiscordID: "424242", Username: "fixturepilot", LinkedAt: notifyT0,
	}); err != nil {
		t.Fatal(err)
	}
	dm := func() bool {
		f.t.Helper()
		link, err := f.q.GetDiscordLink(f.ctx, f.userID)
		if err != nil {
			f.t.Fatal(err)
		}
		return link.DmNotifications
	}
	if dm() {
		t.Fatal("a fresh link starts with direct messages on; the fixture is wrong")
	}
	// The prompt offers the row, naming the linked account.
	_, body := getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "prompt with discord linked", body, "On Discord", "direct messages to fixturepilot")

	code, _ := f.post(cookie, "/notifications/prompt", url.Values{
		"action": {"save"}, "inapp": {"1"}, "discord_shown": {"1"}, "discord": {"1"},
	})
	if code != http.StatusSeeOther || !dm() {
		t.Fatalf("answer = %d, dm on = %v; want 303 and on", code, dm())
	}
	// And back off, the same way.
	code, _ = f.post(cookie, "/notifications/prompt", url.Values{
		"action": {"save"}, "inapp": {"1"}, "discord_shown": {"1"},
	})
	if code != http.StatusSeeOther || dm() {
		t.Fatalf("second answer = %d, dm on = %v; want 303 and off", code, dm())
	}
}

func TestNotifyChannelsGateDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := f.session()
	service := f.enablePush()
	now := notifyT0
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	f.pass(now) // baseline
	if code, msg := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+"prompt-gate")); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %q", code, msg)
	}

	// Both channels off: the news is not stored, not pushed — and
	// still recorded as seen.
	if err := f.app.storeNotifyPrefs(f.ctx, f.userID, notifyPrefs{OffChannels: []string{notifyChannelInApp, notifyChannelDesktop}}); err != nil {
		t.Fatal(err)
	}
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 1}})
	f.pass(now.Add(time.Minute))
	if got := f.titles(); len(got) != 0 {
		t.Fatalf("in-app off stored %q, want nothing", got)
	}
	if service.count() != 0 {
		t.Fatalf("desktop off pushed %d message(s), want none", service.count())
	}

	// Back on: what happened while off is not replayed.
	if err := f.app.storeNotifyPrefs(f.ctx, f.userID, notifyPrefs{}); err != nil {
		t.Fatal(err)
	}
	f.pass(now.Add(2 * time.Minute))
	if got := f.titles(); len(got) != 0 {
		t.Fatalf("switching back on replayed %q, want nothing", got)
	}
	if service.count() != 0 {
		t.Fatalf("switching back on pushed %d message(s), want none", service.count())
	}

	// New news flows on both channels again.
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 2}, {KillmailID: 1}})
	f.pass(now.Add(3 * time.Minute))
	if got := f.titles(); len(got) != 1 {
		t.Fatalf("in-app on stored %q, want the one new notification", got)
	}
	if service.count() != 1 {
		t.Fatalf("desktop on pushed %d message(s), want 1", service.count())
	}
}

func TestNotifyPromptStaysAwayFromVisitors(t *testing.T) {
	f := newNotifyFixture(t)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/about/", nil))
	code, body := rec.Code, rec.Body.String()
	if code != http.StatusOK {
		t.Fatalf("GET /about/ = %d", code)
	}
	if strings.Contains(body, "notify-prompt") || strings.Contains(body, "Notification choices") {
		t.Fatal("a signed-out visitor was shown the notification prompt")
	}
}
