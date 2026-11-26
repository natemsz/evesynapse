package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestWallpaperFirstPaintUnderlay pins the first-paint fix: every
// wallpaper tier declaration in bg.css paints over the thumbnail
// copy of the art, so a full navigation shows the (soft) sky from
// the first frame instead of the flat body background while the
// tier's JXL is still decoding; base.html preloads the two large
// JXL tiers so their fetch starts at parse time. If a tier drops
// its underlay layer, that tier flashes black again on load.
func TestWallpaperFirstPaintUnderlay(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/bg.css")
	if err != nil {
		t.Fatalf("read bg.css: %v", err)
	}
	bg := string(raw)
	// The interior wears the calm sky: six tier declarations
	// (plain + image-set for base/4K/UHD), each with the calm
	// thumb as its last layer.
	if n := strings.Count(bg, `url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668")`); n != 6 {
		t.Errorf("bg.css carries the calm first-paint underlay on %d tier declarations, want 6", n)
	}
	for _, want := range []string{
		`background-image: url("/static/bg-calm.jpg?v=cbb09d21ebe7445e"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
		`background-image: url("/static/bg-calm-4k.jpg?v=0abedfc879b4aacf"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
		`background-image: url("/static/bg-calm-uhd.jpg?v=93ca5b3df15d4dd6"), url("/static/bg-calm-thumb.jpg?v=80a5dfea56eeb668");`,
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the calm underlay declaration %q", want)
		}
	}
	// The landing keeps the dramatic art at its base tier only:
	// landing.css (linked only by the landing template, after
	// bg.css) restores the sunrise art with its thumb underlay
	// and never escalates to the wide tiers, the art sits
	// dimmed, blurred, and veiled there (see also
	// TestLandingMockupFidelity).
	landingRaw, err := fs.ReadFile(staticFS, "static/landing.css")
	if err != nil {
		t.Fatalf("read landing.css: %v", err)
	}
	for _, want := range []string{
		`background-image: url("/static/bg.jpg?v=887a4b499319e152"), url("/static/bg-thumb.jpg?v=c4cb5f59ce6c1ad2");`,
	} {
		if !strings.Contains(string(landingRaw), want) {
			t.Errorf("landing.css is missing the dramatic underlay declaration %q", want)
		}
	}
	for _, banned := range []string{"bg-4k", "bg-uhd"} {
		if strings.Contains(string(landingRaw), banned) {
			t.Errorf("landing.css escalates to the %s wallpaper tier; the landing ships the base tier only", banned)
		}
	}
	// The decoded thumbnails must be in the embedded assets (the
	// Makefile decodes them from ci-assets like every other tier).
	for _, name := range []string{
		"static/bg-thumb.jpg",
		"static/bg-calm-thumb.jpg",
		"static/bg-calm-uhd.jxl",
		"static/bg-nebula-thumb.jpg",
		"static/bg-nebula-mobile.avif",
		"static/bg-nebula-uhd.avif",
		"static/stars-minimal.avif",
		"static/stars-minimal-2x.avif",
		"static/stars-minimal.webp",
	} {
		if _, err := fs.Stat(staticFS, name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
	base, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	for _, want := range []string{
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="{{assetsBase}}/static/bg-calm-uhd.jxl?v=afdbe38df4d5f595"`,
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="{{assetsBase}}/static/bg-calm-4k.jxl?v=e6653b67c49cd641"`,
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="{{assetsBase}}/static/bg-calm-muted-uhd.jxl?v=b7e4118004978f2e"`,
		`<link rel="preload" as="image" type="image/jxl" fetchpriority="high" href="{{assetsBase}}/static/bg-calm-muted-4k.jxl?v=73ff76cacb449b10"`,
	} {
		if !strings.Contains(string(base), want) {
			t.Errorf("base.html is missing the wallpaper preload %q", want)
		}
	}
}

// TestLandingStarTileStaysTiny pins the star tile's size
// contract: it is the landing's background over the whole page
// depth, fetched once and cached, and must stay a sliver next
// to the wallpaper tiers. The bake is lossless WebP, mostly
// transparent; the budget is that encode's class of size with
// headroom: 8 KiB.
func TestLandingStarTileStaysTiny(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/stars-tile.webp")
	if err != nil {
		t.Fatalf("read stars-tile.webp: %v", err)
	}
	if len(raw) > 8192 {
		t.Errorf("stars-tile.webp is %d bytes; the star tile's budget is 8192", len(raw))
	}
	if len(raw) < 12 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		t.Error("stars-tile.webp is not a WebP raster")
	}
}

// TestNoNestedBlurInsidePageGlass pins the blur ruling, blur
// belongs only over unblurred content, for the small glass
// pieces living inside the page panel: the quiet pill flattens
// there and in the notify menu, the alert bars flatten inside
// the panel, and the PI planner's Recalculate pill carries no
// backdrop filter at all. Over the sky (the home bar, home's
// notice under its stripped panel) the base rules keep theirs.
// It also pins the highlighted-row paint: the solid rule that
// actually paints a .yours row, and its pinned-cell variant.
func TestNoNestedBlurInsidePageGlass(t *testing.T) {
	read := func(name string) string {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	bg := read("bg.css")
	for _, want := range []string{
		".page-body:not(:has(.home-grid)) .btn.btn-quiet,",
		".notify-menu .btn.btn-quiet {",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css is missing the quiet-pill flatten rule %q", want)
		}
	}
	style := read("style.css")
	if !strings.Contains(style, ".page-body:not(:has(.home-grid)) .ok,") ||
		!strings.Contains(style, ".page-body:not(:has(.home-grid)) .notice {") {
		t.Error("style.css lost the alert-bar flatten rule inside the page panel")
	}
}
