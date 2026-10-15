package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestPhoneBarRules pins the measured phone fix in the stylesheet:
// a 15px root at <=480px (font-size, never the zoom property, whose
// media queries can oscillate at the boundary), and the Overview bar
// compressed onto one row — dot-only ESI pill, gear-only Overview
// summary, full "Link another character" text. The widths themselves
// were verified in a browser rig; this keeps the rules from quietly
// dropping out of the CSS.
func TestPhoneBarRules(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	if strings.Contains(css, "zoom:") {
		t.Error("style.css uses the zoom property; the phone scale must be root font-size")
	}
	start := strings.Index(css, "@media (max-width: 480px)")
	if start < 0 {
		t.Fatal("the <=480px phone block is missing from style.css")
	}
	// The block is the rest of its media group; the next @media ends it.
	block := css[start:]
	if next := strings.Index(block[len("@media (max-width: 480px)"):], "@media"); next >= 0 {
		block = block[:len("@media (max-width: 480px)")+next]
	}
	for _, want := range []string{
		"html { font-size: 15px; }",
		".home-bar { flex-wrap: nowrap;",
		".home-bar .esi-pill .esi-label { display: none; }",
		".home-bar .home-settings > summary > span { display: none; }",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the phone block is missing %q", want)
		}
	}
	// The pill's words survive for speech and hover: the template
	// keeps the label element and the pill's title.
	rawTpl, err := fs.ReadFile(templatesFS, "templates/home.html")
	if err != nil {
		t.Fatalf("read home.html: %v", err)
	}
	if !strings.Contains(string(rawTpl), `class="esi-pill esi-{{.State}}" title="{{.Title}}"`) {
		t.Error("the ESI pill lost the title that carries its words when the label is hidden")
	}
	if !strings.Contains(string(rawTpl), `aria-label="Overview settings"`) {
		t.Error("the Overview summary lost the aria-label that names it when its word is hidden")
	}
}
