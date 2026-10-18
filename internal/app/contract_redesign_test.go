package app

// The contract search redesign's server-side behavior:
// the buyout cap (not floor), the price window testing the
// figure each row shows, courier-drawer gating, the roll
// quality preset and position floor, the net-spread sort,
// active-filter chips and drawer state, and the inline
// expansion panel fragment.

import (
	"io/fs"
	"strings"
	"testing"
)

func TestContractSearchBuyoutCap(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// The auction's 2,000 ISK buyout breaks a 1,000 cap;
	// contracts with no buyout pass any cap.
	_, body := getAnon(t, app, "/contracts/search/?buyout_max=1000")
	mustContain(t, "buyout cap pass", body, "Cheap Tritanium")
	if strings.Contains(body, "Rare Relic") {
		t.Error("buyout cap 1000 matched a 2000 buyout")
	}
	_, body = getAnon(t, app, "/contracts/search/?buyout_max=2500")
	mustContain(t, "buyout cap 2500", body, "Rare Relic")
}

func TestContractSearchPriceWindowUsesRowFigure(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// The courier's price figure is its 5,000,000 reward:
	// a 1m floor keeps it and drops every cheap exchange.
	_, body := getAnon(t, app, "/contracts/search/?price_min=1000000")
	mustContain(t, "price floor on reward", body, "Move My Stuff")
	if strings.Contains(body, "Cheap Tritanium") {
		t.Error("price floor 1m matched an 850 ISK exchange")
	}
	_, body = getAnon(t, app, "/contracts/search/?price_min=6000000")
	if strings.Contains(body, "Move My Stuff") {
		t.Error("price floor 6m matched a 5m reward")
	}
}

func TestContractSearchCourierFiltersGateToCouriers(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// Any courier-drawer filter narrows to couriers alone.
	_, body := getAnon(t, app, "/contracts/search/?volume_min=500")
	mustContain(t, "courier volume gate", body, "Move My Stuff")
	if strings.Contains(body, "Cheap Tritanium") {
		t.Error("a courier filter matched an exchange contract")
	}
	_, body = getAnon(t, app, "/contracts/search/?volume_min=2000")
	if strings.Contains(body, "Move My Stuff") {
		t.Error("volume floor 2000 matched a 1000 m3 courier")
	}
}

func TestContractSearchMarginSort(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// Net spread: 7001 spreads 150 (1000 value, 850 ask),
	// 7007 spreads 90 (100 value, 10 ask).
	_, body := getAnon(t, app, "/contracts/search/?sort=margin_desc")
	i, j := strings.Index(body, "Cheap Tritanium"), strings.Index(body, "Half Known")
	if i < 0 || j < 0 || i > j {
		t.Error("margin sort should rank the 150 ISK spread above the 90 ISK spread")
	}
}

func TestContractSearchChipsAndDrawerState(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	_, body := getAnon(t, app, "/contracts/search/?me_min=10&spec=blueprints")
	mustContain(t, "chips and drawer", body,
		"Min ME: 10", "Reset Blueprint Filters",
		"Reset Abyssal Filters", "Reset Courier Filters",
		"Abyssal mutaplasmid roll &amp; valuation filters",
		"Courier logistic &amp; hauling filters")
	// The blueprints drawer renders open, the others closed.
	if !strings.Contains(body, `<section class="cs-drawer" data-drawer-panel="blueprints">`) {
		t.Error("blueprints drawer should render open")
	}
	if !strings.Contains(body, `<section class="cs-drawer" data-drawer-panel="abyssal" hidden>`) {
		t.Error("abyssal drawer should render closed")
	}
	// The chip removes just its own filter, keeping the rest.
	if !strings.Contains(body, `href="/contracts/search/?spec=blueprints"`) {
		t.Error("the ME chip should link to the search without me_min")
	}
	// The active drawer is derivable without the spec param.
	_, body = getAnon(t, app, "/contracts/search/?me_min=10")
	if !strings.Contains(body, `<section class="cs-drawer" data-drawer-panel="blueprints">`) {
		t.Error("an active blueprint filter should open the blueprints drawer")
	}
}

func TestContractSearchRollPositionAndQuality(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedAbyssalPage(t, conn)

	// The seed item's primary roll (damage, weight 1.0) sits
	// at the 80th percentile; the CPU penalty roll does not
	// count as a position.
	_, body := getAnon(t, app, "/contracts/search/?pos_min=70")
	mustContain(t, "position floor 70", body, "Bricked But Pretty")
	_, body = getAnon(t, app, "/contracts/search/?pos_min=85")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("position floor 85 matched a primary roll at 80")
	}
	// The quality preset is a score floor: the 45-score item
	// fails both presets, and an unknown preset is ignored.
	_, body = getAnon(t, app, "/contracts/search/?quality=75")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("quality 75 matched a 45 roll score")
	}
	_, body = getAnon(t, app, "/contracts/search/?quality=40")
	mustContain(t, "unknown quality ignored", body, "Bricked But Pretty")
	// The row pill carries the score and position figures.
	_, body = getAnon(t, app, "/contracts/search/")
	mustContain(t, "row abyssal pill", body, "Roll Score: 45/100", "<span>Top 20%</span>")
}

func TestContractSearchStructure(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// The structured zones: the control band's segmented
	// type selector and deals box, the specs band, and the
	// results in their own section.
	_, body := getAnon(t, app, "/contracts/search/?deals=1&spec=couriers")
	mustContain(t, "search structure", body,
		`<div class="seg cs-types"`,
		`<label class="box cs-deals">`,
		`<div class="band cs-specs">`,
		`<section class="box cs-results">`,
		`class="cs-region-search"`)
	// Four equal filter boxes, one per territory.
	if n := strings.Count(body, `<div class="box cs-fbox">`); n != 4 {
		t.Errorf("filter boxes: got %d, want 4", n)
	}
	// Every filter field survives the restructure.
	for _, name := range []string{
		"q_item", "item_dir", "type", "deals", "region", "place",
		"price_min", "price_max", "buyout_max", "q_title", "q_issuer",
		"for_corp", "issued_hrs", "expiring_hrs", "sort", "place_end",
	} {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("field %q missing after restructure", name)
		}
	}
	// The page title is the styled h1 every other page
	// renders, not bare text in the page head.
	mustContain(t, "styled page title", body,
		`<h1><span class="gt">Public contracts</span></h1>`)
	// The quick hunts sit in the intro row, each chip
	// carrying its glyph (the flame shown here).
	mustContain(t, "intro hunts", body,
		`<div class="cs-intro">`,
		`<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 2c1 4-3 5-3 8`)
	// The open spec tab carries its icon, and the result
	// rows render inside the results section.
	if !strings.Contains(body, `data-drawer-tab="couriers" aria-expanded="true"><svg`) {
		t.Error("the open courier tab should carry its icon")
	}
	// The results render as one table inside the results
	// section, its five columns headed per the redesign,
	// and the content lines carry no item icons.
	_, plain := getAnon(t, app, "/contracts/search/")
	mustContain(t, "results table", plain,
		`<table class="cs-table" data-nosort>`,
		`<th>Contract</th>`, `<th>Contents &amp; Specs</th>`,
		`<th>Price / Reward</th>`, `<th>Our Value &amp; Spread</th>`, `<th>Location</th>`)
	if strings.Contains(plain, "images.evetech.net/types/") {
		t.Error("result rows still render item icons")
	}
	i, j := strings.Index(body, `class="box cs-results"`), strings.Index(body, `class="cs-table"`)
	if i < 0 || j < 0 || i > j {
		t.Error("the results table should render inside the results section")
	}
}

// TestContractSearchResultsTable pins the redesigned
// results rows: the count bar, the fixed-width type slot
// with its deal badge, every contents line (no caps, no
// icons), the per-kind price labels and contents pills,
// and the value column's coverage and spread.
func TestContractSearchResultsTable(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	_, body := getAnon(t, app, "/contracts/search/")
	mustContain(t, "count bar", body,
		`<strong>7</strong> contracts match • Page 1 of 1`,
		"Click any row to inspect line-by-line Forge appraisal &amp; mutaplasmid stats")
	// The type label sits in its fixed slot; the deal badge
	// follows it (7001 appraises at exactly 85% of value).
	mustContain(t, "type slot and badge", body,
		`<span class="cs-type t-exchange">Item exchange</span>`,
		"↗ Deal -15%")
	// Every contents line renders, with the blueprint
	// facts in the name and one pill under the items.
	mustContain(t, "blueprint rows", body,
		"Test Blueprint (Original • ME 10 / TE 20)",
		"▦ BPO <span>•</span> <span>ME 10% / TE 20%</span>",
		"Test Blueprint (Copy • 5 Runs • ME 5 / TE 10)",
		"▦ BPC <span>•</span> <span>ME 5% / TE 10%</span>")
	// The auction prices off its standing bid, buyout under.
	mustContain(t, "auction row", body,
		"Current Bid", "700.00 ISK", "Buyout: 2000.00 ISK")
	// The courier: reward and collateral, the sealed wrap
	// line, and the volume/rate pill (no jump count is
	// stored, so none renders).
	mustContain(t, "courier row", body,
		"Sealed Plastic Wrap (1,000 m³)",
		"🚚 1,000 m³ <span>•</span> <span>5000 ISK/m³</span>",
		"Collat: 100.0K ISK")
	// The value column: our figure, the priced coverage,
	// and the spread over the ask.
	mustContain(t, "value column", body,
		"1000.00 ISK", "priced 1 of 1 lines", "&#43;150.00 ISK spread",
		"priced 1 of 2 lines")
	// The expanded row alone carries the accent left bar,
	// and the type slot's width is the badge alignment pin.
	rawCSS, err := fs.ReadFile(staticFS, "static/contracts.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(rawCSS)
	if !strings.Contains(css, ".cs-table tbody tr.cs-row.open {\n  background: rgba(34, 22, 14, 0.55);\n  box-shadow: inset 2px 0 0 var(--accent);\n}") {
		t.Error("contracts.css: the expanded row lost its accent left bar")
	}
	if n := strings.Count(css, "inset 2px 0 0"); n != 1 {
		t.Errorf("contracts.css: the left bar appears %d times, want 1 (expanded rows only)", n)
	}
	if !strings.Contains(css, ".cs-type {\n  display: inline-block; width: 6.6rem;") {
		t.Error("contracts.css: the fixed-width type slot is gone")
	}
}

// TestContractSearchSwapRewiring pins the contracts.js
// swap contract: an in-page swap replaces the results DOM
// without re-running the script, so the page wiring must
// re-run on the shell's content-ready event (guarded by the
// form's identity, like pi.js) — without it, row expansion
// and every other wired behavior are dead after any
// same-page navigation such as a quick-hunt click.
func TestContractSearchSwapRewiring(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/contracts.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, want := range []string{
		`document.addEventListener("evesynapse:content-ready", init)`,
		"form === wiredForm",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("contracts.js missing swap re-wiring pin %q", want)
		}
	}
}

// TestContractSearchDrawerGrid pins the spec drawers'
// deterministic grid in contracts.css: fixed tracks and
// per-field spans, never the auto-fit rule whose column
// widths depended on field count — and the deals box's
// resting green identity.
func TestContractSearchDrawerGrid(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/contracts.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	for _, want := range []string{
		"grid-template-columns: repeat(6, minmax(0, 1fr))",
		".cs-drawer-grid > .cs-field { grid-column: span 2; }",
		"grid-column: span 4; }",
		"border-color: color-mix(in srgb, var(--deal) 55%, transparent);",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("contracts.css: missing %q", want)
		}
	}
	if strings.Contains(css, "repeat(auto-fit, minmax(14.5rem, 1fr))") {
		t.Error("contracts.css: the drawer grid is back on auto-fit tracks")
	}
}

func TestContractPanelFragment(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	code, body := getAnon(t, app, "/contracts/search/7001/panel")
	if code != 200 {
		t.Fatalf("panel: status %d, want 200", code)
	}
	mustContain(t, "panel 7001", body,
		"Contract Line Appraisal (Forge Sweep)", "Tritanium",
		"Swept Forge Typical: 1000.00 ISK",
		"ID: 7001", "Issued by Issuer One (Pilot)",
		"Deal Execution Summary", "Asking Price:", "850.00 ISK",
		"EveSynapse Value:", "Estimated Spread:", "150.00 ISK (15.0%)",
		"In-game link ready for EVE Notepad",
		"Copy Contract Link", "contract:10000002//7001")
	if strings.Contains(body, "<html") || strings.Contains(body, "pagetitle") {
		t.Error("the panel must be a bare fragment, not a full page")
	}
	if strings.Contains(body, "images.evetech.net") {
		t.Error("the drawer still renders item icons")
	}

	// The courier drawer states the private cargo plainly
	// and sets the reward against the collateral; collateral
	// minus reward is not a spread, so none renders.
	_, body = getAnon(t, app, "/contracts/search/7003/panel")
	mustContain(t, "panel 7003", body,
		"private", "Contract Line Appraisal (Route Hauling Rate)",
		"Asking Price:", "5.0M ISK", "EveSynapse Value:", "100.0K ISK")
	if strings.Contains(body, "Estimated Spread:") {
		t.Error("the courier drawer renders a spread figure")
	}

	if code, _ := getAnon(t, app, "/contracts/search/999999/panel"); code != 404 {
		t.Errorf("unknown panel: status %d, want 404", code)
	}
}

func TestContractPanelAbyssal(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedAbyssalPage(t, conn)

	code, body := getAnon(t, app, "/contracts/search/8501/panel")
	if code != 200 {
		t.Fatalf("abyssal panel: status %d, want 200", code)
	}
	// The drawer: the appraisal card credits the abyssal
	// model and prices the line from the stored estimate;
	// the telemetry card carries the stored range and every
	// roll with its percentile bar, inline.
	mustContain(t, "panel 8501", body,
		"Contract Line Appraisal (Abyssal Neural Model)",
		"Abyssal Test Damage", "[Roll 45/100]",
		"Swept Forge Typical: 205.0M ISK",
		"Mutaplasmid Telemetry", "Est Range: 180.0M – 240.0M ISK",
		"Damage Multiplier", "&#43;18.0% (80%)",
		"Capacitor Need", "CPU", `data-w="80"`, `data-w="2"`)
}

// TestContractSearchAbyssalInline pins the mutaplasmid
// stats presentation after the redesign: the row carries
// the score pill alone and the rolls render inline in the
// expansion drawer's telemetry card — the popup the old
// design opened over the results is retired from the
// search page entirely, markup, wiring and paint alike.
// (The contract detail page keeps its own full inline
// roll readout, as before.)
func TestContractSearchAbyssalInline(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedAbyssalPage(t, conn)

	// The row: the pill is a plain span, not a trigger,
	// and no teaser facts ride in the row's flow.
	_, body := getAnon(t, app, "/contracts/search/")
	mustContain(t, "row abyssal pill", body,
		`<span class="cs-pill ab">▦ Roll Score: 45/100`)
	for _, gone := range []string{"data-abpop", "cs-abpop", "Mutated from", "Held back by"} {
		if strings.Contains(body, gone) {
			t.Errorf("the search page still carries %q", gone)
		}
	}

	// The panel fragment: telemetry inline, no popup.
	_, body = getAnon(t, app, "/contracts/search/8501/panel")
	mustContain(t, "panel telemetry", body, "Mutaplasmid Telemetry")
	if strings.Contains(body, "data-abpop") || strings.Contains(body, "cs-abpop") {
		t.Error("the panel fragment still wires the abyssal popup")
	}

	// The wiring and the paint hold no popup machinery.
	rawJS, err := fs.ReadFile(staticFS, "static/contracts.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawJS), "abpop") || strings.Contains(string(rawJS), "AbPop") {
		t.Error("contracts.js still carries the abyssal popup wiring")
	}
	rawCSS, err := fs.ReadFile(staticFS, "static/contracts.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawCSS), "cs-abpop") {
		t.Error("contracts.css still carries the abyssal popup paint")
	}
}
