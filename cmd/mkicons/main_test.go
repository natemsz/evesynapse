package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A small logo of the supported kind: an orange diamond with a
// diamond-shaped hole (even-odd) and a white dot in the middle.
const testLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <path fill="#F87A2D" fill-rule="evenodd" d="M50 5L95 50L50 95L5 50ZM50 30C60 40 60 40 70 50L50 70L30 50Z"/>
  <circle cx="50" cy="50" r="8" fill="#FFFFFF"/>
</svg>`

func TestRunDrawsEveryIconFromTheLogo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte(testLogo), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	for name, size := range map[string]int{
		"icon-192.png": 192, "icon-512.png": 512, "icon-maskable-512.png": 512, "apple-touch-icon.png": 180,
	} {
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
		at := func(fx, fy float64) [3]uint32 {
			r, g, b, _ := img.At(int(fx*float64(size)), int(fy*float64(size))).RGBA()
			return [3]uint32{r >> 8, g >> 8, b >> 8}
		}
		// The dot in the middle is white, the corner is the dark tile,
		// the diamond's band is the logo's orange, and the hole inside
		// it (even-odd) is the tile again.
		if c := at(0.5, 0.5); c != [3]uint32{255, 255, 255} {
			t.Errorf("%s centre is %v, want white", name, c)
		}
		if c := at(0.02, 0.02); c != [3]uint32{0x0d, 0x05, 0x03} {
			t.Errorf("%s corner is %v, want the dark tile", name, c)
		}
		if name != "icon-maskable-512.png" {
			if c := at(0.5, 0.21); c != [3]uint32{0xF8, 0x7A, 0x2D} {
				t.Errorf("%s band is %v, want the logo's orange", name, c)
			}
			if c := at(0.5, 0.40); c != [3]uint32{0x0d, 0x05, 0x03} {
				t.Errorf("%s inside the hole is %v, want the dark tile (even-odd fill)", name, c)
			}
		}
	}

	ico, err := os.ReadFile(filepath.Join(dir, "favicon.ico"))
	if err != nil {
		t.Fatal(err)
	}
	if n := binary.LittleEndian.Uint16(ico[4:6]); n != 3 {
		t.Fatalf("favicon.ico holds %d images, want 3", n)
	}
	svg, err := os.ReadFile(filepath.Join(dir, "favicon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`viewBox="0 0 48 48"`, `<rect width="48" height="48" rx="10" fill="#0d0503"/>`,
		`d="M50 5L95 50L50 95L5 50ZM50 30C60 40 60 40 70 50L50 70L30 50Z"/>`,
		`<circle cx="50" cy="50" r="8" fill="#FFFFFF"/>`,
	} {
		if !strings.Contains(string(svg), want) {
			t.Errorf("favicon.svg is missing %q", want)
		}
	}
}

// TestParseLogoSaysWhatItCannotDraw: a logo that uses something this
// tool does not draw is refused with the reason, never drawn wrong.
func TestParseLogoSaysWhatItCannotDraw(t *testing.T) {
	wrap := func(body string) string {
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` + body + `</svg>`
	}
	for name, svg := range map[string]string{
		"no viewBox":         `<svg><path fill="#000000" d="M0 0L1 0L1 1Z"/></svg>`,
		"a relative command": wrap(`<path fill="#000000" d="M0 0l1 0l0 1z"/>`),
		"an arc":             wrap(`<path fill="#000000" d="M0 0A1 1 0 0 0 1 1Z"/>`),
		"a gradient fill":    wrap(`<path fill="url(#g)" d="M0 0L1 0L1 1Z"/>`),
		"a stroke":           wrap(`<path fill="#000000" stroke="#ffffff" d="M0 0L1 0L1 1Z"/>`),
		"a transform":        wrap(`<path fill="#000000" transform="scale(2)" d="M0 0L1 0L1 1Z"/>`),
		"a rect":             wrap(`<rect width="5" height="5" fill="#000000"/>`),
		"a group":            wrap(`<g><path fill="#000000" d="M0 0L1 0L1 1Z"/></g>`),
		"nothing to draw":    wrap(``),
	} {
		if _, err := parseLogo(svg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseLogo(testLogo); err != nil {
		t.Errorf("the supported logo was refused: %v", err)
	}
}
