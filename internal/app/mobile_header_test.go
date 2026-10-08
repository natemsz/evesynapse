package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestWordmarkSizeAndPhoneHeader pins the header's proportions: the
// EVESYNAPSE text is big enough next to the logo (Pixelify Sans, drawn on
// an 8-pixel em, at whole-pixel sizes: 1.5rem wide, 1rem on phones), and on
// phones the logo shrinks and the keyboard-shortcut chip goes so the
// search box keeps room.
func TestWordmarkSizeAndPhoneHeader(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	for _, want := range []string{
		// Wide: 1.5rem text (3 screen pixels per design pixel).
		".wordmark {\n  display: inline-block;\n  font-family: 'Pixelify Sans', monospace;\n  font-size: 1.5rem;",
		// Phones and the drawer layout: 1rem text (2 per design pixel), smaller logo, tighter gap.
		".topbar-wordmark {\n    display: inline-flex;\n    align-items: center;\n    font-size: 1rem;",
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
