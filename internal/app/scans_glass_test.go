package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestScanSurfacesCarryTheFullGlass pins the scan tool's glass:
// its panels and stat cards carry the same recipe the site's
// cards and panels carry (the --glass tint, the glass-edge
// border, the deepened shadow, and the blur with its saturate),
// the stat figures read in the mono KPI face, and a scan that
// cannot be read lands on a glass panel of its own instead of
// bare text. The reading's standing notes are open prose.
func TestScanSurfacesCarryTheFullGlass(t *testing.T) {
	scans := readStatic(t, "scans.css")
	for _, want := range []string{
		// The panel group: paste box, own-scans list, reading
		// head, breakdown panels, and the unreadable-scan state.
		".sc-panel, .sc-paste, .sc-own, .sc-head, .sc-missing {",
		"background: var(--glass);",
		"box-shadow: 0 4px 18px rgba(0, 0, 0, 0.6);",
		"backdrop-filter: blur(var(--glass-blur)) saturate(0.9);",
		// The stat cards carry the glass too, blur included.
		".sc-stat {",
		// Stat figures read in the mono face at the KPI weight.
		`.sc-stat-v { font-family: "JetBrains Mono", ui-monospace, monospace; font-size: 1.35rem; font-weight: 600;`,
	} {
		if !strings.Contains(scans, want) {
			t.Errorf("scans.css missing glass rule %q", want)
		}
	}
	// The stat card's own block carries the backdrop, not just
	// the tint: the flat .sc-stat block is the one a reader's eye
	// lands on first.
	stat := scans[strings.Index(scans, ".sc-stat {"):]
	stat = stat[:strings.Index(stat, "}")]
	if !strings.Contains(stat, "backdrop-filter: blur(var(--glass-blur)) saturate(0.9);") {
		t.Errorf(".sc-stat block lacks the glass backdrop: %s", stat)
	}

	raw, err := fs.ReadFile(templatesFS, "templates/scan.html")
	if err != nil {
		t.Fatalf("read scan.html: %v", err)
	}
	for _, mark := range []string{`class="sc-missing prose"`, `class="sc-note muted prose"`} {
		if !strings.Contains(string(raw), mark) {
			t.Errorf("scan.html missing mark %q", mark)
		}
	}
}
