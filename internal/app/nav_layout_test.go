package app

// Layout tests for the v0.3.07 navigation shell and the
// Chrome/Android visual-stability rules. These are markup and
// stylesheet invariants: the same page must carry the wide
// sidebar, the small-screen drawer hooks, the dark color-scheme
// declaration, and the dedicated fixed wallpaper layer.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestNavigationShellAndVisualStabilityAssets(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, 0, "")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`<meta name="color-scheme" content="dark">`,
		`<input class="nav-drawer-checkbox" type="checkbox" id="nav-drawer-toggle"`,
		`<aside class="sidebar" id="site-nav">`,
		`<nav class="sidenav" aria-label="Primary">`,
		`<label class="nav-hamburger" for="nav-drawer-toggle" role="button" tabindex="0" aria-expanded="false" aria-controls="site-nav">`,
		`<label class="nav-backdrop" for="nav-drawer-toggle" aria-hidden="true"></label>`,
		`<button type="button" class="nav-state-btn" id="nav-collapse"`,
		`<button type="button" class="nav-state-btn" id="nav-hide"`,
		`<button type="button" class="nav-reopen" id="nav-reopen"`,
		`<span class="nav-label">Character</span>`,
		`<span class="nav-label">Corporation</span>`,
		`id="topbar-q"`,
		`id="topbar-suggest"`,
		`id="page-sync-indicator"`)

	code, css := getPage(t, app, cookie, "/static/style.css")
	if code != http.StatusOK {
		t.Fatalf("GET /static/style.css: status %d", code)
	}
	for _, want := range []string{
		"color-scheme: dark;",
		"body::before",
		`background: url("/static/bg.jpg") no-repeat top center;`,
		"transform: translateZ(0);",
		"html[data-nav=\"rail\"] body",
		"html[data-nav=\"hidden\"] .sidebar",
		".nav-drawer-checkbox:checked ~ .sidebar",
		"@media (max-width: 860px)",
		".tw table",
		"width: max-content;",
		"min-width: 100%;",
		"@supports not ((-webkit-background-clip: text) or (background-clip: text))",
		"color: #ffb84d;",
		".grid.home-grid > .card[data-widget=\"briefing\"]",
		".chart-legend",
		".chart-axis-label",
		".pchart .tick-mid",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"background-attachment: fixed",
		`url("/static/bg.jpg") no-repeat top center fixed`,
	} {
		if strings.Contains(css, unwanted) {
			t.Errorf("style.css still contains %q", unwanted)
		}
	}

	code, js := getPage(t, app, cookie, "/static/app.js")
	if code != http.StatusOK {
		t.Fatalf("GET /static/app.js: status %d", code)
	}
	for _, want := range []string{
		"evesynapse-nav",
		"nav-drawer-toggle",
		"nav-collapse",
		"nav-hide",
		"nav-reopen",
		`setAttribute("aria-expanded"`,
		`matchMedia("(max-width: 860px)")`,
		`"Escape"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}
