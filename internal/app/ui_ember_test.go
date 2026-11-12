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
// drops out of the CSS, every page loses it at once, this says so.
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
		"--glass: color-mix(in srgb, var(--panel) 55%, transparent)",
		"--glass-edge: color-mix(in srgb, var(--accent) 20%, transparent)",
		"--glass-blur: 24px",
		"-webkit-backdrop-filter: blur(var(--glass-blur)) saturate(0.9)",
		"backdrop-filter: blur(var(--glass-blur)) saturate(0.9)",
		".brief-card:hover, .brief-card:focus-within",
		"transform: translateY(-2px)",
		"color-mix(in srgb, var(--sev-glow, var(--accent)) 55%, transparent)",
		"animation: sev-pulse 3.2s ease-in-out infinite;",
		"--panel: #151312;",
		"--r-lg: 0.5rem;",
		"--r-md: 0.5rem;",
		"--r-sm: 0.5rem;",
		"grid-template-columns: repeat(auto-fill, minmax(min(100%, 22rem), 1fr));",
		"--row: rgba(16, 13, 10, 0.55)",
		"--row-alt: rgba(255, 255, 255, 0.03)",
		".chip:hover",
		"thead th {\n  background: var(--panel);",
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
	// The section tabs ride the sub-nav bar: base.html invokes each
	// page's pagetabs block inside .subnav, in the page shell above
	// main, the title and .page-body, and carries an empty default.
	if !strings.Contains(base, "<div class=\"subnav\">{{template \"pagetabs\" .}}</div>\n<main id=\"main-content\">\n{{template \"pagetitle\" .}}") ||
		!strings.Contains(base, "{{define \"pagetabs\"}}{{end}}") {
		t.Error("base.html lost the sub-nav's pagetabs invocation above .page-body or its empty default")
	}
	// The backdrop's two painted states ride the art
	// layer: the muted grade baked into the muted twins of
	// the tiers, and the glow swap with the horizon gradient
	// over the warm black ground. The default state is the
	// unmodified art, no rule at all.
	for _, want := range []string{
		`html[data-backdrop="muted"] body::before,`,
		`background-image: url("/static/bg-calm-muted.jpg?v=d056adc1ec88a9fa"), url("/static/bg-calm-muted-thumb.jpg?v=247f99893af7b2ad");`,
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
	// planets, market and character sections ride it), and the
	// market browse panel paints its own glass again.
	if !strings.Contains(style, "backdrop-filter: blur(var(--glass-blur)) saturate(0.9); }") ||
		!strings.Contains(style, ".foldable .tw { background: none;") {
		t.Error("the foldable section lost its glass surface or its nested-table suppression")
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
	// rules live in bg.css, style.css has outgrown a whole-file
	// push, pinned here so a merge cannot silently drop them.
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
	// Feature card surfaces aligned to the same translucent recipe.
	for name, css := range map[string]string{"plan.css": plan, "link.css": link} {
		if !strings.Contains(css, "background: color-mix(in srgb, var(--row) 85%, transparent);") {
			t.Errorf("%s has a card surface that did not pick up the glass alpha", name)
		}
	}
	// One corner-radius scale: components take the radius tokens,
	// never a raw 3/4/5px (the squarish look I rejected).
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
	// and never escalates to the wide tiers, the art sits
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
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="/static/bg-calm-muted-uhd.jxl?v=b7e4118004978f2e"`,
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="/static/bg-calm-muted-4k.jxl?v=73ff76cacb449b10"`,
	} {
		if !strings.Contains(string(base), want) {
			t.Errorf("base.html is missing the wallpaper preload %q", want)
		}
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

// TestNoNestedBlurInsidePageGlass pins the blur ruling, blur
// belongs only over unblurred content, for the small glass
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
}
