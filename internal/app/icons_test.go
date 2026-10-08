package app

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestIconsAreDrawnFromTheLogo: there is one logo, static/logo.svg.
// The page header shows that file, favicon.svg embeds its shapes
// unchanged, and the PNG icons are pictures of it (cmd/mkicons draws
// them all). A logo replaced without re-running mkicons fails here.
func TestIconsAreDrawnFromTheLogo(t *testing.T) {
	tpl, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tpl), `<img class="wordmark-glyph" src="/static/logo.svg?v={{.AssetVersion}}"`) {
		t.Fatal("the page header does not show static/logo.svg")
	}
	logo, err := fs.ReadFile(staticFS, "static/logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	svg, err := fs.ReadFile(staticFS, "static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	shapes := regexp.MustCompile(`(?s)<(?:path|circle)\b[^>]*/>`).FindAllString(string(logo), -1)
	if len(shapes) == 0 {
		t.Fatal("logo.svg has no shapes")
	}
	// Every shape of the logo, exactly as the logo file has it.
	for _, shape := range shapes {
		if !strings.Contains(string(svg), strings.TrimSpace(shape)) {
			t.Errorf("favicon.svg does not draw this shape of logo.svg (run: go run ./cmd/mkicons): %.80s…", shape)
		}
	}

	type want struct {
		file string
		size int
	}
	for _, w := range []want{
		{"static/icon-192.png", 192}, {"static/icon-512.png", 512},
		{"static/icon-maskable-512.png", 512}, {"static/apple-touch-icon.png", 180},
	} {
		raw, err := fs.ReadFile(staticFS, w.file)
		if err != nil {
			t.Fatalf("read %s: %v", w.file, err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s is not a PNG: %v", w.file, err)
		}
		if b := img.Bounds(); b.Dx() != w.size || b.Dy() != w.size {
			t.Errorf("%s is %dx%d, want %dx%d", w.file, b.Dx(), b.Dy(), w.size, w.size)
		}
		// The centre node is white; the corners are the dark background.
		c := img.Bounds().Dx() / 2
		r, g, b, _ := img.At(c, c).RGBA()
		if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
			t.Errorf("%s centre pixel is (%d,%d,%d), want the white centre node of the current logo", w.file, r>>8, g>>8, b>>8)
		}
		cr, cg, cb, _ := img.At(2, 2).RGBA()
		if cr>>8 > 40 || cg>>8 > 40 || cb>>8 > 40 {
			t.Errorf("%s corner is (%d,%d,%d), want the dark background", w.file, cr>>8, cg>>8, cb>>8)
		}
	}
}

// TestFaviconICOHoldsThreeSizes: /favicon.ico is a real ICO of 16, 32
// and 48 pixel PNG images.
func TestFaviconICOHoldsThreeSizes(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 6 || binary.LittleEndian.Uint16(raw[0:2]) != 0 || binary.LittleEndian.Uint16(raw[2:4]) != 1 {
		t.Fatal("favicon.ico has no ICO header")
	}
	n := int(binary.LittleEndian.Uint16(raw[4:6]))
	if n != 3 {
		t.Fatalf("favicon.ico holds %d images, want 3", n)
	}
	wantSize := []int{16, 32, 48}
	for i := 0; i < n; i++ {
		entry := raw[6+16*i : 6+16*(i+1)]
		length := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset+length > len(raw) {
			t.Fatalf("image %d runs past the end of the file", i)
		}
		img, err := png.Decode(bytes.NewReader(raw[offset : offset+length]))
		if err != nil {
			t.Fatalf("image %d is not a PNG: %v", i, err)
		}
		if img.Bounds().Dx() != wantSize[i] {
			t.Errorf("image %d is %dpx, want %dpx", i, img.Bounds().Dx(), wantSize[i])
		}
	}
}
