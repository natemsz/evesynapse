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
		"-webkit-backdrop-filter: blur(20px) saturate(1.25)",
		"backdrop-filter: blur(20px) saturate(1.25)",
		".brief-card:hover, .brief-card:focus-within",
		"transform: translateY(-2px)",
		"color-mix(in srgb, var(--sev-glow, var(--accent)) 55%, transparent)",
		"animation: sev-pulse 3.2s ease-in-out infinite;",
		"--panel: #130e0a;",
		"--r-lg: 16px;",
		"--r-md: 12px;",
		"--r-sm: 8px;",
		"grid-template-columns: repeat(auto-fill, minmax(21rem, 1fr));",
		"--row: rgba(16, 13, 10, 0.55)",
		"--row-alt: rgba(255, 255, 255, 0.03)",
		".chip:hover",
		"background: rgba(18, 14, 11, 0.94)",
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
	if !strings.Contains(style, "backdrop-filter: blur(20px) saturate(1.25); }") ||
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
