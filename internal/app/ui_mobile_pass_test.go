package app

import (
	"io/fs"
	"strings"
	"testing"
)

// mediaBlock returns the text of the first @media block whose
// query starts with the given prefix, up to the next @media.
func mediaBlock(t *testing.T, css, query string) string {
	t.Helper()
	start := strings.Index(css, query)
	if start < 0 {
		t.Fatalf("the %s block is missing", query)
	}
	block := css[start:]
	rest := block[len(query):]
	if next := strings.Index(rest, "@media"); next >= 0 {
		block = block[:len(query)+next]
	}
	return block
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(staticFS, "static/"+name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// TestResponsiveCanon pins the breakpoint canon: the colliding
// one-pixel-apart widths are gone from style.css (719/720 and
// 1099/1100 were the same intent a pixel apart), the canonical
// 45rem/70rem queries carry those blocks now, and the canon
// itself stays written at the top of the sheet.
func TestResponsiveCanon(t *testing.T) {
	css := readStatic(t, "style.css")
	if !strings.Contains(css, "Responsive canon") {
		t.Error("style.css lost the responsive canon comment")
	}
	for _, gone := range []string{
		"@media (max-width: 719px)",
		"@media (max-width: 720px)",
		"@media (max-width: 1099px)",
		"@media (max-width: 1100px)",
	} {
		if strings.Contains(css, gone) {
			t.Errorf("style.css still carries the off-canon query %q", gone)
		}
	}
	for _, want := range []string{
		"@media (max-width: 45rem)",
		"@media (max-width: 70rem)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css is missing the canonical query %q", want)
		}
	}
}

// TestKPIFiguresNeverBreakMidNumber pins the Fold-width defect
// from the owner's screenshots: the home KPI figures wrapped
// inside the number ("176,032,199,750." / "38 ISK"). Figures now
// wrap at their spaces only, and in the intermediate band the
// glance band rewraps as whole cards, two across.
func TestKPIFiguresNeverBreakMidNumber(t *testing.T) {
	css := readStatic(t, "style.css")
	at := strings.Index(css, "\n.kpi-v {")
	if at < 0 {
		t.Fatal("the .kpi-v rule is missing from style.css")
	}
	rule := css[at:]
	if end := strings.Index(rule, "}"); end >= 0 {
		rule = rule[:end]
	}
	if !strings.Contains(rule, "overflow-wrap: normal;") {
		t.Error(".kpi-v no longer wraps at spaces only; a figure can break mid-number again")
	}
	if strings.Contains(rule, "overflow-wrap: anywhere") {
		t.Error(".kpi-v regained overflow-wrap: anywhere; figures break mid-number")
	}
	band := mediaBlock(t, css, "@media (max-width: 70rem) {\n  .home-grid .kpis")
	if !strings.Contains(band, ".home-grid .kpis > * { flex-basis: 40%; }") {
		t.Error("the intermediate band no longer rewraps the home KPI band two across")
	}
}

// TestCoarsePointerTapFloor pins the touch-target floor: the
// shared small controls reach 2.5rem on coarse pointers only, so
// desktop metrics are untouched.
func TestCoarsePointerTapFloor(t *testing.T) {
	css := readStatic(t, "style.css")
	block := mediaBlock(t, css, "@media (hover: none), (pointer: coarse)")
	if !strings.Contains(block, "min-height: 2.5rem;") {
		t.Error("the coarse-pointer block lost the 2.5rem floor")
	}
	for _, sel := range []string{".chip", ".seg > *", ".ap-preset", ".ap-hub", ".ap-samplebtn", ".btn.small"} {
		if !strings.Contains(block, sel) {
			t.Errorf("the coarse-pointer floor no longer covers %s", sel)
		}
	}

	cs := readStatic(t, "contracts.css")
	csBlock := mediaBlock(t, cs, "@media (hover: none), (pointer: coarse)")
	if !strings.Contains(csBlock, "min-height: 2.5rem;") {
		t.Error("contracts.css lost its coarse-pointer 2.5rem floor")
	}
	for _, sel := range []string{".cs-hunt", ".cs-tab", ".cs-fchip", ".cs-deals", ".cs-copy"} {
		if !strings.Contains(csBlock, sel) {
			t.Errorf("the contracts coarse-pointer floor no longer covers %s", sel)
		}
	}
}

// TestFittingPhoneViews pins the fitting tool's phone layout:
// the workbench fragment carries the view tab bar (with the
// module search toggle) ahead of the ship header for the editor
// (never the read-only workbench), the phone rules live in
// fit.css inside the shared 45rem band and the sheet loads only
// on the fitting page, fitted chips carry their cyclable states
// for the editor, fit.js holds the view state with the Radial
// as the landing view, the module search collapses into its
// popup panel from its home slot in the page (the fragment is
// swapped on every simulate, so the search must never live
// inside it), and the fit head's secondary controls carry the
// fold class its phone toggle hides.
func TestFittingPhoneViews(t *testing.T) {
	rawTpl, err := fs.ReadFile(templatesFS, "templates/fit_workbench.html")
	if err != nil {
		t.Fatalf("read fit_workbench.html: %v", err)
	}
	tpl := string(rawTpl)
	for _, want := range []string{
		`{{if and (not .ReadOnly) .HasShip}}`,
		`<div class="fit-mhead">`,
		`data-fit-view="racks" aria-selected="false">Slot Racks</button>`,
		`data-fit-view="stats" aria-selected="false">Resists &amp; Cap</button>`,
		`data-fit-view="radial" aria-selected="true">Radial</button>`,
		`data-fit-search-btn`,
		`{{if and (not $group.ReadOnly) .StatesCSV}} data-fit-states="{{.StatesCSV}}" data-fit-state="{{.State}}"{{end}}`,
	} {
		if !strings.Contains(tpl, want) {
			t.Errorf("fit_workbench.html is missing the phone hook %q", want)
		}
	}
	// The tab bar leads the workbench's phone chrome.
	if ti, hi := strings.Index(tpl, `<div class="fit-mtabs"`), strings.Index(tpl, `<div class="fit-mhead">`); ti < 0 || hi < 0 || ti > hi {
		t.Error("the view tab bar no longer leads the ship header in the workbench fragment")
	}

	rawPage, err := fs.ReadFile(templatesFS, "templates/fittings.html")
	if err != nil {
		t.Fatalf("read fittings.html: %v", err)
	}
	page := string(rawPage)
	for _, want := range []string{
		`data-fit-head-toggle`,
		`data-fit-search-close`,
		`<label class="fit-head-sec fit-shiponly">Pilot`,
		`<label class="fit-clonepick fit-head-sec fit-shiponly">Clone`,
		`<label class="fit-head-sec fit-shiponly fit-stripsec">Fit name`,
		`id="fit-save-eve" data-fit-eve-char`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("fittings.html is missing the phone toolbar hook %q", want)
		}
	}
	if n := strings.Count(page, "fit-head-sec"); n < 4 {
		t.Errorf("fittings.html carries %d fit-head-sec controls, want at least 4 (pilot, clone, name, save-to-EVE)", n)
	}

	css := readStatic(t, "fit.css")
	if !strings.Contains(css, ".fit-mhead, .fit-mtabs { display: none; }") {
		t.Error("fit.css no longer keeps the phone chrome hidden above the band")
	}
	block := mediaBlock(t, css, "@media (max-width: 45rem)")
	for _, want := range []string{
		".fit-mhead {", ".fit-mtabs {", ".fit-wb .fv-off",
		"#fit-modsearch.fit-collapsible { display: none; }",
		"#fit-modsearch.fit-collapsible.open",
		"#fit-editor.fit-armed .fit-head:not(.fit-open) .fit-head-sec { display: none; }",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the fit.css phone block is missing %q", want)
		}
	}

	rawBase, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	if !strings.Contains(string(rawBase),
		`{{if eq .NavPage "fittings.html"}}<link rel="stylesheet" href="/static/fit.css?v={{.AssetVersion}}">{{end}}`) {
		t.Error("base.html no longer loads fit.css on the fitting page")
	}

	js := readStatic(t, "fit.js")
	for _, want := range []string{
		`window.matchMedia("(max-width: 45rem)")`,
		`var fitView = "radial";`,
		`FitPhoneViews.sections(fitView)`,
		`wb.querySelector("[data-fit-search-btn]")`,
		`ms.classList.add("fit-collapsible")`,
		`editor.classList.add("fit-armed")`,
		`headEl.classList.toggle("fit-open")`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("fit.js is missing the phone-view wiring %q", want)
		}
	}
	// The search is never moved into the swapped fragment.
	for _, banned := range []string{`wb.insertBefore(ms,`, `editor.insertBefore(ms, wb)`} {
		if strings.Contains(js, banned) {
			t.Errorf("fit.js still relocates the module search (%q)", banned)
		}
	}
}
