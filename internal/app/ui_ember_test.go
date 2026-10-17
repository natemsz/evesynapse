package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestEmberGlassAndSkyRules pins the Ember Nebula design pass in the
// shipped stylesheets: the glass recipe on the shared surfaces, the
// art-only sky, the briefing glow and pulse, and the motion/blur
// discipline. Atmosphere lives in the shared classes; if a rule
// drops out of the CSS, every page loses it at once — this says so.
func TestEmberGlassAndSkyRules(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	style, bg, plan, link := read("style.css"), read("bg.css"), read("plan.css"), read("link.css")

	for _, want := range []string{
		"--glass: color-mix(in srgb, var(--panel) 83%, transparent)",
		"--glass-edge: color-mix(in srgb, var(--accent) 24%, transparent)",
		"-webkit-backdrop-filter: blur(25px) saturate(0.9)",
		"backdrop-filter: blur(25px) saturate(0.9)",
		".brief-card:hover, .brief-card:focus-within",
		"transform: translateY(-2px)",
		"color-mix(in srgb, var(--sev-glow, var(--accent)) 55%, transparent)",
		"animation: sev-pulse 3.2s ease-in-out infinite;",
		"--panel: #130e0a;",
		"--r-lg: 0.5rem;",
		"--r-md: 0.5rem;",
		"--r-sm: 0.5rem;",
		"grid-template-columns: repeat(auto-fill, minmax(21rem, 1fr));",
		"--row: rgba(16, 13, 10, 0.55)",
		"--row-alt: rgba(255, 255, 255, 0.03)",
		".chip:hover",
		"tbody td:first-child, tbody th:first-child {\n  background: transparent;",
		".kpi-spark-line { fill: none; stroke: var(--accent);",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css is missing the Ember rule %q", want)
		}
	}
	// Light theme keeps the shadow and drops the glow.
	if !strings.Contains(style, `html[data-theme="light"] .brief-card:hover`) {
		t.Error("the light theme's shadow-only briefing hover is missing")
	}
	// The glass recipe holds at every width: no blur-off tier, no
	// alpha compensation. The <720px block keeps only the fleet
	// Location hiding (a layout rule, not a surface rule).
	phoneAt := strings.Index(style, "Motion and blur discipline")
	if phoneAt < 0 {
		t.Fatal("the motion/blur discipline block is missing from style.css")
	}
	phone := style[phoneAt:]
	if strings.Contains(phone, "backdrop-filter: none") &&
		strings.Contains(phone, ".card, .panel") {
		t.Error("a small-screen tier is stripping the backdrop blur again")
	}
	if strings.Contains(phone, "94%, transparent") ||
		!strings.Contains(phone, ".fleet-table .opt2 { display: none; }") {
		t.Error("the <720px block regained the alpha compensation or lost the fleet Location hiding")
	}
	// No enclosing panel: base.html wraps page content in the
	// unpainted .page-body, and the shared blocks carry their own
	// glass (a card inside a card sheds it again).
	baseRaw, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	base := string(baseRaw)
	if !strings.Contains(base, `<div class="page-body">`) || strings.Contains(base, `<div class="panel">`) {
		t.Error("base.html must wrap content in .page-body, not an enclosing .panel")
	}
	// The section tab nav renders outside the glass with the title:
	// base.html invokes each page's pagetabs block between the
	// pagetitle and .page-body, and carries an empty default.
	if !strings.Contains(base, "{{template \"pagetitle\" .}}\n{{template \"pagetabs\" .}}\n  <div class=\"page-body\">") ||
		!strings.Contains(base, "{{define \"pagetabs\"}}{{end}}") {
		t.Error("base.html lost the pagetabs invocation above .page-body or its empty default")
	}
	// The tabs are actual folder tabs on the panel's top edge, and
	// hover is the owner's spec: background removed, saturate(1.5),
	// 150ms. The active tab's only orange is its top stripe.
	for _, want := range []string{
		"#main-content > .section-tabs a.active",
		"border-top: 3px solid var(--accent);",
		"border-bottom: none;",
		"#main-content > .section-tabs a:hover",
		"filter: saturate(1.5);",
		"transition: background-color 150ms ease, border-color 150ms ease, filter 150ms ease;",
		"#main-content:has(> .section-tabs) > .page-body { margin-top: 0; }",
		// A tabbed panel squares its top corners: the rounded
		// corner curves away under the tab row and notches the
		// active tab's merge into the panel.
		"#main-content:has(> .section-tabs) > .page-body { border-top-left-radius: 0; border-top-right-radius: 0; }",
		".item-lead { display: flex;",
		".item-desc {",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the tab/item-lead rule %q", want)
		}
	}
	if !strings.Contains(style, ".page-body { padding: 1.25rem 1.25rem 1.5rem; }") ||
		!strings.Contains(style, ".tw { overflow-x: auto;") ||
		!strings.Contains(style, ".card .tw, .panel .tw, .foldable .tw { background: none;") ||
		!strings.Contains(style, ".card .kpis, .panel .kpis, .foldable .kpis { background: none;") {
		t.Error("the page-body spacing or the shared-block glass surfaces are missing")
	}
	// Section wrappers: the foldable is a glass section (industry,
	// planets, market and character sections ride it), the market
	// browse panel paints its own glass again, and the two pages
	// that floated entirely on the sky (PI planner, public fit)
	// wrap their bodies in a card.
	if !strings.Contains(style, "backdrop-filter: blur(25px) saturate(0.9); }") ||
		!strings.Contains(style, ".foldable .tw { background: none;") {
		t.Error("the foldable section lost its glass surface or its nested-table suppression")
	}
	if strings.Contains(style, ".panel.market-browse { background: none;") {
		t.Error("the market browse panel is unpainted again; its category tiles float on the sky")
	}
	for name, marker := range map[string]string{
		"piplanner.html":    `<section class="card page-card">`,
		"fit_public.html":   `<section class="card page-card">`,
		"corp_members.html": `<section class="card page-card">`,
	} {
		raw, err := fs.ReadFile(templatesFS, "templates/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(raw), marker) {
			t.Errorf("%s lost its glass wrapper section", name)
		}
	}
	// Top search results are glass with a glowing row highlight.
	if !strings.Contains(style, "background: rgba(14, 11, 9, 0.82);") ||
		!strings.Contains(style, ".suggest li:not(.sug-kind):not(.sug-empty):hover") {
		t.Error("the suggestion dropdown lost its glass or its row glow")
	}
	// Reduced motion stops every animation this pass adds.
	if i := strings.Index(style, "@media (prefers-reduced-motion: reduce) {\n  .brief-card"); i < 0 {
		t.Error("the reduced-motion guard for the briefing animation is missing")
	}
	if !strings.Contains(bg, "@media (prefers-reduced-motion: reduce)") ||
		!strings.Contains(bg, "animation: ember-breathe 9s") ||
		!strings.Contains(bg, "background-position: center;") {
		t.Error("bg.css lost the breathe, the centered composition, or the motion guard")
	}
	// The blur lives or dies on this override: a standing
	// view-transition-name makes <main> a backdrop root and every
	// backdrop-filter inside it samples nothing. bg.css must keep
	// forcing the name off (swap.js arms it only during a swap).
	// Lost once to a stale-branch merge; pinned here so a test,
	// not a user, catches the next one.
	if !strings.Contains(bg, "#main-content { view-transition-name: none; }") {
		t.Error("bg.css lost the view-transition-name override; the backdrop blur is dead")
	}
	// The character selector's open list floats over page content;
	// painted at --input-bg's 60% the text underneath bleeds
	// through and the names are unreadable. bg.css carries its
	// near-opaque override (the .suggest recipe); pinned here for
	// the same reason as the override above.
	if !strings.Contains(bg, ".charselector-dropdown {") ||
		!strings.Contains(bg, "background-color: rgba(13, 10, 8, 0.94);") {
		t.Error("bg.css lost the charselector dropdown's near-opaque paint; the open list is illegible over content again")
	}
	// The home KPI tiles carry the mockup's Lucide glyphs in their
	// top right (the .brief-icon treatment at tile scale). The
	// rules live in bg.css — style.css has outgrown a whole-file
	// push — pinned here so a merge cannot silently drop them.
	for _, want := range []string{
		".kpi { position: relative; }",
		".kpi:has(> .kpi-icon) .kpi-k { padding-right: 1.5rem; }",
		".kpi-icon { position: absolute; top: 0.55rem; right: 0.6rem; width: 16px; height: 16px;",
		".kpi-icon svg { display: block; width: 16px; height: 16px; }",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the KPI glyph rule %q", want)
		}
	}
	// The fitting workbench sits inside the glass page wrapper like
	// every other page; only the widget home is exempt. Its drag
	// ghost and tooltips never depended on the wrapper, and the one
	// fixed element it owns (the picker backdrop) is hoisted to
	// body level by swap.js, so the wrapper's backdrop-filter has
	// nothing left to capture.
	if strings.Contains(style, ":has(.fit-wb)") {
		t.Error("style.css re-exempted the fitting workbench from the page wrapper")
	}
	if !strings.Contains(style, ".page-body:has(.home-grid) {") {
		t.Error("style.css lost the widget-home wrapper exemption")
	}
	swapJS, err := staticFS.ReadFile("static/swap.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(swapJS), `querySelectorAll(".modal-backdrop")`) ||
		!strings.Contains(string(swapJS), "data-hoisted") {
		t.Error("swap.js lost the modal-backdrop hoist; fixed modals would be captured by the glass wrapper again")
	}
	// The art is the sky: no synthetic starfield may come back.
	if strings.Contains(bg, "data:image/svg+xml") ||
		strings.Contains(bg, "ember-twinkle") ||
		strings.Contains(bg, "body::after") {
		t.Error("bg.css regained a synthetic starfield layer; the art carries its own stars")
	}
	// The briefing pulse lives on the severity chip, never on the
	// card's box-shadow (a running shadow animation overrides the
	// hover glow and makes every critical card glow in sync).
	if strings.Contains(style, "brief-pulse") {
		t.Error("the card-level brief-pulse is back; the pulse belongs on the .sev chip only")
	}
	// Feature card surfaces aligned to the same translucent recipe.
	for name, css := range map[string]string{"plan.css": plan, "link.css": link} {
		if !strings.Contains(css, "background: color-mix(in srgb, var(--row) 85%, transparent);") {
			t.Errorf("%s has a card surface that did not pick up the glass alpha", name)
		}
	}
	// One corner-radius scale: components take the radius tokens,
	// never a raw 3/4/5px (the squarish look the owner rejected).
	for _, raw := range []string{"border-radius: 3px", "border-radius: 4px", "border-radius: 5px"} {
		if strings.Contains(style, raw) {
			t.Errorf("style.css regained a raw %s; use the --r-* tokens", raw)
		}
	}
	// The sparkline renders only when it has points (never a fake line).
	tpl, err := fs.ReadFile(templatesFS, "templates/home.html")
	if err != nil {
		t.Fatalf("read home.html: %v", err)
	}
	if !strings.Contains(string(tpl), `{{if .NetWorth.SparkPoints}}<svg class="kpi-spark"`) {
		t.Error("the KPI sparkline is not gated on its points being present")
	}
}

// TestCharactersActiveRowTint pins the Characters page's active-row
// treatment: one translucent ember tint over every cell of the
// active character's row, the pinned name cell included. In
// style.css the row's translucent tint is overridden by a later
// solid #2a2015 and the name cell carries that solid as its own
// rule, so the active row read as an opaque muddy band with a
// dark name cell; the fix lives in bg.css (which loads after
// style.css), scoped by the table's own class so no other table's
// .yours rows change. If either rule drops out, the row silently
// goes back to mud — this says so instead.
func TestCharactersActiveRowTint(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/bg.css")
	if err != nil {
		t.Fatalf("read bg.css: %v", err)
	}
	bg := string(raw)
	for _, want := range []string{
		".chars-table tbody tr.yours td { background: rgba(255, 110, 0, 0.16); }",
		".chars-table tbody tr.yours td:first-child { background: rgba(255, 110, 0, 0.16); }",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the characters active-row rule %q", want)
		}
	}
	tpl, err := fs.ReadFile(templatesFS, "templates/characters.html")
	if err != nil {
		t.Fatalf("read characters.html: %v", err)
	}
	if !strings.Contains(string(tpl), `<table class="chars-table">`) {
		t.Error("characters.html lost the chars-table scope class; the bg.css active-row tint has nothing to land on")
	}
}
