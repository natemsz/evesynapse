package tests

// Layout tests for the v0.3.07 navigation shell and the
// Chrome/Android visual-stability rules. These are markup and
// stylesheet invariants: the same page must carry the wide
// sidebar, the small-screen drawer hooks, the dark color-scheme
// declaration, and the dedicated fixed wallpaper layer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

func TestNavigationShellAndVisualStabilityAssets(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Ceo")
	cookie := apptest.SessionCookie(t, rig, user.ID, 0, "")

	code, body := apptest.GetPage(t, rig, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	apptest.MustContain(t, "/", body,
		`<meta name="color-scheme" content="dark">`,
		`<input class="nav-drawer-checkbox" type="checkbox" id="nav-drawer-toggle"`,
		`<aside class="sidebar" id="site-nav">`,
		`<nav class="sidenav" aria-label="Primary">`,
		`<label class="nav-hamburger" for="nav-drawer-toggle" role="button" tabindex="0" aria-expanded="false" aria-controls="site-nav" aria-label="Menu" title="Menu"><svg class="nav-control-svg nav-hamburger-icon" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2.2" stroke-linecap="round"><path d="M4 7h16M4 12h16M4 17h16"/></g></svg></label>`,
		`<label class="nav-backdrop" for="nav-drawer-toggle" aria-hidden="true"></label>`,
		`<button type="button" class="nav-expand-btn" id="nav-expand-all" aria-label="Expand all navigation categories" title="Expand all navigation categories" aria-expanded="false"><span class="expand-icons" aria-hidden="true"><svg class="nav-control-svg expand-icon expand-icon-plus" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="14" height="14" rx="2"/><path d="M12 8.5v7M8.5 12h7"/></g></svg><svg class="nav-control-svg expand-icon expand-icon-minus" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="14" height="14" rx="2"/><path d="M8.5 12h7"/></g></svg></span></button>`,
		`<button type="button" class="nav-state-btn" id="nav-collapse"`,
		`<button type="button" class="nav-state-btn" id="nav-hide"`,
		`<label class="nav-drawer-close" for="nav-drawer-toggle" role="button" tabindex="0" aria-label="Close navigation"><svg class="nav-control-svg" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 7l10 10M17 7L7 17" fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2.2" stroke-linecap="round"/></svg></label>`,
		`id="nav-drawer-toggle" autocomplete="off"`,
		`<button type="button" class="nav-reopen" id="nav-reopen"`,
		`<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">`,
		`<svg class="wordmark-glyph" viewBox="11.8 5.1 24.2 38" aria-hidden="true">`,
		`<defs><linearGradient id="brand-glyph-gradient" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#ffd27a"/><stop offset=".52" stop-color="#ff6a1a"/><stop offset="1" stop-color="#d63c14"/></linearGradient></defs>`,
		`stroke="url(#brand-glyph-gradient)"`,
		`fill="url(#brand-glyph-gradient)"`,
		`<path d="M29.5 15.9L26.4 12.2M20.8 11.6L19.1 13.0M18.7 18.6L20.5 20.6M27.1 27.6L29.1 29.7M28.8 35.2L27.0 36.6M21.4 36.0L18.3 32.1"/>`,
		`<circle cx="31.82" cy="18.76" r="2.7"/>`,
		`<a class="wordmark topbar-wordmark" href="/" aria-label="EveSynapse home"><svg class="wordmark-glyph"`,
		`data-nav-category="pilot"`,
		`data-nav-category="corporation"`,
		`<span class="nav-label">Pilot</span>`,
		`<span class="nav-label">Corporation</span>`,
		`data-tip="Pilot"`,
		`data-tip="Tools"`,
		`data-tip="Home"`,
		`<defs><linearGradient id="nav-glyph-gradient" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="#ffd27a"/><stop offset="0.5" stop-color="#ff6a1a"/><stop offset="1" stop-color="#d63c14"/></linearGradient></defs>`,
		`<svg class="nav-icon-svg nav-icon-home" viewBox="0 0 24 24" aria-hidden="true"><path d="M4.2 11.4 12 4.4l7.8 7M6.6 9.7v9.9h10.8V9.7" fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>`,
		`<svg class="nav-icon-svg nav-icon-character" viewBox="0 0 24 24" aria-hidden="true">`,
		`<circle cx="12" cy="8.2" r="3.8"/>`,
		`<svg class="nav-icon-svg nav-icon-economy" viewBox="0 0 24 24" aria-hidden="true">`,
		`<rect x="16.3" y="7" width="4.2" height="13" rx="1"/>`,
		`<svg class="nav-icon-svg nav-icon-corporation" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.7l2.45 4.97 5.48.8-3.97 3.87.94 5.46L12 16.24l-4.9 2.56.94-5.46L4.07 9.47l5.48-.8Z" fill="url(#nav-glyph-gradient)"/></svg>`,
		`<svg class="nav-icon-svg nav-icon-intel" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2"><circle cx="12" cy="12" r="7.4"/><circle cx="12" cy="12" r="3.6"/></g><circle cx="12" cy="12" r="1.35" fill="url(#nav-glyph-gradient)"/></svg>`,
		`<path d="M14.5 5.5 8 12l6.5 6.5" fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>`,
		`stroke="url(#nav-glyph-gradient)"`,
		`<span class="nav-label">Market</span>`,
		`<div class="topbar-character">`,
		`<span class="topbar-character-name">`,
		`<details class="branch switcher topbar-switcher">`,
		`<a class="navlink sidebar-signout" href="/auth/logout">`,
		`id="topbar-q"`,
		`<svg class="search-glyph" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2" stroke-linecap="round"><circle cx="10.8" cy="10.8" r="6.2"/><path d="M15.2 15.2 20.4 20.4"/></g></svg>`,
		`id="topbar-suggest"`,
		`id="page-sync-indicator"`)

	// Nav categories are independent now: no shared exclusive
	// details group remains anywhere in the shell.
	if strings.Contains(body, `name="mainnav"`) {
		t.Error("page still carries the exclusive mainnav details group")
	}
	// Every category and sidebar control icon is drawn SVG
	// painted from the shared nav gradient, and the retired
	// text glyphs stay retired. The page carries exactly two
	// gradient definitions: the shared horizontal nav def and
	// the brand mark's own diagonal def (the approved mock's
	// ramp — the .007 build painted the mark from the nav
	// def and the nodes landed in the wrong colors), each with
	// a unique id.
	if n := strings.Count(body, "<linearGradient"); n != 2 {
		t.Errorf("page carries %d linearGradient definitions, want 2 (nav + brand defs)", n)
	}
	for _, id := range []string{`id="nav-glyph-gradient"`, `id="brand-glyph-gradient"`} {
		if n := strings.Count(body, id); n != 1 {
			t.Errorf("page carries %d %q definitions, want exactly 1", n, id)
		}
	}
	for _, gone := range []string{
		"▂▄▆", "◉", "nav-icon-bars", "nav-economy-gradient",
		`<span class="nav-icon" aria-hidden="true">⌂</span>`,
		`<span class="nav-icon" aria-hidden="true">▦</span>`,
		`<span class="nav-icon" aria-hidden="true">★</span>`,
		`<span class="nav-icon" aria-hidden="true">◎</span>`,
		`<span class="nav-icon" aria-hidden="true">↻</span>`,
		`<span class="nav-icon" aria-hidden="true">✦</span>`,
		`<span class="expand-glyph"`,
		"hamburger-lines",
		"nav-hamburger-text",
		`>«</button>`,
		`>×</button>`,
		`>×</label>`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("page still contains retired nav glyph markup %q", gone)
		}
	}
	// Wide top bar, left to right at the right edge: page-sync
	// indicator, global search, then the active character block.
	indicatorAt := strings.Index(body, `id="page-sync-indicator"`)
	searchAt := strings.Index(body, `id="topbar-q"`)
	characterAt := strings.Index(body, `<div class="topbar-character">`)
	if indicatorAt < 0 || searchAt < 0 || characterAt < 0 || !(indicatorAt < searchAt && searchAt < characterAt) {
		t.Errorf("topbar cluster order = indicator %d, search %d, character %d; want indicator < search < character", indicatorAt, searchAt, characterAt)
	}
	// The search glyph parks inside the top search wrap, ahead
	// of the input it decorates; the suggestion dropdown keeps
	// anchoring to the same wrap.
	glyphAt := strings.Index(body, `<svg class="search-glyph"`)
	if glyphAt < 0 || glyphAt > searchAt {
		t.Errorf("search glyph at %d, search input at %d; want the glyph inside the wrap before the input", glyphAt, searchAt)
	}
	// The top bar is a direct child of the body, ahead of the
	// content column, so the fixed strip spans over the
	// sidebar as well.
	headerAt := strings.Index(body, "<header>")
	shellAt := strings.Index(body, `<div class="page-shell">`)
	if headerAt < 0 || shellAt < 0 || headerAt > shellAt {
		t.Errorf("header at %d, page-shell at %d; want the header first, outside the shell", headerAt, shellAt)
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
	if !strings.Contains(aside, `class="navlink sidebar-signout" href="/auth/logout"`) {
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
	if !strings.Contains(body[topbarStart:topbarStart+topbarEnd], `<a class="wordmark topbar-wordmark" href="/" aria-label="EveSynapse home"><svg class="wordmark-glyph"`) {
		t.Error("topbar is missing the wordmark")
	}
	// Branding: the wordmark is the node-and-spoke glyph
	// followed by the full EVESYNAPSE text (Issue 25), painted
	// from its own diagonal gradient def (the nav icons keep
	// the horizontal def).
	topbar := body[topbarStart : topbarStart+topbarEnd]
	wordmarkAt := strings.Index(topbar, `<a class="wordmark topbar-wordmark"`)
	sGlyphAt := strings.Index(topbar, `<svg class="wordmark-glyph"`)
	if wordmarkAt < 0 || sGlyphAt < 0 || sGlyphAt < wordmarkAt {
		t.Errorf("wordmark at %d, glyph at %d in topbar; want the glyph inside the wordmark link", wordmarkAt, sGlyphAt)
	}
	if !strings.Contains(topbar, "</svg><span>EVESYNAPSE</span></a>") {
		t.Error("glyph does not lead the full EVESYNAPSE wordmark")
	}

	code, css := apptest.GetPage(t, rig, cookie, "/static/style.css")
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
		"--success: #ffb84d;",
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
		".nav-icon {\n  flex: 0 0 1.35rem;\n  width: 1.35rem;\n  color: var(--success);\n  line-height: 1;\n  text-align: center;\n}",
		".nav-icon svg {\n  display: block;\n  width: 1.05em;\n  height: 1.05em;\n  margin: 0 auto;\n  fill: #ffb84d;\n  stroke: #ffb84d;\n}",
		".sidebar .branch.active > summary .nav-icon svg,\n.sidenav a.navlink.active .nav-icon svg {\n  filter: brightness(1.15);\n}",
		".nav-hamburger {\n  display: none;\n  align-items: center;\n  justify-content: center;",
		".nav-control-svg {\n  display: block;\n  flex: none;\n  width: 1.05rem;\n  height: 1.05rem;\n  fill: #ffb84d;\n  stroke: #ffb84d;\n}",
		".nav-expand-btn .expand-icon-minus { display: none; }",
		".nav-expand-btn[aria-expanded=\"true\"] .expand-icon-plus { display: none; }",
		".nav-expand-btn[aria-expanded=\"true\"] .expand-icon-minus { display: block; }",
		"html[data-nav=\"rail\"] #nav-collapse .nav-control-svg { transform: scaleX(-1); }",
		".sidebar .nav-drawer-close { display: inline-flex; }",
		"html[data-nav=\"rail\"] .nav-label {\n    display: block;\n    max-width: 12rem;\n    opacity: 1;\n    transform: none;\n  }",
		"html[data-nav=\"rail\"] .sidebar-account-footer { display: flex; }",
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
		// v0.3.07.003 wide chrome: the top bar is a fixed
		// full-width strip over everything, and the sidebar
		// hangs below it (top edge offset by the bar height)
		// in every sidebar state; phones restore the sticky
		// in-flow header and drop the body offset.
		"header {\n  position: fixed;\n  top: 0;\n  right: 0;\n  left: 0;\n  z-index: 60;\n}",
		"header nav { max-width: none; }",
		"background: linear-gradient(to bottom, rgba(24, 24, 24, 0.9), rgba(11, 11, 11, 0.9));",
		"-webkit-backdrop-filter: blur(10px);",
		"backdrop-filter: blur(10px);",
		// v0.3.07.010: the sidebar wears the top bar's glass
		// recipe with its own colors kept — #151515 (21,21,21)
		// into #111111 (17,17,17) at 93% over the same blur.
		// The rail flyout menus stay solid (no pin changes
		// there); only the bar itself goes glass.
		"background: linear-gradient(rgba(21, 21, 21, 0.9), rgba(17, 17, 17, 0.9)) padding-box;\n  -webkit-backdrop-filter: blur(10px);\n  backdrop-filter: blur(10px);",
		// v0.3.07.011: user toggles between wide nav states
		// glide on the grid track that actually drives the
		// layout, labels fade as the rail narrows, and hidden
		// waits for the shrink before it leaves the screen.
		// Transitions arm only once app.js marks the restored
		// initial state ready, so loading a saved rail/hidden
		// layout never sweeps; reduced motion snaps instead.
		"html[data-nav=\"hidden\"] .sidebar {\n  visibility: hidden;\n  pointer-events: none;\n  overflow: hidden;\n  border-right-color: transparent;\n}",
		"html.nav-motion-ready body {\n    transition: grid-template-columns 180ms ease-out;\n  }",
		"html.nav-motion-ready .sidebar {\n    transition: visibility 0s linear 180ms;\n  }",
		"html.nav-motion-ready:not([data-nav=\"hidden\"]) .sidebar {\n    transition-delay: 0s;\n  }",
		"html.nav-motion-ready .nav-label {\n    transition: max-width 180ms ease-out, opacity 140ms ease-out, transform 180ms ease-out;\n  }",
		"html[data-nav=\"rail\"] .nav-label {\n  max-width: 0;\n  opacity: 0;\n  transform: translateX(-0.25rem);\n}",
		"@media (prefers-reduced-motion: reduce) {\n  body,\n  .sidebar,\n  .nav-label,\n  .sidenav a.navlink,\n  .sidebar .branch > summary { transition: none !important; }\n}",
		// v0.3.07.010: shared darker header bands restored —
		// panel titles, card/module headings, foldable section
		// headings, and direct panel section headings all sit
		// on the same solid darker band while the ember ramp
		// still paints the heading text. v0.3.07.012: the band
		// is a solid ::before behind the heading, never a
		// layered background on the heading itself — these
		// pins assert the rendered distinction, not the
		// intention. v0.3.07.014: the module titles themselves
		// paint solid flare gold — the gradient-text clip
		// inside these positioned headings does not paint on
		// every phone browser, and an invisible title is worse
		// than a solid one. Page titles keep the ramp.
		"background: var(--panel-head);\n  margin: -1.25rem -1.25rem 1rem;",
		".panel > h2, .panel > h3,\n.card > h2:first-child, .card > h3:first-child,\n.foldable > h2:first-child, .foldable > h3:first-child {\n  display: block;\n  position: relative;\n  z-index: 0;\n  border-bottom: 1px solid #262626;\n}",
		".panel > h2::before, .panel > h3::before,\n.card > h2:first-child::before, .card > h3:first-child::before,\n.foldable > h2:first-child::before, .foldable > h3:first-child::before {\n  content: \"\";\n  position: absolute;\n  inset: 0;\n  z-index: -1;\n  background: var(--panel-head);\n  border-radius: inherit;\n}",
		"h2, h3 {\n  background: linear-gradient(90deg, #ffd27a 0%, #ff6a1a 50%, #d63c14 100%);\n  -webkit-background-clip: text;\n  background-clip: text;\n  color: transparent;",
		// v0.3.07.014: module/card/foldable titles are solid —
		// no clipped ramp, no transparent glyphs on these
		// headings; the exact-rule pin carries the assertion
		// (solid --success, clip reset to border-box).
		".panel > h2, .panel > h3,\n.card > h2:first-child, .card > h3:first-child,\n.foldable > h2:first-child, .foldable > h3:first-child {\n  background: none;\n  -webkit-background-clip: border-box;\n  background-clip: border-box;\n  color: var(--success);\n  -webkit-text-fill-color: currentColor;\n}",
		// v0.3.07.012: Needs attention rows wear the same
		// zebra as table rows — same --row/--row-alt cycle
		// and the same hover fill.
		".attention li { background: var(--row); padding: 0.35rem 0.5rem; border-bottom: 1px solid #222; }",
		".attention li:nth-child(even) { background: var(--row-alt); }",
		".attention li:hover { background: var(--row-hover); }",
		// v0.3.07.013: nav category labels keep Medium
		// Condensed; the menu links themselves (dropdown
		// entries plus the standalone Home/Sync/Admin rows,
		// sidebar and drawer alike) ride the non-condensed
		// Univers. v0.3.07.015: the links sit at Regular
		// (400), which finally renders true Regular now
		// that the genuine Regular face is embedded —
		// before it landed, a 400 request resolved to the
		// 500 Medium face (only 300/500/700 were declared),
		// which reads bold.
		"@font-face {\n  font-family: 'Univers Next Pro';\n  font-style: normal;\n  font-weight: 400;\n  font-display: swap;\n  src: url('/static/fonts/univers-next-pro-regular.woff2') format('woff2');\n}",
		".branch > summary {\n  font-family: \"Univers Next Pro Condensed\", \"Univers Next Pro\", -apple-system, \"SF Pro Display\", \"Segoe UI\", \"Inter\", sans-serif;\n  font-weight: 500;\n  font-synthesis-weight: none;\n}",
		"header a.navlink, .sidenav a.navlink, .branch .menu a {\n  font-family: \"Univers Next Pro\", -apple-system, \"SF Pro Display\", \"Segoe UI\", \"Inter\", sans-serif;\n  font-weight: 400;\n  font-synthesis-weight: none;\n}",
		".card > h2:first-child, .card > h3:first-child { margin: -0.85rem -1rem 0.75rem; padding: 0.55rem 1rem; border-radius: 4px 4px 0 0; }",
		".foldable > h2:first-child, .foldable > h3:first-child { margin: 0 0 0.75rem; padding: 0.45rem 0.65rem; border-radius: 3px; }",
		// v0.3.07.010 drawer refinement: on small screens the
		// open drawer does NOT blur the page beneath it — the
		// scrim stays a plain dark veil and the drawer keeps
		// the same 93% retained-color paint, crisp over content.
		"z-index: 80;\n    background: linear-gradient(rgba(21, 21, 21, 0.9), rgba(17, 17, 17, 0.9)) padding-box;\n    -webkit-backdrop-filter: none;\n    backdrop-filter: none;\n    transform: translateX(-105%);",
		".nav-backdrop {\n    position: fixed;\n    inset: 0;\n    z-index: 70;\n    background: rgba(0, 0, 0, 0.66);\n    cursor: pointer;\n  }",
		"padding-top: calc(3.75rem + 1px);",
		".topbar { min-height: 3.75rem; }",
		"top: calc(3.75rem + 1px);",
		"height: calc(100dvh - 3.75rem - 1px);",
		"header { position: sticky; z-index: 90; }",
		// v0.3.07.002 expand-all: shown with scripts on, kept in
		// the drawer even when the persisted rail state applies.
		".nav-expand-btn",
		"html.js .nav-expand-btn { display: inline-flex; }",
		"html[data-nav=\"rail\"] .sidebar .nav-expand-btn { display: inline-flex; }",
		// v0.3.07.007: the glyph-only controls float bare —
		// no resting background or visible border; the labeled
		// .nav-reopen keeps its box.
		".nav-state-btn,\n.nav-drawer-close,\n.nav-expand-btn {\n  background: transparent;\n  border-color: transparent;\n}",
		// v0.3.07.005 wide top bar: the wordmark's auto margin
		// pins the sync/search/character cluster to the right
		// edge as one group, and the search field carries its
		// gradient magnifier on extra left padding.
		"@media (min-width: 861px)",
		".topbar-wordmark { margin-right: auto; }",
		".topsearch { flex: 0 1 22rem; }",
		".search-glyph {",
		"pointer-events: none;",
		".topsearch input { padding: 0.3rem 0.55rem 0.3rem 2rem; font-size: 0.85rem; }",
		// v0.3.07.008 branding: the Hub mark rides at the
		// wordmark's left at a chunkier fixed square size,
		// pulled slightly into the topbar flex gap so the
		// gap before the text stays small.
		".wordmark-glyph {\n  display: inline-block;\n  height: 1.9em;\n  width: auto;\n  vertical-align: -0.35em;\n  margin: 0 1.25rem 0 0;\n  transform: rotate(100deg);\n}",
		// v0.3.07.008: the phone-bar hamburger floats bare
		// like the sidebar glyph controls (no resting box on
		// the same footprint), with the same faint hover wash.
		"background: transparent;\n  border: 1px solid transparent;\n  border-radius: 3px;\n  cursor: pointer;\n  font-family: \"Univers Next Pro Condensed\", \"Univers Next Pro\", sans-serif;",
		".nav-hamburger:hover { background: rgba(255, 106, 26, 0.14); }",
		// v0.3.07.006 rail discipline: closed categories never
		// paint a flyout panel, and the active-section pill is
		// the current page's category alone — a merely open
		// flyout wears only a faint ember wash, wide screens
		// only so the drawer keeps its expanded-style look.
		"html[data-nav=\"rail\"] .sidebar .branch:not([open]) > .menu { display: none; }",
		"html[data-nav=\"rail\"] .sidebar .branch[open]:not(.active) > summary {\n    background: rgba(255, 106, 26, 0.14);\n    box-shadow: none;\n  }",
		".sidebar .branch.active > summary {\n  color: var(--text-bright);\n  background: #1b1b1b;\n  box-shadow: inset 3px 0 0 var(--accent);\n}",
		// v0.3.07.006 top-bar divider: a 1px ember rule between
		// the search field and the character switcher, wide
		// layout only (the character block is display:none on
		// phones, and this block never applies there).
		".topbar-character::before {\n    content: \"\";\n    position: absolute;\n    left: calc(-0.5rem - 0.5px);\n    top: 50%;\n    width: 1px;\n    height: 2.25rem;\n    transform: translateY(-50%);\n    background: rgba(255, 106, 26, 0.25);\n  }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"background-attachment: fixed",
		`url("/static/bg.jpg") no-repeat top center fixed`,
		".nav-icon-bars",
		".expand-glyph",
		".hamburger-lines",
		".nav-hamburger-text",
		// v0.3.07.009: the sidebar's old fully-solid paint is
		// gone — the bar is glass now (93% since v0.3.07.010).
		"background: linear-gradient(#151515, #111111) padding-box;",
		// v0.3.07.012: the layered header background that
		// filled module header bars with the ember ramp is
		// gone for good.
		"background-clip: text, border-box",
		"linear-gradient(var(--panel-head), var(--panel-head))",
		// v0.3.07.013: the nav links left the condensed rule —
		// the old combined selector cannot come back.
		"header a.navlink, .sidenav a.navlink, .branch > summary, .branch .menu a",
	} {
		if strings.Contains(css, unwanted) {
			t.Errorf("style.css still contains %q", unwanted)
		}
	}

	code, js := apptest.GetPage(t, rig, cookie, "/static/app.js")
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
		// v0.3.07.011: motion arms after the restored state
		// has landed, never during the load that restored it.
		"nav-motion-ready",
		"requestAnimationFrame",
		// v0.3.07.006: entering the rail folds every category,
		// and in the rail one flyout at a time opens.
		"closeCategoryBranches",
		"closeCategoryBranches(null);",
		"closeCategoryBranches(event.target);",
		"railActive",
		// v0.3.07.006: the live-fill pollers re-check the
		// moment a hidden tab comes back to the front.
		"visibilitychange",
		"document.visibilityState",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if strings.Contains(js, "collapseButton.textContent") {
		t.Error("app.js still rewrites the collapse control as a text glyph")
	}
	// The live-fill pollers never abandon a page that is still
	// waiting: no attempt cap may strand pending content behind
	// a manual refresh (the cold-boot stall of v0.3.07.005).
	for _, gone := range []string{"maxAttempts", "attempts > 40", "attempts > maxAttempts"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js still contains the poller give-up %q", gone)
		}
	}

	// v0.3.07.007 branding: the favicon asset itself. The file
	// server must hand it out as SVG (Go's mime table covers
	// .svg in this environment — this pins that it stays true),
	// carrying The Hub on the page-base backdrop instead of the
	// retired E glyph.
	freq := httptest.NewRequest(http.MethodGet, "/static/favicon.svg", nil)
	frec := httptest.NewRecorder()
	rig.Handler().ServeHTTP(frec, freq)
	if frec.Code != http.StatusOK {
		t.Fatalf("GET /static/favicon.svg: status %d", frec.Code)
	}
	if ct := frec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Errorf("favicon.svg content type %q, want image/svg+xml", ct)
	}
	fav := frec.Body.String()
	for _, want := range []string{
		`viewBox="0 0 48 48"`,
		`<rect width="48" height="48" rx="10" fill="#0d0503"/>`,
		`<circle cx="31.82" cy="18.76" r="2.7"/>`,
		`<circle cx="16.01" cy="29.23" r="2.7"/>`,
	} {
		if !strings.Contains(fav, want) {
			t.Errorf("favicon.svg missing %q", want)
		}
	}
	if strings.Contains(fav, "M20 16h26") {
		t.Error("favicon.svg still carries the retired E glyph")
	}
}
