package app

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"io/fs"
	"testing"
)

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
