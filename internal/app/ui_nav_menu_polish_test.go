package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNavMenuFaceAndRailTooltips pins two nav behaviours: every row of
// the menu (category headers, Home/Sync/Admin, Theme/Sign out) is set
// in the same condensed face, with only the dropdown entries in the
// regular one; and the icon-only rail names each icon in a themed
// label that appears promptly, on ::before (the category caret owns
// ::after), without the browser's own title tooltip competing.
func TestNavMenuFaceAndRailTooltips(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	for _, want := range []string{
		// One face for every top-level row.
		".branch > summary,\n.sidenav a.navlink,\n.sidebar-account-footer .navlink {\n  font-family: \"Univers Next Pro Condensed\"",
		// Only the dropdown entries (and the legacy header links) stay regular.
		"header a.navlink, .branch .menu a {\n  font-family: \"Univers Next Pro\"",
		// The rail tooltip: ::before, short delay, footer included, guarded for touch.
		"@media (min-width: 861px) and (hover: hover) {",
		`html[data-nav="rail"] .sidenav [data-tip]::before,`,
		`html[data-nav="rail"] .sidebar-account-footer [data-tip]::before {`,
		"transition-delay: 0.2s;",
		`html[data-nav="rail"] .sidenav details[open] > summary[data-tip]::before {`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
	if strings.Contains(css, `.sidenav [data-tip]::after`) {
		t.Error("the rail tooltip is back on ::after, where the category caret rule hides it")
	}
	if strings.Contains(css, "transition-delay: 2s") {
		t.Error("the rail tooltip waits 2s again")
	}
}
