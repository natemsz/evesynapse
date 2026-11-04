package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestScanSurfacesCarryTheFullGlass pins the scan tool's surfaces on
// the layout framework: its panels and stat cards are solid page
// panels (the --surface-inset ground and its hairline) with no glass
// of their own, the stat figures read in the numeral face, and every
// scan page measures its own space inside .l-page. A scan that
// cannot be read lands on a panel of its own instead of bare text.
func TestScanSurfacesCarryTheFullGlass(t *testing.T) {
	scans := readStatic(t, "scans.css")
	if strings.Contains(scans, "var(--glass)") || strings.Contains(scans, "backdrop-filter") {
		t.Errorf("scans.css still paints glass; its panels are the framework's solid panels")
	}
	for _, want := range []string{
		".sc-panel, .sc-paste, .sc-own, .sc-head, .sc-missing {",
		".sc-stat {",
		`.sc-stat-v { font-family: var(--font-num); font-size: var(--fs-3xl); font-weight: 600;`,
	} {
		if !strings.Contains(scans, want) {
			t.Errorf("scans.css missing rule %q", want)
		}
	}
	for _, sel := range []string{".sc-panel, .sc-paste, .sc-own, .sc-head, .sc-missing {", ".sc-stat {"} {
		block := scans[strings.Index(scans, sel):]
		block = block[:strings.Index(block, "}")]
		for _, want := range []string{"border: 1px solid var(--hairline);", "background: var(--surface-inset);"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s block lacks %q: %s", sel, want, block)
			}
		}
	}
	for _, name := range []string{"scans.html", "scan.html", "scan_compare.html"} {
		raw, err := fs.ReadFile(templatesFS, "templates/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(raw), `<div class="l-page sc">`) {
			t.Errorf("%s is not on the layout framework (no .l-page)", name)
		}
	}
}
