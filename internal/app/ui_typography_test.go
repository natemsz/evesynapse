package app

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestMenuTextIsAllCapsRajdhani pins the menu face: every menu
// surface (sidenav rows, branch summaries, header links, pop menus)
// reads in Rajdhani full capitals, never the body face and never
// small caps. Dropdown items are the one exception: Rajdhani
// Medium in their labels' own case, under capital group labels.
func TestMenuTextIsAllCapsRajdhani(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// The display-face rows state their capitals in the menu rule.
		".sidenav a.navlink,\n.sidebar-account-footer .navlink,\n.branch > summary {\n  text-transform: uppercase;\n}",
		// Header links keep the SemiBold face.
		"header a.navlink {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  font-synthesis-weight: none;\n  text-transform: uppercase;",
		// Dropdown items ride a step lighter: Medium weight, in
		// their labels' own case, tighter letter spacing.
		".branch .menu a, .switcher .menu a, .switcher-menu a {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 500;\n  font-synthesis-weight: none;\n  text-transform: none;\n  letter-spacing: 0.03em;",
		// Pop-menu items too.
		".menu-pop-body a, .menu-pop-body button {\n  display: block;\n  width: 100%;\n  text-align: left;\n  padding: 0.45rem 0.6rem;\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  text-transform: uppercase;",
		// The sidenav layout rule carries no casing of its own: the
		// menu rule above is the only casing menu rows get.
		".sidenav a.navlink,\n.sidebar .branch > summary {\n  display: flex;\n  align-items: center;\n  gap: 0.6rem;\n  min-height: 2.35rem;\n  padding: 0.42rem 0.55rem;\n  border-radius: var(--r-sm);\n  color: var(--text);\n  font-size: var(--fs-xl);\n  letter-spacing: 0.04em;\n  cursor: pointer;\n}",
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

// TestTabNavTypeSpec pins the tab-navigation face: every tab bar
// on the Rajdhani capitals treatment (section tabs, the section
// sub-tabs, the strategy tabs, the contract spec tabs, the
// fitting view tabs) reads at 12px, weight 500, regular letter
// spacing. Filter chips keep their own heavier control
// treatment.
func TestTabNavTypeSpec(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// Section tabs, sub-tabs and strategy tabs share the tab
		// rule; the chip is not a tab and keeps its own rule.
		".section-tabs a, .section-tabs .tabs-more-btn, .section-subtabs a, .section-subtabs .tabs-more-btn, .ti-tabs button {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 500;\n  text-transform: uppercase;\n  letter-spacing: normal;\n  font-size: var(--fs-sm);\n}",
		".chip {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  text-transform: uppercase;\n  letter-spacing: 0.06em;\n}",
		// The strategy tabs' pill paint block states the same spec.
		".ti-tabs button {\n  font-family: Rajdhani, sans-serif;\n  font-weight: 500;\n  text-transform: uppercase;\n  letter-spacing: normal;\n  font-size: var(--fs-sm);",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing tab rule %q", want)
		}
	}
	contracts := readStatic(t, "contracts.css")
	if want := "font-weight: 600; text-transform: uppercase; letter-spacing: normal; font-size: var(--fs-sm);"; !strings.Contains(contracts, want) {
		t.Errorf("contracts.css .cs-tab missing tab spec %q", want)
	}
	fit := readStatic(t, "fit.css")
	if want := "font-weight: 500; text-transform: uppercase; letter-spacing: normal;\n    font-size: var(--fs-sm);"; !strings.Contains(fit, want) {
		t.Errorf("fit.css .fit-mtabs button missing tab spec %q", want)
	}
}

// TestOpenProseReadsInInter pins the prose role: the shared sheet
// carries the Inter prose rule for the few blocks that still use it.
// The Intel pages left it when they moved onto the layout framework:
// like every framework page, their explanations, labels and forms
// read in the one body face, so none of them marks prose.
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
	for _, name := range []string{"channels.html", "channel.html", "feed.html", "intel_fw.html", "intel_wars.html", "intel_incursions.html", "intel_workbench.html", "killboard.html", "kill.html", "scans.html", "scan.html", "scan_compare.html"} {
		if regexp.MustCompile(`class="(?:[^"]* )?prose(?: [^"]*)?"`).MatchString(readTpl(name)) {
			t.Errorf("%s marks prose; Intel pages read in the framework's one body face", name)
		}
	}
}

// TestPlanNodeSelectionWeight pins the PI tree's selection state:
// a selected node is marked by its border and background alone, at
// the node's normal text weight, like its sibling states.
func TestPlanNodeSelectionWeight(t *testing.T) {
	pi := readStatic(t, "pi.css")
	want := ".pi-node.selected { border-color: var(--gold); background: var(--sel-bg); }"
	if !strings.Contains(pi, want) {
		t.Errorf("pi.css missing selection rule %q", want)
	}
}
