package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestKillboardSurfacesCarryTheFullGlass pins the killboard's
// surfaces. The feed sits on the layout framework: its headline
// figures are the framework's stat tiles and its list and
// leaderboards are solid page panels (the --surface-inset ground and
// its hairline), with no glass of their own. The kill page keeps its
// glass recipe on every panel (the --glass tint, the glass-edge
// border, the deepened shadow, and the blur with its saturate), its
// value reads in the numeral face, and a kill that is not on the
// board lands on a panel of its own instead of bare text.
func TestKillboardSurfacesCarryTheFullGlass(t *testing.T) {
	kb := readStatic(t, "killboard.css")
	if strings.Contains(kb, "var(--glass)") || strings.Contains(kb, "backdrop-filter") {
		t.Errorf("killboard.css still paints glass; the feed's panels are the framework's solid panels")
	}
	for _, sel := range []string{".kb-list {", ".kb-top {"} {
		i := strings.Index(kb, sel)
		if i < 0 {
			t.Fatalf("killboard.css missing rule %q", sel)
		}
		block := kb[i:]
		block = block[:strings.Index(block, "}")]
		for _, want := range []string{"border: 1px solid var(--hairline);", "background: var(--surface-inset);"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s block lacks %q: %s", sel, want, block)
			}
		}
	}
	feed, err := fs.ReadFile(templatesFS, "templates/killboard.html")
	if err != nil {
		t.Fatalf("read killboard.html: %v", err)
	}
	for _, mark := range []string{`<div class="l-page kb">`, `class="l-tiles kb-stats"`, `<a class="l-stat kb-big"`} {
		if !strings.Contains(string(feed), mark) {
			t.Errorf("killboard.html missing framework mark %q", mark)
		}
	}

	kp := readStatic(t, "kill.css")
	for _, sel := range []string{".kp-hero {", ".kp-panel {", ".kp-missing {"} {
		i := strings.Index(kp, sel)
		if i < 0 {
			t.Fatalf("kill.css missing rule %q", sel)
		}
		block := kp[i:]
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
	if want := `.kp-value-v { font-family: var(--font-num); font-size: var(--fs-4xl); font-weight: 600;`; !strings.Contains(kp, want) {
		t.Errorf("kill.css missing figure rule %q", want)
	}
	raw, err := fs.ReadFile(templatesFS, "templates/kill.html")
	if err != nil {
		t.Fatalf("read kill.html: %v", err)
	}
	if !strings.Contains(string(raw), `class="kp-missing prose"`) {
		t.Errorf("kill.html missing mark %q", `class="kp-missing prose"`)
	}
}
