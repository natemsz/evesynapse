package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBalanceChartTextStaysReadable pins the balance chart's axis
// label sizing. app.js sets --chart-text-scale on each chart SVG to
// its viewBox width over its drawn width so style.css draws labels
// and their stroke outline at one size in every box. Without script,
// bg.css steps the unit size by container width on unscaled SVGs.
func TestBalanceChartTextStaysReadable(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	style, bg, appJS := read("style.css"), read("bg.css"), read("app.js")

	for _, want := range []string{
		// Scaled axis/tick size and stroke outline in style.css.
		".pchart text {\n  font-family: var(--font-mono);\n  font-size: calc(var(--fs-2xs) * var(--chart-text-scale, 1));\n}",
		"stroke-width: calc(6px * var(--chart-text-scale, 1));",
		// Legend and scrub tooltip sit on the site's small scale.
		"font-weight: var(--fw-display);\n  font-size: var(--fs-2xs);",
		"font-size: var(--fs-md);\n  line-height: 1.45;",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing chart text rule %q", want)
		}
	}
	for _, want := range []string{
		`svg.style.setProperty("--chart-text-scale", (vb.width / w).toFixed(4));`,
		`svgs[i].setAttribute("data-text-scaled", "1");`,
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing chart text scaling %q", want)
		}
	}
	for _, want := range []string{
		".pchart { container-type: inline-size; }",
		"@container (max-width: 330px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-5xl); }\n}",
		"@container (min-width: 331px) and (max-width: 400px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-4xl); }\n}",
		"@container (min-width: 401px) and (max-width: 480px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-3xl); }\n}",
		"@container (min-width: 481px) and (max-width: 560px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-xl); }\n}",
		"@container (min-width: 561px) and (max-width: 640px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-md); }\n}",
		"@container (min-width: 641px) and (max-width: 720px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-sm); }\n}",
		"@container (min-width: 721px) and (max-width: 820px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-xs); }\n}",
		"@container (min-width: 821px) {\n  .pchart svg:not([data-text-scaled]) text { font-size: var(--fs-2xs); }\n}",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css missing chart text rule %q", want)
		}
	}
}
