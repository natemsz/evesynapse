package app

// The Customize hint: it drops once per session for an account
// that has not answered, and the answer (following it in, saying
// not now, or opening Customize itself) sticks to the account,
// so a fresh session never sees it again.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCustomizePromptShowsOnceAndSticks(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	newAccount := func(charID int64, name string) (int64, *http.Cookie) {
		t.Helper()
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		seedCharacter(t, q, user.ID, charID, name)
		return user.ID, sessionCookie(t, app, user.ID, charID, name)
	}
	postPrompt := func(cookie *http.Cookie, action string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/home/customize-prompt", strings.NewReader(url.Values{"action": {action}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("Location")
	}

	answerNotify := func(cookie *http.Cookie) {
		t.Helper()
		if code, _ := postForm(t, app, cookie, "/notifications/prompt", url.Values{"action": {"dismiss"}}); code != http.StatusSeeOther {
			t.Fatalf("answer notification prompt: code %d, want 303", code)
		}
	}

	// Account A: the hint queues behind the notification prompt,
	// one by one. It does not drop on the first home view while
	// that card is still to show; it drops once the notification
	// is answered, and a "Not now" answer ends it for good.
	userA, cookieA := newAccount(710001, "Hint Alpha")
	if code, body := getPage(t, app, cookieA, "/"); code != http.StatusOK || strings.Contains(body, `id="customize-prompt"`) {
		t.Fatalf("first home view: code %d, hint present %v, want 200 without the hint while the notification is showing", code, strings.Contains(body, `id="customize-prompt"`))
	}
	answerNotify(cookieA)
	if _, body := getPage(t, app, cookieA, "/"); !strings.Contains(body, `id="customize-prompt"`) {
		t.Fatal("home view after answering the notification did not show the hint")
	}
	if _, body := getPage(t, app, cookieA, "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("third home view in the same session showed the hint again")
	}
	if code, _ := postPrompt(cookieA, "dismiss"); code != http.StatusSeeOther {
		t.Fatalf("dismiss: code %d, want 303", code)
	}
	if _, body := getPage(t, app, sessionCookie(t, app, userA, 710001, "Hint Alpha"), "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("fresh session after dismiss showed the hint again")
	}

	// Account B: following the hint in answers it and lands on
	// Customize with the module list open.
	userB, cookieB := newAccount(710002, "Hint Bravo")
	answerNotify(cookieB)
	if _, body := getPage(t, app, cookieB, "/"); !strings.Contains(body, `id="customize-prompt"`) {
		t.Fatal("second account never saw the hint")
	}
	code, loc := postPrompt(cookieB, "try")
	if code != http.StatusSeeOther || loc != "/?customize=1&add=1" {
		t.Fatalf("try: code %d location %q, want 303 to /?customize=1&add=1", code, loc)
	}
	if _, body := getPage(t, app, sessionCookie(t, app, userB, 710002, "Hint Bravo"), "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("fresh session after following the hint showed it again")
	}

	// Account C: opening Customize without ever answering counts
	// as answering.
	userC, cookieC := newAccount(710003, "Hint Charlie")
	if code, _ := getPage(t, app, cookieC, "/?customize=1"); code != http.StatusOK {
		t.Fatalf("customize view: code %d, want 200", code)
	}
	if _, body := getPage(t, app, sessionCookie(t, app, userC, 710003, "Hint Charlie"), "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("account that opened Customize still gets the hint")
	}
}

// The demo account reads the notification prompt's shape without
// the remembered answer: it drops once per login, and nothing is
// saved for it, so every visitor sees it once. It queues behind
// the notification prompt like any account: it waits while that
// card is to show, and drops once it is dismissed, which for demo
// is a session-only dismiss that writes nothing.
func TestCustomizePromptDemoSeesItOncePerLogin(t *testing.T) {
	app, _, _ := demoTestApp(t)
	ctx := context.Background()

	userID := app.demoUserID(ctx)
	if userID == 0 {
		t.Fatal("demo user id is 0 after ensure")
	}
	cookie := sessionCookie(t, app, userID, demoCharacterID, demoCharName)
	dismissNotify := func(cookie *http.Cookie) {
		t.Helper()
		if code, _ := postForm(t, app, cookie, "/notifications/prompt", url.Values{"action": {"dismiss"}}); code != http.StatusSeeOther {
			t.Fatalf("demo dismiss notification prompt: code %d, want 303", code)
		}
	}

	// First home view of the login: the notification is to show,
	// so the hint waits and does not drop yet.
	if code, body := getPage(t, app, cookie, "/"); code != http.StatusOK || strings.Contains(body, `id="customize-prompt"`) {
		t.Fatalf("demo first home view: code %d, hint present %v, want 200 without the hint while the notification is showing", code, strings.Contains(body, `id="customize-prompt"`))
	}
	// Dismissing the notification is a plain dismiss for demo:
	// it writes nothing and does not turn the visitor away.
	dismissNotify(cookie)
	// Home view after the dismiss: the hint drops, once.
	if _, body := getPage(t, app, cookie, "/"); !strings.Contains(body, `id="customize-prompt"`) {
		t.Fatal("demo home view after dismissing the notification did not show the hint")
	}
	// Next home view in the same session: already shown, so it
	// stays down.
	if _, body := getPage(t, app, cookie, "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("demo home view after the hint showed it again in the same session")
	}
	// A fresh login: nothing was saved for demo, so the cycle
	// repeats, notification first, then the hint.
	fresh := sessionCookie(t, app, userID, demoCharacterID, demoCharName)
	if _, body := getPage(t, app, fresh, "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("demo fresh login showed the hint before the notification")
	}
	dismissNotify(fresh)
	if _, body := getPage(t, app, fresh, "/"); !strings.Contains(body, `id="customize-prompt"`) {
		t.Error("demo fresh login did not see the hint again after dismissing the notification")
	}
	// And the seen stamp was never written for demo.
	seen, err := app.queries.GetUserCustomizePromptSeen(ctx, userID)
	if err != nil {
		t.Fatalf("read demo seen stamp: %v", err)
	}
	if seen.Valid {
		t.Error("demo seen stamp was written; it must stay unset")
	}
}
