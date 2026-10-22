package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestMenuTextIsAllCapsRajdhani pins the menu face: every menu
// surface (sidenav rows, branch summaries, header links, dropdown
// items, pop menus) reads in Rajdhani full capitals, never the body
// face and never small caps.
func TestMenuTextIsAllCapsRajdhani(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// The display-face rows state their capitals in the menu rule.
		".sidenav a.navlink,\n.sidebar-account-footer .navlink,\n.branch > summary {\n  text-transform: uppercase;\n}",
		// Header links and dropdown items take the same face.
		"header a.navlink, .branch .menu a, .switcher .menu a {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  font-synthesis-weight: none;\n  text-transform: uppercase;",
		// Pop-menu items too.
		".menu-pop-body a, .menu-pop-body button {\n  display: block;\n  width: 100%;\n  text-align: left;\n  padding: 0.45rem 0.6rem;\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  text-transform: uppercase;",
		// The sidenav layout rule carries no casing of its own: the
		// menu rule above is the only casing menu rows get.
		".sidenav a.navlink,\n.sidebar .branch > summary {\n  display: flex;\n  align-items: center;\n  gap: 0.6rem;\n  min-height: 2.35rem;\n  padding: 0.42rem 0.55rem;\n  border-radius: var(--r-sm);\n  color: var(--text);\n  font-size: 1rem;\n  letter-spacing: 0.04em;\n  cursor: pointer;\n}",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing menu rule %q", want)
		}
	}
	// Menu text never renders in small caps.
	if strings.Contains(style, "font-variant-caps: small-caps") {
		t.Errorf("style.css carries a small-caps rule; menu text is full capitals")
	}
}

// TestOpenProseReadsInInter pins the prose role: the shared sheet
// carries the Inter prose rule, and the intel pages mark their
// standalone explanatory blocks with it. Widget, card and table
// text stays on the condensed face.
func TestOpenProseReadsInInter(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		".prose {\n  font-family: \"Inter\", \"Segoe UI\", \"Helvetica Neue\", Helvetica, Arial, sans-serif;\n  line-height: 1.55;\n}",
		// Definition answers inside a prose block stay prose.
		".prose dd { font-family: inherit; }",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing prose rule %q", want)
		}
	}
	readTpl := func(name string) string {
		raw, err := fs.ReadFile(templatesFS, "templates/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	for name, marks := range map[string][]string{
		"channels.html":   {`class="lede prose"`, `class="ch-steps prose"`, `class="ch-faq prose"`},
		"channel.html":    {`class="hint prose"`},
		"intel_fw.html":   {`class="prose"`},
		"intel_wars.html": {`class="prose"`},
	} {
		tpl := readTpl(name)
		for _, mark := range marks {
			if !strings.Contains(tpl, mark) {
				t.Errorf("%s missing prose mark %q", name, mark)
			}
		}
	}
}

// TestPlanNodeSelectionWeight pins the PI tree's selection state:
// a selected node is marked by its border and background alone, at
// the node's normal text weight, like its sibling states.
func TestPlanNodeSelectionWeight(t *testing.T) {
	plan := readStatic(t, "plan.css")
	want := ".pi-node.selected { border-color: #ffd27a; background: var(--row-hover); }"
	if !strings.Contains(plan, want) {
		t.Errorf("plan.css missing selection rule %q", want)
	}
}
