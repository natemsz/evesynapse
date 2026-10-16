package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestEmberGlassAndSkyRules pins the Ember Nebula design pass in the
// shipped stylesheets: the glass recipe on the shared surfaces, the
// starfield sky, the briefing glow and pulse, and the motion/blur
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
		"--glass: color-mix(in srgb, var(--panel) 85%, transparent)",
		"--glass-edge: color-mix(in srgb, var(--accent) 24%, transparent)",
		"-webkit-backdrop-filter: blur(9px) saturate(1.15)",
		"backdrop-filter: blur(9px) saturate(1.15)",
		".brief-card:hover, .brief-card:focus-within",
		"transform: translateY(-2px)",
		"color-mix(in srgb, var(--sev-glow, var(--accent)) 45%, transparent)",
		"animation: brief-pulse 3.2s ease-in-out infinite;",
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
	// Blur cap under 720px, alpha compensating.
	phoneAt := strings.Index(style, "Motion and blur discipline")
	if phoneAt < 0 {
		t.Fatal("the motion/blur discipline block is missing from style.css")
	}
	phone := style[phoneAt:]
	if !strings.Contains(phone, "backdrop-filter: none") ||
		!strings.Contains(phone, "--glass: color-mix(in srgb, var(--panel) 94%, transparent)") ||
		!strings.Contains(phone, ".fleet-table .opt2 { display: none; }") {
		t.Error("the <720px block lost the blur cap, the alpha compensation, or the fleet Location hiding")
	}
	// Reduced motion stops every animation this pass adds.
	if i := strings.Index(style, "@media (prefers-reduced-motion: reduce) {\n  .brief-card"); i < 0 {
		t.Error("the reduced-motion guard for the briefing animation is missing")
	}
	if !strings.Contains(bg, "@media (prefers-reduced-motion: reduce)") ||
		!strings.Contains(bg, "animation: ember-twinkle 6s") ||
		!strings.Contains(bg, "animation: ember-breathe 9s") ||
		strings.Count(bg, "data:image/svg+xml") < 3 ||
		!strings.Contains(bg, `html[data-theme="light"] body::after { display: none; }`) {
		t.Error("bg.css lost the starfield layers, the twinkle/breathe, or a motion/light guard")
	}
	// Feature card surfaces aligned to the same translucent recipe.
	for name, css := range map[string]string{"plan.css": plan, "link.css": link} {
		if !strings.Contains(css, "background: color-mix(in srgb, var(--row) 85%, transparent);") {
			t.Errorf("%s has a card surface that did not pick up the glass alpha", name)
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
