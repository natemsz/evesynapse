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
	if !strings.Contains(band, ".home-grid .kpis { grid-template-columns: repeat(2, minmax(0, 1fr)); }") {
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

// TestAppraisalBandLayout pins the appraisal half of the
// intermediate-band fix: the summary cards keep a 13rem floor so
// a nowrap figure can never spill into its neighbour, and at
// phone widths the fixed-layout reprocess table scrolls inside
// its wrapper instead of crushing its columns (the itemized
// table's phone treatment is pinned by the pass-2 test below).
func TestAppraisalBandLayout(t *testing.T) {
	css := readStatic(t, "style.css")
	if !strings.Contains(css, ".ap-hud { display: grid; grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));") {
		t.Error(".ap-hud lost its 13rem card floor; summary figures can collide again")
	}
	block := mediaBlock(t, css, "@media (max-width: 45rem) {\n  /* Phone widths: the fixed-layout reprocess table")
	if !strings.Contains(block, ".ap-reprocess table { min-width: 42rem; }") {
		t.Error("the phone block no longer holds the reprocess table at 42rem inside its wrapper")
	}
	band := mediaBlock(t, css, "@media (max-width: 70rem) {\n  .ap-bench")
	if !strings.Contains(band, ".ap-bench { grid-template-columns: minmax(0, 1fr); }") ||
		!strings.Contains(band, ".ap-hud { grid-template-columns: repeat(2, 1fr); }") {
		t.Error("the intermediate band no longer stacks the appraisal bench / pairs the summary cards")
	}
}

// TestAppraisalPhoneTreatment pins mobile pass 2's appraisal
// surface: the segmented Paste Input / Valuation switch (sticky,
// JS-unhidden, one pane at a time under the phone breakpoint),
// the hero card's compact-first figure order, the itemized
// results as stacked cards under their column labels, the
// swipeable samples strip, the paste-from-clipboard hook, and
// the bottom dock re-surfacing existing actions only.
func TestAppraisalPhoneTreatment(t *testing.T) {
	css := readStatic(t, "style.css")
	if !strings.Contains(css, ".ap-switch, .ap-dock { display: none; }") {
		t.Error("the switch and dock are not display:none until app.js unhides them")
	}
	block := mediaBlock(t, css, "@media (max-width: 45rem) {\n  .ap-switch:not([hidden])")
	for _, want := range []string{
		".ap-switch:not([hidden])",
		"position: sticky;",
		`.ap-bench.ap-switched[data-ap-view="input"] > .ap-right { display: none; }`,
		`.ap-bench.ap-switched[data-ap-view="result"] > .ap-form { display: none; }`,
		".ap-samples { flex-wrap: nowrap; overflow-x: auto;",
		".ap-card.hero .ap-cs { display: contents; }",
		".ap-card.hero .ap-cv {",
		"#ap-panel-items thead { display: none; }",
		"#ap-panel-items td[data-th]::before",
		"content: attr(data-th);",
		".ap-dock:not([hidden])",
		".page-body:has(.ap-dock:not([hidden]))",
		".kpi-exact {",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the appraisal phone block is missing %q", want)
		}
	}

	rawTpl, err := fs.ReadFile(templatesFS, "templates/market_appraise.html")
	if err != nil {
		t.Fatalf("read market_appraise.html: %v", err)
	}
	tpl := string(rawTpl)
	for _, want := range []string{
		`data-ap-switch hidden`,
		"1. Paste Input",
		"2. Valuation",
		`data-ap-view="result"`,
		`data-ap-paste hidden`,
		"Paste from clipboard",
		`data-ap-buy-raw="{{$r.BuyRaw}}"`,
		`data-th="Qty"`,
		`data-th="Unit price"`,
		`data-th="Line total"`,
		`data-th="Volume"`,
		`data-ap-dock hidden`,
		"Edit paste",
		`data-ap-dock-edit`,
	} {
		if !strings.Contains(tpl, want) {
			t.Errorf("market_appraise.html is missing the pass-2 hook %q", want)
		}
	}
	// The dock's copy actions are the page's existing ones, on the
	// shared copy hooks — and nothing else: no invented actions.
	dock := tpl[strings.Index(tpl, "ap-dock"):]
	if !strings.Contains(dock, `data-copy-from="ap-multisell"`) || !strings.Contains(dock, `data-copy data-copy-text="{{$r.ShareURL}}"`) {
		t.Error("the dock lost the existing copy actions it re-surfaces")
	}

	js := readStatic(t, "app.js")
	for _, want := range []string{
		"data-ap-view-btn",
		"navigator.clipboard.readText",
		`window.matchMedia("(max-width: 45rem)")`,
		"[data-ap-buy-raw]",
		".home-grid .kpi-v",
		`(v / 1e9).toFixed(2) + "B"`,
		`(v / 1e6).toFixed(2) + "M"`,
		`(v / 1e3).toFixed(1) + "K"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js is missing the pass-2 behavior %q", want)
		}
	}
}

// TestLandingTotalsWrap pins the landing planner card's totals
// row: at small widths the three figures wrap as a left-aligned
// group rather than stretching across the card.
func TestLandingTotalsWrap(t *testing.T) {
	css := readStatic(t, "landing.css")
	block := mediaBlock(t, css, "@media (max-width: 900px)")
	if !strings.Contains(block, ".ld-ptotal { justify-content: flex-start;") {
		t.Error("the landing small-width block no longer groups the planner totals on wrap")
	}
}
