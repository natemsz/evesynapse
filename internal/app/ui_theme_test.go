package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAccountThemeSaveAndRender: the theme is an account setting.
// Saving posts to /settings/theme, the stored values are rendered
// onto <html> by the server on later loads, and one account's
// theme never appears on another account's pages.
func TestAccountThemeSaveAndRender(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	seedCharacter(t, q, userA.ID, fixtureCharA, "Fixture A")
	seedCharacter(t, q, userB.ID, fixtureCharB, "Fixture B")
	cookieA := sessionCookie(t, app, userA.ID, fixtureCharA, "Fixture A")
	cookieB := sessionCookie(t, app, userB.ID, fixtureCharB, "Fixture B")

	post := func(cookie *http.Cookie, form url.Values) int {
		req := httptest.NewRequest(http.MethodPost, "/settings/theme", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	// Nothing saved yet: the load says so, and no theme attributes
	// are rendered.
	code, body := getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters before save: status %d", code)
	}
	mustContain(t, "no saved theme", body, `<meta name="es-theme-account" content="none">`)
	if strings.Contains(body, "data-scheme=") || strings.Contains(body, "data-backdrop=") {
		t.Fatal("theme attributes rendered before any save")
	}

	// The save the browser's adoption posts lands on the account.
	adopt := url.Values{"scheme": {"jove"}, "tier": {"minimalistic"}, "backdrop": {""}}
	if code := post(cookieA, adopt); code != http.StatusOK {
		t.Fatalf("adoption save: status %d", code)
	}
	if prefs, stored := app.themePrefsFor(ctx, userA.ID); !stored || prefs.Scheme != "jove" || prefs.Tier != "minimalistic" {
		t.Fatalf("stored prefs = %+v, stored=%v; want jove/minimalistic", prefs, stored)
	}

	// Later loads render the account's theme in the server's own
	// HTML, ahead of any script.
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after save: status %d", code)
	}
	mustContain(t, "stored theme rendered", body,
		`<html lang="en" data-nav="rail" data-scheme="jove" data-tier="minimalistic">`)
	mustContain(t, "stored marker", body, `<meta name="es-theme-account" content="stored">`)

	// Account B is untouched by A's theme.
	code, body = getPage(t, app, cookieB, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters for B: status %d", code)
	}
	mustContain(t, "B has no saved theme", body, `<meta name="es-theme-account" content="none">`)
	if strings.Contains(body, "data-scheme=") {
		t.Fatal("account A's theme rendered for account B")
	}

	// A scheme the picker cannot produce is refused, and a return
	// to the default saves as a stored default with its backdrop.
	if code := post(cookieA, url.Values{"scheme": {"neon"}, "tier": {"muted"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown scheme: status %d, want 400", code)
	}
	if code := post(cookieA, url.Values{"scheme": {""}, "tier": {""}, "backdrop": {"muted"}}); code != http.StatusOK {
		t.Fatalf("default save: status %d", code)
	}
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after default save: status %d", code)
	}
	mustContain(t, "default backdrop rendered", body,
		`<html lang="en" data-nav="rail" data-backdrop="muted">`)
	mustContain(t, "default still stored", body, `<meta name="es-theme-account" content="stored">`)
}
