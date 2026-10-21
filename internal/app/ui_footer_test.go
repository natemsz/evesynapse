package app

import (
	"io/fs"
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
// from a min-height under the in-flow top bar. The footer's
// background is the star tile: a real compressed raster whose
// ?v= token names its own bytes, like the wallpaper's.
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
	if !strings.Contains(foot, "background: rgba(11, 11, 11, 0.85);") {
		t.Error("the footer's flat paint (its no-image fallback) changed")
	}
	idx := loadStaticIndex()
	etag, ok := idx.etags["/stars-tile.webp"]
	if !ok {
		t.Fatal("stars-tile.webp is not in the static index")
	}
	want := "background-image: url(\"/static/stars-tile.webp?v=" + strings.Trim(etag, `"`) + "\");"
	if !strings.Contains(foot, want) {
		t.Errorf("the footer does not paint the star tile under its content-hash token; want %q", want)
	}
}

// TestStarTileStaysTiny pins the tile's size contract: it is a
// footer background, fetched once and cached forever, and must
// stay a sliver next to the wallpaper tiers. The tile is a
// lossless crop of a photographic star field, so its budget is
// the lossless encode's size with headroom: 24 KiB.
func TestStarTileStaysTiny(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/stars-tile.webp")
	if err != nil {
		t.Fatalf("read stars-tile.webp: %v", err)
	}
	if len(raw) > 24576 {
		t.Errorf("stars-tile.webp is %d bytes; the footer tile's budget is 24576", len(raw))
	}
	if len(raw) < 4 || string(raw[:4]) != "RIFF" {
		t.Error("stars-tile.webp is not a WebP raster")
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
