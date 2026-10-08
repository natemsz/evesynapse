package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestWordmarkSizeAndPhoneHeader pins the header's proportions: the
// EVESYNAPSE text is big enough next to the logo (VT323 is a pixel
// font with a small cap height, so the sizes are generous), and on
// phones the logo shrinks and the keyboard-shortcut chip goes so the
// search box keeps room.
func TestWordmarkSizeAndPhoneHeader(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	for _, want := range []string{
		// Wide: 1.5rem text (it was 1.2rem, 0.78rem on phones).
		".wordmark {\n  display: inline-block;\n  font-family: 'VT323', monospace;\n  font-size: 1.5rem;",
		// Phones and the drawer layout: readable text, smaller logo, tighter gap.
		".topbar-wordmark {\n    display: inline-flex;\n    align-items: center;\n    font-size: 1.2rem;",
		".wordmark-glyph { height: 1.2rem; margin-right: 0.5rem; }",
		".topbar-wordmark > span { padding-left: 0.5rem; }",
		// The shortcut chip does nothing on a touch screen.
		".quickjump-btn { display: none; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
}
