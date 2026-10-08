package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// A small icon of the supported kind: a rounded tile filled left to
// right from red to blue, an orange diamond with a diamond-shaped hole
// (even-odd) and a white dot in the middle.
const testIcon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <defs>
    <linearGradient id="tile" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="#ff0000"/>
      <stop offset="1" stop-color="#0000ff"/>
    </linearGradient>
  </defs>
  <rect width="100" height="100" rx="20" fill="url(#tile)"/>
  <path fill="#F87A2D" fill-rule="evenodd" d="M50 20L80 50L50 80L20 50ZM50 35C55 40 55 40 65 50L50 65L35 50Z"/>
  <circle cx="50" cy="50" r="6" fill="#FFFFFF"/>
</svg>`

// The simplified favicon: the tile in one colour and one triangle.
const testFavicon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <rect width="100" height="100" rx="20" fill="#204060"/>
  <path d="M50 30 L70 70 L30 70 Z" fill="#102030"/>
</svg>`

func TestRunDrawsEveryIcon(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"icon.svg": testIcon, "favicon.svg": testFavicon} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	type px [4]uint32
	open := func(name string, size int) func(fx, fy float64) px {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s is not a PNG: %v", name, err)
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Fatalf("%s is %dx%d, want %d square", name, b.Dx(), b.Dy(), size)
		}
		return func(fx, fy float64) px {
			r, g, b, a := img.At(int(fx*float64(size)), int(fy*float64(size))).RGBA()
			return px{r >> 8, g >> 8, b >> 8, a >> 8}
		}
	}
	near := func(got, want px) bool {
		for i := range got {
			if d := int(got[i]) - int(want[i]); d < -3 || d > 3 {
				return false
			}
		}
		return true
	}

	for name, size := range map[string]int{"icon-192.png": 192, "icon-512.png": 512} {
		at := open(name, size)
		// The dot is white, the band the mark's orange, the hole
		// inside it (even-odd) the tile again; the tile runs from red
		// on the left to blue on the right, and its rounded corner is
		// clear.
		if c := at(0.5, 0.5); c != (px{255, 255, 255, 255}) {
			t.Errorf("%s centre is %v, want white", name, c)
		}
		if c := at(0.5, 0.25); c != (px{0xF8, 0x7A, 0x2D, 255}) {
			t.Errorf("%s band is %v, want the mark's orange", name, c)
		}
		if c := at(0.5, 0.41); !near(c, px{127, 0, 127, 255}) {
			t.Errorf("%s inside the hole is %v, want the tile's middle colour (even-odd fill)", name, c)
		}
		if c := at(0.03, 0.5); !near(c, px{247, 0, 8, 255}) {
			t.Errorf("%s left edge is %v, want the gradient's red end", name, c)
		}
		if c := at(0.97, 0.5); !near(c, px{8, 0, 247, 255}) {
			t.Errorf("%s right edge is %v, want the gradient's blue end", name, c)
		}
		if c := at(0.01, 0.01); c[3] != 0 {
			t.Errorf("%s corner is %v, want it clear outside the rounded tile", name, c)
		}
	}

	// Edge to edge, for a mask or for iOS to round: the corner is tile.
	for name, size := range map[string]int{"icon-maskable-512.png": 512, "apple-touch-icon.png": 180} {
		at := open(name, size)
		if c := at(0.01, 0.01); c[3] != 255 || c[0] < 240 {
			t.Errorf("%s corner is %v, want the tile to the edge", name, c)
		}
		if c := at(0.5, 0.5); c != (px{255, 255, 255, 255}) {
			t.Errorf("%s centre is %v, want white", name, c)
		}
	}
	// The masked icon keeps the mark inside the safe middle: where the
	// plain icon has the band, it has only tile.
	if c := open("icon-maskable-512.png", 512)(0.5, 0.22); !near(c, px{127, 0, 127, 255}) {
		t.Errorf("maskable icon at the plain band is %v, want tile (the mark is drawn smaller)", c)
	}

	// The badge: the favicon's mark in white, and nothing else.
	badge := open("badge-96.png", 96)
	if c := badge(0.5, 0.6); c != (px{255, 255, 255, 255}) {
		t.Errorf("badge mark is %v, want white", c)
	}
	if c := badge(0.05, 0.05); c[3] != 0 {
		t.Errorf("badge corner is %v, want nothing (no tile)", c)
	}

	ico, err := os.ReadFile(filepath.Join(dir, "favicon.ico"))
	if err != nil {
		t.Fatal(err)
	}
	if n := binary.LittleEndian.Uint16(ico[4:6]); n != 3 {
		t.Fatalf("favicon.ico holds %d images, want 3", n)
	}
}

// TestParseDrawingSaysWhatItCannotDraw: a file that uses something
// this tool does not draw is refused with the reason, never drawn
// wrong.
func TestParseDrawingSaysWhatItCannotDraw(t *testing.T) {
	wrap := func(body string) string {
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` + body + `</svg>`
	}
	for name, svg := range map[string]string{
		"no viewBox":           `<svg><path fill="#000000" d="M0 0L1 0L1 1Z"/></svg>`,
		"a viewBox not square": `<svg viewBox="0 0 10 20"><path fill="#000000" d="M0 0L1 0L1 1Z"/></svg>`,
		"a relative command":   wrap(`<path fill="#000000" d="M0 0l1 0l0 1z"/>`),
		"an arc":               wrap(`<path fill="#000000" d="M0 0A1 1 0 0 0 1 1Z"/>`),
		"a missing gradient":   wrap(`<path fill="url(#g)" d="M0 0L1 0L1 1Z"/>`),
		"a radial gradient":    wrap(`<radialGradient id="g"><stop offset="0" stop-color="#ffffff"/></radialGradient><circle cx="5" cy="5" r="2" fill="url(#g)"/>`),
		"a see-through stop":   wrap(`<linearGradient id="g"><stop offset="0" stop-color="#ffffff" stop-opacity=".5"/></linearGradient><path fill="url(#g)" d="M0 0L1 0L1 1Z"/>`),
		"a see-through fill":   wrap(`<path fill="#000000" fill-opacity=".5" d="M0 0L1 0L1 1Z"/>`),
		"a stroke":             wrap(`<path fill="#000000" stroke="#ffffff" d="M0 0L1 0L1 1Z"/>`),
		"a transform":          wrap(`<path fill="#000000" transform="scale(2)" d="M0 0L1 0L1 1Z"/>`),
		"a group":              wrap(`<g><path fill="#000000" d="M0 0L1 0L1 1Z"/></g>`),
		"nothing to draw":      wrap(``),
	} {
		if _, err := parseDrawing(svg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, svg := range map[string]string{"icon": testIcon, "favicon": testFavicon} {
		if _, err := parseDrawing(svg); err != nil {
			t.Errorf("the supported %s was refused: %v", name, err)
		}
	}
}

// TestCommittedIconsMatchTheDrawings: the icons in the repository are
// what this tool draws from the SVG files beside them. A drawing
// replaced without re-running the tool fails here.
func TestCommittedIconsMatchTheDrawings(t *testing.T) {
	static := filepath.Join("..", "..", "internal", "app", "static")
	dir := t.TempDir()
	for _, name := range []string{"icon.svg", "favicon.svg"} {
		raw, err := os.ReadFile(filepath.Join(static, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, name := range []string{"favicon.ico", "icon-192.png", "icon-512.png", "icon-maskable-512.png", "apple-touch-icon.png", "badge-96.png"} {
		drawn, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		committed, err := os.ReadFile(filepath.Join(static, name))
		if err != nil {
			t.Fatalf("%s is missing (run: go run ./cmd/mkicons): %v", name, err)
		}
		if !bytes.Equal(drawn, committed) {
			t.Errorf("%s is not what the drawings give now (run: go run ./cmd/mkicons)", name)
		}
	}
}
