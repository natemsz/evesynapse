package app

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// TestTabsMoreWrapperGuard pins the More wrapper's own [hidden]
// guard in bg.css. The wrapper is a flex item carrying an author
// display of its own (its measuring state), and in some engines
// that display wins over the UA's [hidden] rule — a visible More
// button with an empty panel behind it, whose tap does nothing.
// The panel has always carried this guard; the wrapper must too,
// with the measuring state carved out (measuring a hidden
// wrapper is the one moment it must lay out).
func TestTabsMoreWrapperGuard(t *testing.T) {
	css := readStatic(t, "bg.css")
	guard := "#main-content .tabs-more[hidden]:not(.tabs-more-measuring) { display: none; }"
	if !strings.Contains(css, guard) {
		t.Error("bg.css lost the tabs-more wrapper's [hidden] guard; in some engines the More button renders with an empty panel when every tab fits")
	}
}

// TestPhoneShellStatic pins the phone shell's stylesheet and
// template contracts: in the 45rem band the bottom bar renders,
// the hamburger/drawer stand down, the page clears the bar, and
// the appraisal dock floats over it; above the band none of the
// phone chrome shows. The folder strip stands down in the same
// band wherever a sub-row remains under it.
func TestPhoneShellStatic(t *testing.T) {
	css := readStatic(t, "style.css")
	if !strings.Contains(css, ".phone-bar, .phone-sheet, .phone-sheet-backdrop { display: none; }") {
		t.Error("style.css no longer keeps the phone chrome hidden above the band")
	}
	last := strings.LastIndex(css, "@media (max-width: 45rem)")
	if last < 0 {
		t.Fatal("style.css has no 45rem block")
	}
	block := css[last:]
	for _, want := range []string{
		"--phone-bar-h: 3.6rem",
		".nav-hamburger { display: none; }",
		"#nav-reopen { display: none; }",
		"#site-nav { display: none; }",
		".phone-bar {",
		"position: fixed",
		".page-shell {",
		"padding-bottom: calc(var(--phone-bar-h)",
		".phone-sheet:not([hidden]) {",
		".phone-sheet-backdrop:not([hidden]) {",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the style.css phone-shell block is missing %q", want)
		}
	}
	if !strings.Contains(css, "bottom: calc(var(--phone-bar-h, 0px) + env(safe-area-inset-bottom, 0px));") {
		t.Error("the appraisal dock no longer floats over the phone bar")
	}

	bg := readStatic(t, "bg.css")
	bgBlock := mediaBlock(t, bg, "@media (max-width: 45rem)")
	if !strings.Contains(bgBlock, "#main-content:has(> .section-subtabs) > .section-tabs { display: none; }") {
		t.Error("bg.css no longer stands the folder strip down in the phone band where a sub-row remains")
	}

	rawBase, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	base := string(rawBase)
	// The nav glyph gradient is defined once, ahead of the
	// sidebar: engines do not paint references into a
	// display:none subtree, and the sidebar is display:none in
	// the phone band (and whenever the nav is hidden), which
	// would blank every glyph in the bar that uses it.
	defAt := strings.Index(base, `id="nav-glyph-gradient"`)
	asideAt := strings.Index(base, `id="site-nav"`)
	if defAt < 0 || asideAt < 0 || defAt > asideAt {
		t.Error("the nav glyph gradient must be defined once, before the sidebar, so bar glyphs paint while the sidebar is display:none")
	}
	for _, want := range []string{
		`{{range .BottomNav}}`,
		`<nav class="phone-bar" aria-label="Primary">`,
		`id="phone-sections-btn"`,
		`id="phone-sheet"`,
		`id="phone-sheet-backdrop"`,
		`{{range .BottomSections}}`,
		`data-install-app`,
		`data-theme-toggle`,
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.html is missing the phone-shell hook %q", want)
		}
	}

	js := readStatic(t, "app.js")
	for _, want := range []string{
		"initPhoneBar",
		"syncPhoneBar",
		"phoneSectionState",
		`document.querySelectorAll("[data-theme-toggle]")`,
		`document.querySelectorAll("[data-install-app]")`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js is missing the phone-bar wiring %q", want)
		}
	}
}

// phoneBarBlock returns a rendered page's bottom bar markup.
func phoneBarBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="phone-bar"`)
	if start < 0 {
		t.Fatal("rendered page has no phone bar")
	}
	end := strings.Index(body[start:], `</nav>`)
	if end < 0 {
		t.Fatal("rendered page has no phone bar")
	}
	return body[start : start+end]
}

// phoneSheetBlock returns a rendered page's sections sheet markup.
func phoneSheetBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<section class="phone-sheet"`)
	end := strings.Index(body, `</section>`)
	if start < 0 || end < start {
		t.Fatal("rendered page has no sections sheet")
	}
	return body[start:end]
}

// TestPhoneBottomBarRenders: the bar carries Home, the slotted
// sections and the Sections button; the sheet lists every
// primary section with its landing (and a grouped section's
// groups); the current section is marked in the bar when it has
// a slot, on the Sections button when it lives only in the
// sheet, and in the sheet either way.
func TestPhoneBottomBarRenders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	_, body := getPage(t, app, cookie, "/market/leaderboard/")
	bar := phoneBarBlock(t, body)
	for _, want := range []string{
		`<a class="phone-bar-item" href="/" data-section="home">`,
		`<a class="phone-bar-item" href="/character/" data-section="pilot">`,
		`<a class="phone-bar-item" href="/industry/" data-section="industry">`,
		`<a class="phone-bar-item active" href="/market/" data-section="market" aria-current="page">`,
		`<button type="button" class="phone-bar-item" id="phone-sections-btn"`,
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("leaderboard phone bar is missing %q", want)
		}
	}
	sheet := phoneSheetBlock(t, body)
	for _, want := range []string{
		`<a class="phone-sheet-section" href="/character/" data-section="pilot">Pilot</a>`,
		`<a class="phone-sheet-section" href="/fittings/" data-section="fitting">Fitting</a>`,
		`<a class="phone-sheet-section" href="/industry/" data-section="industry">Industry</a>`,
		`<a class="phone-sheet-section active" href="/market/" data-section="market" aria-current="page">Market</a>`,
		`<a class="phone-sheet-section" href="/corporations/" data-section="corporation">Corporation</a>`,
		`<a class="phone-sheet-section" href="/intel/kills/" data-section="intel">Intel</a>`,
		`<a class="phone-sheet-section" href="/items/" data-section="tools">Tools</a>`,
		`<a href="/orders/">My Finances</a>`,
		`<a href="/characters/">Manage Characters</a>`,
		`action="/auth/logout"`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("leaderboard sections sheet is missing %q", want)
		}
	}

	// A section with no bar slot marks the Sections button, and
	// its own sheet row.
	_, body = getPage(t, app, cookie, "/items/")
	bar = phoneBarBlock(t, body)
	if !strings.Contains(bar, `<button type="button" class="phone-bar-item active" id="phone-sections-btn"`) {
		t.Error("on a sheet-only section the Sections button should carry the active mark")
	}
	if strings.Contains(bar, `class="phone-bar-item active" href`) {
		t.Error("on a sheet-only section no bar link should be active")
	}
	sheet = phoneSheetBlock(t, body)
	if !strings.Contains(sheet, `<a class="phone-sheet-section active" href="/items/" data-section="tools" aria-current="page">Tools</a>`) {
		t.Error("the sheet should mark the Tools row on the item browser")
	}

	// Home marks its own slot.
	_, body = getPage(t, app, cookie, "/")
	bar = phoneBarBlock(t, body)
	if !strings.Contains(bar, `<a class="phone-bar-item active" href="/" data-section="home" aria-current="page">`) {
		t.Error("home should mark the Home slot")
	}

	// A grouped section's other groups ride the sheet: from a
	// pilot page, Assets & Money and Comms are one tap away even
	// though the folder strip is stood down on phones.
	_, body = getPage(t, app, cookie, "/character/")
	sheet = phoneSheetBlock(t, body)
	for _, want := range []string{
		`<a class="phone-sheet-section active" href="/character/" data-section="pilot" aria-current="page">Pilot</a>`,
		`<a href="/assets/">Assets &amp; Money</a>`,
		`<a href="/mail/">Comms</a>`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("character sections sheet is missing %q", want)
		}
	}
	if code, _ := getPage(t, app, cookie, "/market/leaderboard/"); code != http.StatusOK {
		t.Errorf("GET /market/leaderboard/: status %d", code)
	}
}

// TestSheetSwitcherCap pins the sheet grid's arithmetic: the
// grid holds six cells, and past that its last cell is the
// overflow tile, so shown plus the tile's count is always the
// account's pilot count.
func TestSheetSwitcherCap(t *testing.T) {
	for _, tc := range []struct{ in, shown, more int }{
		{0, 0, 0}, {3, 3, 0}, {6, 6, 0}, {7, 5, 2}, {8, 5, 3}, {12, 5, 7},
	} {
		entries := make([]switcherEntry, tc.in)
		got, more := sheetSwitcherCap(entries)
		if len(got) != tc.shown || more != tc.more {
			t.Errorf("cap(%d) = %d shown, %d more; want %d shown, %d more",
				tc.in, len(got), more, tc.shown, tc.more)
		}
	}
}

// TestPhoneSheetSwitcherGrid: whatever the account carries,
// the sheet's character grid shows at most five pilots beside
// the "+N more" tile; at or under the grid's six cells every
// pilot shows and there is no tile. The full switcher still
// rides the page for the sidebar (the sheet is its capped
// view, not a replacement model).
func TestPhoneSheetSwitcherGrid(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	sheetFor := func(baseID int64, pilots int) string {
		user, err := q.CreateUser(context.Background())
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		for i := 0; i < pilots; i++ {
			seedCharacter(t, q, user.ID, baseID+int64(i), "Grid Pilot")
		}
		cookie := sessionCookie(t, app, user.ID, baseID, "Grid Pilot")
		_, body := getPage(t, app, cookie, "/market/leaderboard/")
		return phoneSheetBlock(t, body)
	}

	sheet := sheetFor(90000200, 8)
	if n := strings.Count(sheet, `href="/characters/switch?character=`); n != 5 {
		t.Errorf("8-pilot sheet shows %d switch links, want 5 beside the tile", n)
	}
	if !strings.Contains(sheet, `<a class="phone-sheet-more" href="/characters/">+3 more</a>`) {
		t.Error("8-pilot sheet is missing the +3 more tile to Manage Characters")
	}
	if !strings.Contains(sheet, `<a href="/characters/">Manage Characters</a>`) {
		t.Error("8-pilot sheet is missing the Manage Characters utility link")
	}

	sheet = sheetFor(90000300, 6)
	if n := strings.Count(sheet, `href="/characters/switch?character=`); n != 6 {
		t.Errorf("6-pilot sheet shows %d switch links, want all 6", n)
	}
	if strings.Contains(sheet, `phone-sheet-more`) {
		t.Error("6-pilot sheet should have no overflow tile")
	}

	sheet = sheetFor(90000400, 3)
	if n := strings.Count(sheet, `href="/characters/switch?character=`); n != 3 {
		t.Errorf("3-pilot sheet shows %d switch links, want all 3", n)
	}
	if strings.Contains(sheet, `phone-sheet-more`) {
		t.Error("3-pilot sheet should have no overflow tile")
	}
}
