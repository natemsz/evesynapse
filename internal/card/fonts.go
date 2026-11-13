package card

import (
	"embed"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

//go:embed fonts/*.ttf.b64
var fontFiles embed.FS

// family is one typeface as a chain of font files: a rune is drawn
// from the first file that has a glyph for it, so a subset split
// (Latin, Latin Extended) reads as one face.
type family struct {
	fonts []*sfnt.Font
	// capRatio is the cap height (of H) per pixel of font size, so
	// layout sizes text by its visible cap height.
	capRatio float64
	// spaceEm, when set, overrides the space's advance: the site
	// sets Eve Sans Neue's word space at 0.3em.
	spaceEm float64
}

var (
	// display is Rajdhani 600: names, labels and figures.
	display = mustFamily(0, "rajdhani-600", "rajdhani-600-ext")
	// body is Eve Sans Neue Regular: secondary text.
	body = mustFamily(0.3, "evesansneue-regular")
)

func mustFamily(spaceEm float64, names ...string) *family {
	fam := &family{spaceEm: spaceEm}
	for _, n := range names {
		raw, err := fontFiles.ReadFile("fonts/" + n + ".ttf.b64")
		if err != nil {
			panic("card: font " + n + ": " + err.Error())
		}
		ttf, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		if err != nil {
			panic("card: font " + n + ": " + err.Error())
		}
		f, err := opentype.Parse(ttf)
		if err != nil {
			panic("card: font " + n + ": " + err.Error())
		}
		fam.fonts = append(fam.fonts, f)
	}
	face, err := opentype.NewFace(fam.fonts[0], &opentype.FaceOptions{Size: 100, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic("card: face: " + err.Error())
	}
	b, _, _ := face.GlyphBounds('H')
	fam.capRatio = fix(-b.Min.Y) / 100
	return fam
}

// text is one family at one size. Faces are made per render: an
// opentype face keeps scratch buffers and is not safe to share
// between goroutines.
type text struct {
	fam      *family
	size     float64
	faces    []font.Face
	tracking float64
}

// newText sizes a family by its cap height in pixels; tracking is
// the extra space after every letter but the last.
func newText(fam *family, capPx, tracking float64) *text {
	t := &text{fam: fam, size: capPx / fam.capRatio, tracking: tracking}
	for _, f := range fam.fonts {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: t.size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			panic("card: face: " + err.Error())
		}
		t.faces = append(t.faces, face)
	}
	return t
}

// faceFor picks the first file of the chain carrying the rune.
func (t *text) faceFor(r rune) int {
	var buf sfnt.Buffer
	for i, f := range t.fam.fonts {
		if g, err := f.GlyphIndex(&buf, r); err == nil && g != 0 {
			return i
		}
	}
	return 0
}

// advance is a rune's own advance in pixels.
func (t *text) advance(r rune, fi int) float64 {
	if r == ' ' && t.fam.spaceEm > 0 {
		return t.fam.spaceEm * t.size
	}
	adv, _ := t.faces[fi].GlyphAdvance(r)
	return fix(adv)
}

// kern is the pair adjustment between two runes from the same file.
func (t *text) kern(prev rune, pf int, r rune, fi int) float64 {
	if prev == 0 || pf != fi {
		return 0
	}
	return fix(t.faces[fi].Kern(prev, r))
}

// width measures a string exactly as draw sets it.
func (t *text) width(s string) float64 {
	w := 0.0
	var prev rune
	pf := -1
	n := 0
	for _, r := range s {
		fi := t.faceFor(r)
		w += t.kern(prev, pf, r, fi) + t.advance(r, fi)
		prev, pf = r, fi
		n++
	}
	if n > 1 {
		w += t.tracking * float64(n-1)
	}
	return w
}

// fit shortens a string with an ellipsis until it fits maxW.
func (t *text) fit(s string, maxW float64) string {
	if t.width(s) <= maxW {
		return s
	}
	rs := []rune(s)
	for len(rs) > 0 {
		rs = rs[:len(rs)-1]
		cut := strings.TrimRight(string(rs), " ") + "…"
		if t.width(cut) <= maxW {
			return cut
		}
	}
	return ""
}

// draw sets a string with its baseline at y, starting at x.
func (t *text) draw(dst *image.RGBA, x, y float64, s string, col color.NRGBA) {
	src := image.NewUniform(col)
	var prev rune
	pf := -1
	for _, r := range s {
		fi := t.faceFor(r)
		x += t.kern(prev, pf, r, fi)
		if r != ' ' {
			dot := fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(y * 64)}
			if dr, mask, mp, _, ok := t.faces[fi].Glyph(dot, r); ok {
				draw.DrawMask(dst, dr, src, image.Point{}, mask, mp, draw.Over)
			}
		}
		x += t.advance(r, fi) + t.tracking
		prev, pf = r, fi
	}
}

func fix(v fixed.Int26_6) float64 { return float64(v) / 64 }
