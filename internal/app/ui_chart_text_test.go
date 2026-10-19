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
// the rendered labels stay near the UI's small-text floor
// (~12px). The old viewport step-downs (7 units at 1200px+, 6 at
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
		".pchart text {\n  font-family: \"JetBrains Mono\", ui-monospace, monospace;\n  font-size: 12px;\n}",
		// Legend and scrub tooltip sit on the site's small scale.
		"font-weight: 600;\n  font-size: 0.95em;",
		"font-size: 0.95em;\n  line-height: 1.45;",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing chart text rule %q", want)
		}
	}
	for _, want := range []string{
		".pchart { container-type: inline-size; }",
		".pchart text { font-size: 27px; }", // box up to 360px
		".pchart text { font-size: 21px; }", // 361–520px
		".pchart text { font-size: 15px; }", // 521–800px
		".pchart text { font-size: 10px; }", // 801–1150px
		".pchart text { font-size: 7px; }",  // 1151px and up
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
