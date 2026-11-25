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

	// Account A: while the notification prompt is unanswered the
	// hint waits; after it, the hint drops on the first home view
	// of a session, not the second, and a "Not now" answer ends
	// it for good.
	userA, cookieA := newAccount(710001, "Hint Alpha")
	if _, body := getPage(t, app, cookieA, "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Fatal("hint dropped beside the unanswered notification prompt")
	}
	answerNotify(cookieA)
	if code, body := getPage(t, app, cookieA, "/"); code != http.StatusOK || !strings.Contains(body, `id="customize-prompt"`) {
		t.Fatalf("first home view: code %d, hint present %v, want 200 with the hint", code, strings.Contains(body, `id="customize-prompt"`))
	}
	if _, body := getPage(t, app, cookieA, "/"); strings.Contains(body, `id="customize-prompt"`) {
		t.Error("second home view in the same session showed the hint again")
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
