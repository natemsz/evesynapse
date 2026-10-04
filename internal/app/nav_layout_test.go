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
		`<button type="button" class="nav-expand-btn" id="nav-expand-all" aria-label="Expand all navigation categories" aria-expanded="false">Expand all</button>`,
		`<button type="button" class="nav-state-btn" id="nav-collapse"`,
		`<button type="button" class="nav-state-btn" id="nav-hide"`,
		`<label class="nav-drawer-close" for="nav-drawer-toggle" role="button" tabindex="0" aria-label="Close navigation">×</label>`,
		`id="nav-drawer-toggle" autocomplete="off"`,
		`<button type="button" class="nav-reopen" id="nav-reopen"`,
		`<a class="wordmark topbar-wordmark" href="/">EVESYNAPSE</a>`,
		`data-nav-category="character"`,
		`data-nav-category="corporation"`,
		`<span class="nav-label">Character</span>`,
		`<span class="nav-label">Corporation</span>`,
		`<span class="nav-icon nav-icon-bars" aria-hidden="true">▂▄▆</span><span class="nav-label">Economy</span>`,
		`<span class="nav-icon" aria-hidden="true">★</span><span class="nav-label">Corporation</span>`,
		`<div class="topbar-character">`,
		`<span class="topbar-character-name">`,
		`<details class="branch switcher topbar-switcher">`,
		`<a class="sidebar-signout" href="/auth/logout">Sign out</a>`,
		`id="topbar-q"`,
		`id="topbar-suggest"`,
		`id="page-sync-indicator"`)

	// Nav categories are independent now: no shared exclusive
	// details group remains anywhere in the shell.
	if strings.Contains(body, `name="mainnav"`) {
		t.Error("page still carries the exclusive mainnav details group")
	}
	// Wide top bar, left to right at the right edge: page-sync
	// indicator, global search, then the active character block.
	indicatorAt := strings.Index(body, `id="page-sync-indicator"`)
	searchAt := strings.Index(body, `id="topbar-q"`)
	characterAt := strings.Index(body, `<div class="topbar-character">`)
	if indicatorAt < 0 || searchAt < 0 || characterAt < 0 || !(indicatorAt < searchAt && searchAt < characterAt) {
		t.Errorf("topbar cluster order = indicator %d, search %d, character %d; want indicator < search < character", indicatorAt, searchAt, characterAt)
	}
	// The wordmark lives in the top bar (always visible on wide
	// screens); the sidebar no longer carries a brand duplicate,
	// and sign-out stays at the sidebar's bottom.
	asideStart := strings.Index(body, `<aside class="sidebar"`)
	asideEnd := strings.Index(body, `</aside>`)
	if asideStart < 0 || asideEnd < asideStart {
		t.Fatal("sidebar markup not found")
	}
	aside := body[asideStart:asideEnd]
	if strings.Contains(aside, "sidebar-wordmark") || strings.Contains(aside, "EVESYNAPSE") {
		t.Error("sidebar still carries the wordmark")
	}
	if !strings.Contains(aside, `<a class="sidebar-signout" href="/auth/logout">Sign out</a>`) {
		t.Error("sidebar is missing its bottom sign-out link")
	}
	topbarStart := strings.Index(body, `<nav class="topbar">`)
	if topbarStart < 0 {
		t.Fatal("topbar markup not found")
	}
	topbarEnd := strings.Index(body[topbarStart:], `</nav>`)
	if topbarEnd < 0 {
		t.Fatal("topbar markup not closed")
	}
	if !strings.Contains(body[topbarStart:topbarStart+topbarEnd], `<a class="wordmark topbar-wordmark" href="/">EVESYNAPSE</a>`) {
		t.Error("topbar is missing the wordmark")
	}

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
		// v0.3.07.002 drawer fixes: on small screens the state
		// toggles hide, a real close control shows, and the
		// persisted rail presentation is forced back to the
		// full labeled list (labels, account block, inline
		// branch menus) no matter what data-nav says.
		".nav-drawer-close",
		".nav-icon-bars",
		".sidebar .sidebar-actions .nav-state-btn { display: none; }",
		".sidebar .nav-drawer-close { display: inline-flex; }",
		"html[data-nav=\"rail\"] .nav-label { display: block; }",
		"html[data-nav=\"rail\"] .sidebar-account { display: block; }",
		"html[data-nav=\"rail\"] .sidebar .branch .menu {\n    position: static;",
		// v0.3.07.002 top-bar cluster: the wordmark is always in
		// the wide top bar, the active character block sits at
		// its right edge, and both leave the phone bar (the
		// drawer keeps the character block and sign-out).
		".topbar-wordmark { display: inline-block; }",
		".topbar-character {\n  display: flex;",
		".topbar-character { display: none; }",
		".sidebar-account.has-character .sidebar-character { display: none; }",
		".sidebar-account.has-character .sidebar-character { display: block; }",
		".sidebar-signout",
		".topbar > .topsearch { order: 3; }",
		".topbar > .page-sync { order: 4; }",
		// v0.3.07.002 expand-all: shown with scripts on, kept in
		// the drawer even when the persisted rail state applies.
		".nav-expand-btn",
		"html.js .nav-expand-btn { display: inline-flex; }",
		"html[data-nav=\"rail\"] .sidebar .nav-expand-btn { display: inline-flex; }",
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
		"nav-drawer-close",
		"nav-expand-all",
		".sidenav details.branch",
		"Expand all",
		"Collapse all",
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
