package app

import (
	"strings"
	"testing"
)

// TestStickyFooterShell pins the app shell's footer contract: the
// page column fills its grid row and the main row absorbs the
// slack, so the footer rests on the viewport's bottom edge on
// pages shorter than the viewport (and simply follows the content
// on long ones). Main is sized by width, never by its content:
// it fills its column at its content height, so container pages
// span the column and wide content stays inside its own scroll
// regions. In the block layout under 860px the same floor comes
// from a min-height under the in-flow top bar. The footer paints
// black glass: the top bar's gradient inverted and deepened to
// near-black, over the same backdrop blur.
func TestStickyFooterShell(t *testing.T) {
	css := readStatic(t, "style.css")

	shell := cssBlock(t, css, ".page-shell {")
	for _, want := range []string{
		"display: grid;",
		"grid-template-rows: minmax(0, 1fr) auto;",
		"height: 100%;",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("the .page-shell rule is missing %q; the footer cannot rest on the bottom edge of short pages", want)
		}
	}
	main := cssBlock(t, css, ".page-shell > main {")
	for _, want := range []string{
		"align-self: start;",
		"width: 100%;",
		"min-width: 0;",
		"margin-bottom: 0;",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("the .page-shell > main rule is missing %q; main must fill its column at its content height, sized by width and never by its content (and its bottom margin stays out, so the footer's own margin-top is the one gap above it)", want)
		}
	}

	block := mediaBlock(t, css, "@media (max-width: 860px)")
	if !strings.Contains(block, ".page-shell { min-height: calc(100vh - 3.75rem); min-height: calc(100dvh - 3.75rem); }") {
		t.Error("the 860px block lost the page-shell min-height; short pages there end the footer mid-viewport again")
	}

	foot := cssBlock(t, css, "footer {")
	if !strings.Contains(foot, "background: linear-gradient(to bottom, rgba(5, 5, 5, 0.9), rgba(18, 18, 18, 0.9));") {
		t.Error("the footer's black glass paint changed; the footer is the top bar's gradient inverted and deepened to near-black")
	}
	if !strings.Contains(foot, "backdrop-filter: blur(18px);") {
		t.Error("the footer lost the top bar's backdrop blur; the wallpaper must blur through its black glass")
	}
}

// cssBlock returns the declaration block of the first rule whose
// selector text starts at the given marker.
func cssBlock(t *testing.T, css, marker string) string {
	t.Helper()
	at := strings.Index(css, marker)
	if at < 0 {
		t.Fatalf("style.css has no rule at %q", marker)
	}
	rest := css[at:]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("unterminated rule at %q", marker)
	}
	return rest[:end]
}
