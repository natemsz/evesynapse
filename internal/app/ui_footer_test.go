package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestStickyFooterShell pins the app shell's footer contract: the
// page column fills its grid row and main grows into the slack,
// so the footer rests on the viewport's bottom edge on pages
// shorter than the viewport (and simply follows the content on
// long ones). In the block layout under 860px the same floor
// comes from a min-height under the in-flow top bar. The footer's
// background is the star tile: a real compressed raster whose
// ?v= token names its own bytes, like the wallpaper's.
func TestStickyFooterShell(t *testing.T) {
	css := readStatic(t, "style.css")

	shell := cssBlock(t, css, ".page-shell {")
	for _, want := range []string{
		"display: flex;",
		"flex-direction: column;",
		"height: 100%;",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("the .page-shell rule is missing %q; the footer cannot rest on the bottom edge of short pages", want)
		}
	}
	if !strings.Contains(css, ".page-shell > main { flex: 1 0 auto; margin-bottom: 0; }") {
		t.Error("main no longer grows inside the shell (or its bottom margin crept back, doubling the gap above the footer)")
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
// stay a sliver next to the wallpaper tiers.
func TestStarTileStaysTiny(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/stars-tile.webp")
	if err != nil {
		t.Fatalf("read stars-tile.webp: %v", err)
	}
	if len(raw) > 8192 {
		t.Errorf("stars-tile.webp is %d bytes; the footer tile's budget is 8192", len(raw))
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
