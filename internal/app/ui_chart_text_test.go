package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBalanceChartTextStaysReadable pins the balance chart's text
// sizes. The chart SVG is 720 units wide and scales with its box,
// and the box is placement-driven (a full-width page renders it
// near 1500px; the Home widget squeezes it under 500px), so the
// axis type is sized by the chart's own width: bg.css makes
// .pchart a container and steps the unit size per width band so
// the rendered labels stay between about 11 and 15px up to an
// 820px box. The old viewport step-downs (7 units at 1200px+, 6 at
// 1700px+) rendered the Home chart's labels at 3–4px — too small
// to read — while the full-width chart read fine.
func TestBalanceChartTextStaysReadable(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	style, bg := read("style.css"), read("bg.css")

	for _, want := range []string{
		// Base axis/tick size in style.css (the fallback where
		// container queries are unavailable).
		".pchart text {\n  font-family: \"JetBrains Mono\", ui-monospace, monospace;\n  font-size: var(--fs-2xs);\n}",
		// Legend and scrub tooltip sit on the site's small scale.
		"font-weight: 500;\n  font-size: var(--fs-2xs);",
		"font-size: var(--fs-md);\n  line-height: 1.45;",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing chart text rule %q", want)
		}
	}
	for _, want := range []string{
		".pchart { container-type: inline-size; }",
		"@container (max-width: 330px) {\n  .pchart text { font-size: var(--fs-5xl); }\n}",
		"@container (min-width: 331px) and (max-width: 400px) {\n  .pchart text { font-size: var(--fs-4xl); }\n}",
		"@container (min-width: 401px) and (max-width: 480px) {\n  .pchart text { font-size: var(--fs-3xl); }\n}",
		"@container (min-width: 481px) and (max-width: 560px) {\n  .pchart text { font-size: var(--fs-xl); }\n}",
		"@container (min-width: 561px) and (max-width: 640px) {\n  .pchart text { font-size: var(--fs-md); }\n}",
		"@container (min-width: 641px) and (max-width: 720px) {\n  .pchart text { font-size: var(--fs-sm); }\n}",
		"@container (min-width: 721px) and (max-width: 820px) {\n  .pchart text { font-size: var(--fs-xs); }\n}",
		"@container (min-width: 821px) {\n  .pchart text { font-size: var(--fs-2xs); }\n}",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css missing chart text rule %q", want)
		}
	}
	// The old viewport step-downs must not come back: at desktop
	// widths they shrank the Home chart's labels to 3–4px.
	for _, gone := range []string{
		"@media (min-width: 1200px) {\n  .pchart text { font-size: 7px; }",
		"@media (min-width: 1700px) {\n  .pchart text { font-size: 6px; }",
	} {
		if strings.Contains(bg, gone) {
			t.Errorf("bg.css still carries viewport-stepped chart text rule %q", gone)
		}
	}
}
