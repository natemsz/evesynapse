package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestKillboardSurfacesCarryTheFullGlass pins the killboard's
// glass: every panel surface on the feed and the kill page
// carries the same recipe the site's cards and panels carry (the
// --glass tint, the glass-edge border, the deepened shadow, and
// the blur with its saturate), the headline figures read in the
// mono KPI face, and a kill that is not on the board lands on a
// glass panel of its own instead of bare text. The not-found
// note is open prose.
func TestKillboardSurfacesCarryTheFullGlass(t *testing.T) {
	kb := readStatic(t, "killboard.css")

	// No killboard backdrop may keep the bare blur: every
	// backdrop-filter the sheet declares pairs the blur with its
	// saturate, the way the shared recipe does.
	if strings.Contains(kb, "blur(var(--glass-blur));") {
		t.Errorf("killboard.css still carries a backdrop blur without its saturate")
	}

	// Each panel surface's own block carries the full recipe,
	// not just the tint and the edge.
	for _, sel := range []string{".kb-stat {", ".kb-list {", ".kb-top {", ".kp-hero {", ".kp-panel {", ".kp-missing {"} {
		i := strings.Index(kb, sel)
		if i < 0 {
			t.Fatalf("killboard.css missing rule %q", sel)
		}
		block := kb[i:]
		block = block[:strings.Index(block, "}")]
		for _, want := range []string{
			"border: 1px solid var(--glass-edge);",
			"background:",
			"var(--glass)",
			"box-shadow: 0 4px 18px rgba(0, 0, 0, 0.6);",
			"backdrop-filter: blur(var(--glass-blur)) saturate(0.9);",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("%s block lacks %q: %s", sel, want, block)
			}
		}
	}

	// The headline figures read in the mono face at the KPI
	// weight: the feed's stat tiles and the kill page's value.
	for _, want := range []string{
		`.kb-stat-v { font-family: "JetBrains Mono", ui-monospace, monospace; font-size: 1.35rem; font-weight: 600;`,
		`.kp-value-v { font-family: "JetBrains Mono", ui-monospace, monospace; font-size: 1.7rem; font-weight: 600;`,
	} {
		if !strings.Contains(kb, want) {
			t.Errorf("killboard.css missing figure rule %q", want)
		}
	}

	raw, err := fs.ReadFile(templatesFS, "templates/kill.html")
	if err != nil {
		t.Fatalf("read kill.html: %v", err)
	}
	if !strings.Contains(string(raw), `class="kp-missing prose"`) {
		t.Errorf("kill.html missing mark %q", `class="kp-missing prose"`)
	}
}
