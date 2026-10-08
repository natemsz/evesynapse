package app

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"io/fs"
	"strings"
	"testing"
)

// TestIconsAreTheCurrentLogo: the page header shows static/logo.svg,
// the service worker's badge and the linked icons exist, and the PNG
// icons are pictures of the current drawings: the light centre node on
// the orange tile. (cmd/mkicons draws them from icon.svg and
// favicon.svg, and its own test fails when they are out of date.)
func TestIconsAreTheCurrentLogo(t *testing.T) {
	tpl, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tpl), `<img class="wordmark-glyph" src="/static/logo.svg?v={{.AssetVersion}}"`) {
		t.Fatal("the page header does not show static/logo.svg")
	}
	for _, name := range []string{"static/logo.svg", "static/icon.svg", "static/favicon.svg", "static/badge-96.png"} {
		if raw, err := fs.ReadFile(staticFS, name); err != nil || len(raw) == 0 {
			t.Fatalf("%s is missing: %v", name, err)
		}
	}
	worker, err := fs.ReadFile(staticFS, "static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), `badge: '/static/badge-96.png'`) {
		t.Error("the service worker does not use the monochrome badge")
	}

	type want struct {
		file   string
		size   int
		square bool // the tile runs to every edge
	}
	for _, w := range []want{
		{"static/icon-192.png", 192, false}, {"static/icon-512.png", 512, false},
		{"static/icon-maskable-512.png", 512, true}, {"static/apple-touch-icon.png", 180, true},
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
		c := img.Bounds().Dx() / 2
		r, g, b, _ := img.At(c, c).RGBA()
		if r>>8 < 240 || g>>8 < 230 || b>>8 < 210 {
			t.Errorf("%s centre pixel is (%d,%d,%d), want the light centre node of the current logo", w.file, r>>8, g>>8, b>>8)
		}
		// A little way in from the middle of the left edge: the orange tile.
		er, eg, eb, ea := img.At(w.size/20, c).RGBA()
		if ea>>8 != 255 || er>>8 < 200 || eb>>8 > 120 || eg>>8 < 60 {
			t.Errorf("%s edge is (%d,%d,%d,%d), want the orange tile", w.file, er>>8, eg>>8, eb>>8, ea>>8)
		}
		_, _, _, ca := img.At(1, 1).RGBA()
		if w.square && ca>>8 != 255 {
			t.Errorf("%s corner is clear; this icon's tile should run to the edge", w.file)
		}
		if !w.square && ca>>8 != 0 {
			t.Errorf("%s corner is not clear; this icon has rounded corners", w.file)
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
