package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseNotifyPoll(t *testing.T) {
	for raw, want := range map[string]int{
		"":     defaultNotifyPoll,
		"  ":   defaultNotifyPoll,
		"30":   30,
		" 45 ": 45,
		"0":    0, // off
		"1":    minNotifyPoll,
		"5":    5,
		"3600": 3600,
		"9999": maxNotifyPoll,
	} {
		if got, ok := parseNotifyPoll(raw); got != want || !ok {
			t.Errorf("parseNotifyPoll(%q) = %d, %v; want %d, true", raw, got, ok, want)
		}
	}
	// What cannot be read is said, and the default is used.
	for _, raw := range []string{"soon", "30s", "1.5", "-5"} {
		if got, ok := parseNotifyPoll(raw); got != defaultNotifyPoll || ok {
			t.Errorf("parseNotifyPoll(%q) = %d, %v; want the default and false", raw, got, ok)
		}
	}
}

// badge asks for the icon's contents the way an open page does.
func (f *notifyFixture) badge(cookie *http.Cookie, etag string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, notifyBadgePath, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec
}

// TestNotifyBadgeEndpoint: an open page gets the icon's contents, the
// same ones a page load draws; an unchanged icon is answered with no
// body; a new notification changes the answer; and a visitor who is
// not signed in gets nothing that could be taken for an icon.
func TestNotifyBadgeEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	empty := f.badge(cookie, "")
	if empty.Code != http.StatusOK || empty.Header().Get(notifyBadgeHeader) != "1" || empty.Header().Get("X-Notify-Unread") != "0" {
		t.Fatalf("empty badge: %d, headers %v", empty.Code, empty.Header())
	}
	mustContain(t, "empty badge", empty.Body.String(), `aria-label="Notifications: nothing new"`, "Nothing new.", `href="/notifications/settings"`)
	if cc := empty.Header().Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q; the answer is one account's own and must be checked each time", cc)
	}
	etag := empty.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	// Nothing changed: no body.
	same := f.badge(cookie, etag)
	if same.Code != http.StatusNotModified || same.Body.Len() != 0 {
		t.Fatalf("unchanged badge: %d with %d bytes, want 304 and none", same.Code, same.Body.Len())
	}

	// A notification arrives: the answer changes, and it is what the
	// page itself would draw.
	f.add(f.userID, f.ch.CharacterID, notifyMail, "Fixture Ceo has new mail: Hello", "/mail/?character=90000001")
	fresh := f.badge(cookie, etag)
	if fresh.Code != http.StatusOK || fresh.Header().Get("X-Notify-Unread") != "1" || fresh.Header().Get("ETag") == etag {
		t.Fatalf("badge after a notification: %d, unread %q, etag %q (was %q)", fresh.Code, fresh.Header().Get("X-Notify-Unread"), fresh.Header().Get("ETag"), etag)
	}
	fragment := fresh.Body.String()
	mustContain(t, "badge after a notification", fragment,
		`aria-label="Notifications: 1 unread"`, "1 new mail", "Fixture Ceo has new mail: Hello", `action="/notifications/open"`)
	if strings.Contains(fragment, "<details") || strings.Contains(fragment, "<html") {
		t.Error("the answer is more than the icon's contents")
	}
	_, page := getPage(t, f.app, cookie, "/notifications/")
	if !strings.Contains(page, strings.TrimSpace(fragment)) {
		t.Error("the page draws the icon differently from what the check returns")
	}

	// Another account's notifications are not this one's.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Alt")
	theirs := f.badge(sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Alt"), "")
	if theirs.Header().Get("X-Notify-Unread") != "0" || strings.Contains(theirs.Body.String(), "Hello") {
		t.Fatal("another account was shown this account's notification")
	}

	// Signed out: sent away, and not marked as an icon.
	anon := f.badge(nil, "")
	if anon.Code != http.StatusSeeOther || anon.Header().Get(notifyBadgeHeader) != "" {
		t.Fatalf("signed out: %d, marked %q; want a redirect and no mark", anon.Code, anon.Header().Get(notifyBadgeHeader))
	}
}

// TestPagesCarryThePollInterval: the page tells its script how often
// to check, and says nothing when checking is off.
func TestPagesCarryThePollInterval(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	_, body := getPage(t, f.app, cookie, "/notifications/")
	if strings.Contains(body, "data-poll") {
		t.Fatal("a page asks for checks although NOTIFY_POLL_SECONDS is off")
	}
	f.app.cfg.notifyPoll = 30
	_, body = getPage(t, f.app, cookie, "/notifications/")
	mustContain(t, "page with checks on", body, `<details class="notify" id="notify-menu" data-poll="30">`)
}
