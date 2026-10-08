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

// TestPhoneCardTitlesMatchTheCardPadding: a card's title bleeds to the
// card's edges with negative margins equal to the card's padding. When
// the phone breakpoint shrinks the padding, the margins must shrink
// with it, or the title overshoots the card and paints over its
// border (3.2px each side and 1.6px on top at 375px, measured).
func TestPhoneCardTitlesMatchTheCardPadding(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	for _, want := range []string{
		".card { padding: 0.75rem 0.8rem 0.9rem; }",
		".card > h2:first-child, .card > h3:first-child {\n    margin: -0.75rem -0.8rem 0.75rem;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
}
