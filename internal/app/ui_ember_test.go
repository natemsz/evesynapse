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
		"--glass: color-mix(in srgb, var(--panel) 78%, transparent)",
		"--glass-edge: color-mix(in srgb, var(--accent) 24%, transparent)",
		"--glass-blur: 24px",
		"-webkit-backdrop-filter: blur(var(--glass-blur)) saturate(0.9)",
		"backdrop-filter: blur(var(--glass-blur)) saturate(0.9)",
		".brief-card:hover, .brief-card:focus-within",
		"transform: translateY(-2px)",
		"color-mix(in srgb, var(--sev-glow, var(--accent)) 55%, transparent)",
		"animation: sev-pulse 3.2s ease-in-out infinite;",
		"--panel: #1a120b;",
		"--r-lg: 0.5rem;",
		"--r-md: 0.5rem;",
		"--r-sm: 0.5rem;",
		"grid-template-columns: repeat(auto-fill, minmax(min(100%, 22rem), 1fr));",
		"--row: rgba(16, 13, 10, 0.55)",
		"--row-alt: rgba(255, 255, 255, 0.03)",
		".chip:hover",
		"tbody td:first-child, tbody th:first-child {\n  background: transparent;",
		".kpi-spark-line { fill: none; stroke: var(--accent);",
		// Briefing cards fill their row and cap at a third of
		// the widget's content width; the fleet tag filters ride
		// in the widget's header band.
		"max-width: calc((100% - 1.5rem) / 3);",
		".fleet-tags { position: absolute;",
		".brief-tabs { position: absolute;",
		".fleet-tools { border-bottom: 1px solid var(--glass-edge);",
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
	// Below the widest flat strip's wrap point (the market
	// strip, ~1,279px viewport; corporation is grouped and no
	// longer sets it) the folder strip becomes a detached glass
	// rail: one row of pills on the panel's glass, scrolling
	// sideways with the scrollbar hidden and both ends faded by
	// a mask gradient, the panel's top border and rounded top
	// corners restored under it, and the active pill carrying
	// the accent fill and edge instead of the stripe. A grouped
	// section's sub-row sits joined to the panel under the
	// strip at full width and rails the same way below it.
	for _, want := range []string{
		"@media (max-width: 1313px) {",
		"flex-wrap: nowrap;",
		"overflow-x: auto;",
		"scrollbar-width: none;",
		"#main-content > .section-tabs::-webkit-scrollbar { display: none; }",
		"#main-content > .section-subtabs::-webkit-scrollbar { display: none; }",
		"mask-image: linear-gradient(to right,",
		"border-top: 1px solid var(--glass-edge);",
		"border-top-left-radius: var(--r-lg);",
		"border-top: 1px solid color-mix(in srgb, var(--accent) 40%, transparent);",
		"border-bottom: 1px solid color-mix(in srgb, var(--accent) 40%, transparent);",
		"#main-content > .section-subtabs {",
		"#main-content > .section-subtabs a.active {",
		"#main-content:has(> .section-subtabs) > .page-body { border-top: none; }",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the tab rail rule %q", want)
		}
	}
	// The backdrop's two painted states ride the art
	// layer: the muted grade in place, and the glow swap with
	// the horizon gradient over the warm black ground. The
	// default state is the unmodified art, no rule at all.
	for _, want := range []string{
		`html[data-backdrop="muted"] body::before {`,
		"filter: brightness(0.42) saturate(0.30) contrast(1.03);",
		`html[data-backdrop="glow"] body::before {`,
		"radial-gradient(ellipse 95vw 58vh at 100% 0%, rgba(234, 88, 12, 0.32) 0%, rgba(180, 65, 10, 0.13) 52%, rgba(11, 8, 6, 0) 100%)",
		"linear-gradient(180deg, #110c09 0%, #0b0806 100%);",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the backdrop state rule %q", want)
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
	if !strings.Contains(style, "backdrop-filter: blur(var(--glass-blur)) saturate(0.9); }") ||
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
	if !strings.Contains(bg, "background-position: center;") ||
		strings.Contains(bg, "ember-breathe") {
		t.Error("bg.css lost the centered composition or regained the infinite breathe animation")
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
	// deep override (the .suggest recipe: the landing frost over
	// the top bar's panel colour); pinned here for the same
	// reason as the override above.
	if !strings.Contains(bg, ".charselector-dropdown {") ||
		!strings.Contains(bg, "background-color: color-mix(in srgb, var(--panel) 88%, transparent);") {
		t.Error("bg.css lost the charselector dropdown's deep paint; the open list is illegible over content again")
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

// TestWallpaperFirstPaintUnderlay pins the first-paint fix: every
// wallpaper tier declaration in bg.css paints over the thumbnail
// copy of the art, so a full navigation shows the (soft) sky from
// the first frame instead of the flat body background while the
// tier's JXL is still decoding; base.html preloads the two large
// JXL tiers so their fetch starts at parse time. If a tier drops
// its underlay layer, that tier flashes black again on load.
func TestWallpaperFirstPaintUnderlay(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/bg.css")
	if err != nil {
		t.Fatalf("read bg.css: %v", err)
	}
	bg := string(raw)
	// The interior wears the calm sky: six tier declarations
	// (plain + image-set for base/4K/UHD), each with the calm
	// thumb as its last layer.
	if n := strings.Count(bg, `url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668")`); n != 6 {
		t.Errorf("bg.css carries the calm first-paint underlay on %d tier declarations, want 6", n)
	}
	for _, want := range []string{
		`background-image: url("/static/bg-calm.jpg?v=cbb09d21ebe7445e"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
		`background-image: url("/static/bg-calm-4k.jpg?v=0abedfc879b4aacf"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
		`background-image: url("/static/bg-calm-uhd.jpg?v=93ca5b3df15d4dd6"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the calm underlay declaration %q", want)
		}
	}
	// The landing keeps the dramatic art at its base tier only:
	// landing.css (linked only by the landing template, after
	// bg.css) restores the sunrise art with its thumb underlay
	// and never escalates to the wide tiers — the art sits
	// dimmed, blurred, and veiled there (see also
	// TestLandingMockupFidelity).
	landingRaw, err := fs.ReadFile(staticFS, "static/landing.css")
	if err != nil {
		t.Fatalf("read landing.css: %v", err)
	}
	for _, want := range []string{
		`background-image: url("/static/bg.jpg?v=887a4b499319e152"), url("/static/bg-thumb.jpg?v=c4cb5f59ce6c1ad2");`,
	} {
		if !strings.Contains(string(landingRaw), want) {
			t.Errorf("landing.css is missing the dramatic underlay declaration %q", want)
		}
	}
	for _, banned := range []string{"bg-4k", "bg-uhd"} {
		if strings.Contains(string(landingRaw), banned) {
			t.Errorf("landing.css escalates to the %s wallpaper tier; the landing ships the base tier only", banned)
		}
	}
	// The decoded thumbnails must be in the embedded assets (the
	// Makefile decodes them from ci-assets like every other tier).
	for _, name := range []string{"static/bg-thumb.jpg", "static/bg-calm-thumb.jpg", "static/bg-calm-uhd.jxl"} {
		if _, err := fs.Stat(staticFS, name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
	base, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	for _, want := range []string{
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="/static/bg-calm-uhd.jxl?v=afdbe38df4d5f595"`,
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="/static/bg-calm-4k.jxl?v=e6653b67c49cd641"`,
	} {
		if !strings.Contains(string(base), want) {
			t.Errorf("base.html is missing the wallpaper preload %q", want)
		}
	}
}

// TestLandingMockupFidelity pins the landing's fidelity to its
// approved design: the sunrise art band at the top of the page
// fading into page black with the star layer carrying the
// background below it, the floating glass header, the site
// gradient on the hero tail and the playstyle labels (the
// section headlines take it from style.css's heading rule), the
// design's own glass recipe at a 9px backdrop blur instead of
// the site's full-strength one, the landing dim text tone, the
// JetBrains Mono footer, and the preview's flat KPI/brief
// treatment. If one drops out, the landing quietly stops
// looking like the approved design — this says so.
func TestLandingMockupFidelity(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/landing.css")
	if err != nil {
		t.Fatalf("read landing.css: %v", err)
	}
	landing := string(raw)
	for _, want := range []string{
		// The sky stack: the art band seated at the top of the
		// page and masked into it, the fade layer deepening to
		// page black at the band's foot, the star layer above
		// both, each under no pointer.
		"body::before { z-index: -3; position: absolute; top: 0; left: 0; right: 0; bottom: auto; height: 72rem;",
		"body::after { content: \"\"; position: absolute; top: 0; left: 0; right: 0; height: 72rem; z-index: -2; background: linear-gradient(180deg, rgba(4, 2, 2, 0.1), rgba(4, 2, 2, 0) 30%, rgba(4, 2, 2, 0.26) 58%, rgba(4, 2, 2, 0.62) 82%, #040202 100%); pointer-events: none; }",
		".ld-stars { position: absolute; inset: 0; z-index: -1; overflow: hidden; pointer-events: none; background: url(\"/static/stars-tile.webp?v=047d2bb63562f133\") repeat; }",
		// The header is one glass pill pinned flush to the top of the
		// viewport over the art: no gap above it.
		".ld-header { position: fixed; top: 0;",
		// The site gradient on the two landing spots that carry
		// it outside an h2/h3: the hero's tail and the
		// playstyle labels.
		".ld-hero h1 .ld-amp { background: linear-gradient(90deg, #ffd27a 0%, #ff6a1a 50%, #d63c14 100%);",
		".ld-scard .ld-who { background: linear-gradient(90deg, #ffd27a 0%, #ff6a1a 50%, #d63c14 100%);",
		// The design's glass: dark gradient surface, 9px blur,
		// ember-tinted edge, 14px radius.
		".ld-glass { background: linear-gradient(180deg, rgba(12, 8, 6, 0.58), rgba(12, 8, 6, 0) 52%), rgba(19, 14, 10, 0.84); backdrop-filter: blur(9px) saturate(1.15); -webkit-backdrop-filter: blur(9px) saturate(1.15); border: 1px solid rgba(255, 106, 0, 0.16); border-radius: 14px;",
		// The landing dim tone, defined here and used in place
		// of the site's warmer muted.
		":root { --ld-dim: #b9a892; }",
		".ld-scard p { color: var(--ld-dim);",
		".ld-footer a { color: var(--ld-dim); }",
		// The footer is JetBrains Mono, like every other
		// footer on the site.
		".ld-footer { border-top: 1px solid rgba(255, 255, 255, 0.07); padding: 22px 0 30px; color: var(--faint, #8a7a66); font-size: 13.5px; font-family: \"JetBrains Mono\", monospace; }",
		// The preview's borrowed markup carries no glass or
		// backdrop blur of its own inside the landing.
		".ld-preview .kpis { grid-template-columns: repeat(2, 1fr); gap: 14px; padding: 0; background: none; border: none; border-radius: 0; box-shadow: none; -webkit-backdrop-filter: none; backdrop-filter: none; }",
		".ld-preview .brief-card { flex: none; max-width: none; padding: 12px 15px;",
	} {
		if !strings.Contains(landing, want) {
			t.Errorf("landing.css is missing the approved-design declaration %q", want)
		}
	}
	// The site's muted tone stays the site's: no landing rule
	// reads it anymore.
	if strings.Contains(landing, "var(--muted)") {
		t.Error("landing.css still reads the site --muted tone; landing copy uses --ld-dim")
	}
	// The landing's glass never reaches for the site's
	// full-strength blur token.
	if strings.Contains(landing, "var(--glass-blur)") {
		t.Error("landing.css still uses the site --glass-blur token; the landing glass is a fixed 9px")
	}
}

// TestLandingStarTileStaysTiny pins the star tile's size
// contract: it is the landing's background over the whole page
// depth, fetched once and cached, and must stay a sliver next
// to the wallpaper tiers. The bake is lossless WebP, mostly
// transparent; the budget is that encode's class of size with
// headroom: 8 KiB.
func TestLandingStarTileStaysTiny(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/stars-tile.webp")
	if err != nil {
		t.Fatalf("read stars-tile.webp: %v", err)
	}
	if len(raw) > 8192 {
		t.Errorf("stars-tile.webp is %d bytes; the star tile's budget is 8192", len(raw))
	}
	if len(raw) < 12 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		t.Error("stars-tile.webp is not a WebP raster")
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

// TestNoNestedBlurInsidePageGlass pins the blur ruling — blur
// belongs only over unblurred content — for the small glass
// pieces living inside the page panel: the quiet pill flattens
// there and in the notify menu, the alert bars flatten inside
// the panel, and the PI planner's Recalculate pill carries no
// backdrop filter at all. Over the sky (the home bar, home's
// notice under its stripped panel) the base rules keep theirs.
// It also pins the highlighted-row paint: the solid rule that
// actually paints a .yours row, and its pinned-cell variant.
func TestNoNestedBlurInsidePageGlass(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	bg := read("bg.css")
	for _, want := range []string{
		".page-body:not(:has(.home-grid)) .btn.btn-quiet,",
		".notify-menu .btn.btn-quiet {",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the quiet-pill flatten rule %q", want)
		}
	}
	style := read("style.css")
	if !strings.Contains(style, ".page-body:not(:has(.home-grid)) .ok,") ||
		!strings.Contains(style, ".page-body:not(:has(.home-grid)) .notice {") {
		t.Error("style.css lost the alert-bar flatten rule inside the page panel")
	}
	if !strings.Contains(style, "tbody tr.yours td { background-color: #2a2015; }") {
		t.Error("style.css lost the solid rule that paints highlighted rows")
	}
	if !strings.Contains(style, ".tw tbody tr.yours td:first-child { background: #2a2015; }") {
		t.Error("style.css lost the pinned-cell variant of the highlighted row")
	}
	plan := read("plan.css")
	i := strings.Index(plan, "#pi-form button[type=\"submit\"] {")
	if i < 0 {
		t.Fatal("plan.css lost the PI Recalculate pill rule")
	}
	rule := plan[i:]
	if end := strings.Index(rule, "}"); end >= 0 {
		rule = rule[:end]
	}
	if strings.Contains(rule, "backdrop-filter") {
		t.Error("the PI Recalculate pill carries a backdrop filter inside the panel glass")
	}
}

// TestPIPlannerColoniesAndControls pins the PI planner spacing and
// controls pass: suggested colonies carry their own box (the generic
// nested-foldable rule flattens them to padding 0 with no border, so
// they ran together as one text block), the controls card keeps the
// foldable padding rhythm instead of sitting on the page card's
// edge, its fields take the warm glass edge instead of the flat
// greys, and the Recalculate button wears the quiet glass pill, not
// the default orange gradient slab.
func TestPIPlannerColoniesAndControls(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/plan.css")
	if err != nil {
		t.Fatalf("read plan.css: %v", err)
	}
	plan := string(raw)
	for _, want := range []string{
		"#pi-wrap .pi-controls { padding: 0.9rem 1rem 1rem; }",
		"#pi-wrap .pi-controls select {",
		"#pi-wrap .pi-colony {",
		"border: 1px solid var(--glass-edge);",
		"padding: 0.9rem 1rem 1rem;",
		"margin-bottom: 1.25rem;",
		"#pi-form button[type=\"submit\"] {",
		"background: rgba(23, 17, 13, 0.55);",
		".pi-planets { border: 1px solid var(--glass-edge); border-radius: var(--r-sm);",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan.css lost the PI planner rule %q", want)
		}
	}
	tpl, err := fs.ReadFile(templatesFS, "templates/piplanner.html")
	if err != nil {
		t.Fatalf("read piplanner.html: %v", err)
	}
	if !strings.Contains(string(tpl), `<section class="foldable pi-colony" data-fold="never">`) {
		t.Error("piplanner.html lost the pi-colony scope class on the suggested colonies")
	}
}

// TestSubtabBarSharesPanelGlass pins the subtab slab as one piece
// of the panel's own glass: the same 1640px column margins as the
// tab strip and .page-body, the shared --glass-blur (a hardcoded
// blur of its own makes the slab a different material from the
// panel it joins borderlessly), no backdrop filter on the pills
// over the slab's already-blurred glass, and rail-mode end
// padding that keeps the first and last pills clear of the fade.
func TestSubtabBarSharesPanelGlass(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/bg.css")
	if err != nil {
		t.Fatalf("read bg.css: %v", err)
	}
	bg := string(raw)
	block := func(selector string, from int) (string, int) {
		i := strings.Index(bg[from:], selector)
		if i < 0 {
			return "", from
		}
		start := from + i
		end := strings.Index(bg[start:], "}")
		if end < 0 {
			return "", from
		}
		return bg[start : start+end+1], start + end + 1
	}
	slab, next := block("#main-content > .section-subtabs {", 0)
	if slab == "" {
		t.Fatal("bg.css lost the section-subtabs slab rule")
	}
	if !strings.Contains(slab, "margin: 0 max(1.25rem, calc((100% - 1640px) / 2));") {
		t.Error("the subtab slab no longer shares the 1640px column margins with the tab strip and panel")
	}
	if !strings.Contains(slab, "backdrop-filter: blur(var(--glass-blur)) saturate(0.9);") {
		t.Error("the subtab slab no longer blurs with the panel's shared --glass-blur")
	}
	if strings.Contains(slab, "blur(25px)") {
		t.Error("the subtab slab hardcodes its own blur again; the join to the panel will seam")
	}
	// The slab's top edge is the page body's top edge on a grouped
	// page: the slab must keep its top border (suppressing it leaves
	// the whole glass unit open at the top — the panel's own top
	// border is given up at the seam), and the tab strip must paint
	// above the slab (the strip carries z-index 1) so the border
	// line passes behind the tabs and the active tab still merges.
	if strings.Contains(slab, "border-top: none") {
		t.Error("the subtab slab suppresses its top border again; a grouped page's glass unit has no top border line")
	}
	if !strings.Contains(slab, "border: 1px solid var(--glass-edge);") {
		t.Error("the subtab slab lost the shared glass-edge border that closes the unit's outline")
	}
	if strings.Contains(slab, "z-index") {
		t.Error("the subtab slab asserts its own stacking again; it would paint over the tab strip's merge row")
	}
	rail, _ := block("#main-content > .section-subtabs {", next)
	if rail == "" {
		t.Fatal("bg.css lost the railed section-subtabs rule")
	}
	if !strings.Contains(rail, "padding: 0.55rem 1.5rem;") ||
		!strings.Contains(rail, "scroll-padding-inline: 1.5rem;") {
		t.Error("the railed subtab bar's end pills no longer rest clear of the mask fade")
	}
	// Every pill rule (base, light theme, rail) stays free of a
	// backdrop filter: the slab behind them is already blurred.
	for from := 0; ; {
		var pill string
		pill, from = block("#main-content > .section-subtabs a {", from)
		if pill == "" {
			break
		}
		if strings.Contains(pill, "backdrop-filter") {
			t.Errorf("a subtab pill rule carries its own backdrop filter again: %q", pill)
		}
	}
}
