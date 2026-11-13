// Package card draws EveSynapse's summary cards: the 1200x630 PNG a
// page's og:image points at, so a shared link unfurls with the
// page's own figures. Every card kind shares one template: the
// minmatar-v3 field, the logo mark and wordmark with the kind tag,
// a picture panel (portrait or ship render), a name, a secondary
// line, a row of labelled figures and the site address.
//
// The package draws only what it is handed; reading stored data,
// fetching pictures and caching the PNGs are the caller's work.
package card

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

// W and H are the card's size, the Open Graph large image ratio.
const W, H = 1200, 630

// Card is one card's content.
type Card struct {
	// Kind is the tag at the header's right: "PILOT", "KILL".
	Kind string
	// Picture fills the panel at the left (cropped to a square);
	// nil leaves the panel empty.
	Picture image.Image
	// Title is the name line; Subtitle the secondary line under it.
	Title    string
	Subtitle string
	// Stats is the figure row; empty draws no row.
	Stats []Stat
	// Detail is an optional labelled line under the figures, for a
	// value too long to read as a figure (a pilot's name).
	Detail *Detail
}

// Detail is a labelled line of secondary text.
type Detail struct {
	Label string
	Text  string
}

// Stat is one labelled figure.
type Stat struct {
	Label string
	Value []Seg
}

// Seg is a run of a figure in its own colour; the zero colour is
// the figure colour (hi).
type Seg struct {
	Text  string
	Color color.NRGBA
}

// Figure is a one-run figure in the figure colour.
func Figure(s string) []Seg { return []Seg{{Text: s}} }

// The minmatar-v3 scheme tokens.
var (
	groundTop  = hex(0x191513)
	groundBot  = hex(0x0f0d0c)
	glow1      = color.NRGBA{242, 102, 25, 71} // rgba(242,102,25,.28)
	glow2      = color.NRGBA{143, 45, 12, 38}  // rgba(143,45,12,.15)
	panelC     = hex(0x151312)
	textC      = hex(0xfaf4e8)
	textBright = hex(0xfffbf2)
	mutedC     = hex(0xbfa585)
	muted2C    = hex(0xcdb99c)
	hiC        = hex(0xff983d)
	accentC    = hex(0xf26619)
	ruleGold   = hex(0xffc36c)
	ruleEmber  = hex(0xdc4315)
)

// The template's geometry, in card pixels.
const (
	margin    = 56.0
	headMidY  = 74.0
	markSize  = 54.0
	wordX     = 132.0
	wordCap   = 20.0
	tagCap    = 15.0
	ruleY     = 130.0
	panelY    = 172.0
	panelSide = 328.0
	panelR    = 14.0
	colX      = margin + panelSide + 46
	nameBase  = 250.0
	nameCap   = 50.0
	nameMin   = 34.0
	subBase   = 306.0
	subCap    = 22.0
	labelBase = 386.0
	labelCap  = 13.0
	// The detail line sits under the figures.
	detailLabel = 492.0
	detailBase  = 524.0
	figBase     = 440.0
	figCap      = 30.5
	statGap     = 64.0
	footBase    = 588.0
	footCap     = 16.0
)

// Render draws a card.
func Render(c Card) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	field(img)

	// Header: the mark, the wordmark, the kind tag; under it the
	// accent rule stepping gold to ember.
	drawMark(img, margin, headMidY-markSize/2, markSize)
	drawWordmark(img, wordX, headMidY, wordCap)
	if c.Kind != "" {
		tag := newText(display, tagCap, 4.5)
		tag.draw(img, W-margin-tag.width(c.Kind), headMidY+tagCap/2, c.Kind, hiC)
	}
	fill(img, rect(margin, ruleY, W-margin, ruleY+2), linear(margin, 0, W-margin, 0, alpha(ruleGold, .6), alpha(ruleEmber, .6)))

	drawPanel(img, c.Picture)

	right := W - margin
	avail := right - colX
	if c.Title != "" {
		capPx := nameCap
		name := newText(display, capPx, 0)
		for name.width(c.Title) > avail && capPx > nameMin {
			capPx -= 2
			name = newText(display, capPx, 0)
		}
		name.draw(img, colX, nameBase, name.fit(c.Title, avail), textBright)
	}
	if c.Subtitle != "" {
		sub := newText(body, subCap, 0)
		sub.draw(img, colX, subBase, sub.fit(c.Subtitle, avail), mutedC)
	}
	drawStats(img, c.Stats, right)
	if d := c.Detail; d != nil && d.Text != "" {
		label := newText(display, labelCap, 3)
		label.draw(img, colX, detailLabel, d.Label, muted2C)
		line := newText(body, subCap, 0)
		line.draw(img, colX, detailBase, line.fit(d.Text, avail), textC)
	}

	foot := newText(body, footCap, 0)
	foot.draw(img, margin, footBase, "evesynapse.app", muted2C)
	return img
}

// PNG draws a card and encodes it.
func PNG(c Card) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, Render(c)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// field paints the ground and the scheme's glow as the approved
// card draws them: the ground falls diagonally from ground-2 at
// the top left to ground by mid-card, and the glow is an ellipse
// hanging from the top edge toward the right, glow-1 at its heart
// through glow-2 to nothing.
func field(img *image.RGBA) {
	const cx, rx, ry = 1020.0, 560.0, 240.0
	clear := color.NRGBA{glow2.R, glow2.G, glow2.B, 0}
	for y := range H {
		for x := range W {
			g := mix(groundTop, groundBot, float64(x)/900+float64(y)/480)
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = g.R, g.G, g.B, 0xff
			dx, dy := (float64(x)+.5-cx)/rx, (float64(y)+.5+10)/ry
			d := math.Sqrt(dx*dx + dy*dy)
			if d >= 1 {
				continue
			}
			var c color.NRGBA
			if d < .52 {
				c = mix(glow1, glow2, d/.52)
			} else {
				c = mix(glow2, clear, (d-.52)/.48)
			}
			blend(img, x, y, c, 1)
		}
	}
}

// drawPanel draws the picture panel: a rounded square on panel
// glass with an accent hairline, the picture cropped to fill it.
func drawPanel(img *image.RGBA, pic image.Image) {
	x0, y0 := margin, panelY
	x1, y1 := x0+panelSide, y0+panelSide
	fill(img, roundRect(x0, y0, x1, y1, panelR), solid(alpha(accentC, .45)))
	const inset = 2.0
	ix0, iy0, ix1, iy1 := x0+inset, y0+inset, x1-inset, y1-inset
	inner := roundRect(ix0, iy0, ix1, iy1, panelR-inset)
	if pic == nil {
		fill(img, inner, solid(panelC))
		return
	}
	side := int(ix1 - ix0)
	sb := pic.Bounds()
	crop := sb
	if sb.Dx() > sb.Dy() {
		off := (sb.Dx() - sb.Dy()) / 2
		crop = image.Rect(sb.Min.X+off, sb.Min.Y, sb.Min.X+off+sb.Dy(), sb.Max.Y)
	} else if sb.Dy() > sb.Dx() {
		off := (sb.Dy() - sb.Dx()) / 2
		crop = image.Rect(sb.Min.X, sb.Min.Y+off, sb.Max.X, sb.Min.Y+off+sb.Dx())
	}
	scaled := image.NewRGBA(image.Rect(0, 0, side, side))
	xdraw.Draw(scaled, scaled.Bounds(), image.NewUniform(panelC), image.Point{}, xdraw.Src)
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), pic, crop, xdraw.Over, nil)
	ox, oy := int(ix0), int(iy0)
	fill(img, inner, func(x, y float64) color.NRGBA {
		px, py := int(x)-ox, int(y)-oy
		if px < 0 || py < 0 || px >= side || py >= side {
			return panelC
		}
		i := scaled.PixOffset(px, py)
		return color.NRGBA{scaled.Pix[i], scaled.Pix[i+1], scaled.Pix[i+2], 0xff}
	})
}

// drawStats draws the figure row left to right from the text
// column, each column as wide as its label or figure; a figure
// that runs past the right edge is shortened, and a column with no
// room left is not drawn.
func drawStats(img *image.RGBA, stats []Stat, right float64) {
	label := newText(display, labelCap, 3)
	fig := newText(display, figCap, 0)
	x := colX
	for _, st := range stats {
		avail := right - x
		if avail < 80 {
			break
		}
		lw := label.width(st.Label)
		segs := fitSegs(fig, st.Value, avail)
		fw := 0.0
		for _, s := range segs {
			fw += fig.width(s.Text)
		}
		label.draw(img, x, labelBase, st.Label, muted2C)
		fx := x
		for _, s := range segs {
			c := s.Color
			if c == (color.NRGBA{}) {
				c = hiC
			}
			fig.draw(img, fx, figBase, s.Text, c)
			fx += fig.width(s.Text)
		}
		x += math.Max(lw, fw) + statGap
	}
}

// fitSegs shortens a figure's runs to fit maxW, cutting the last
// run that still has room.
func fitSegs(t *text, segs []Seg, maxW float64) []Seg {
	out := make([]Seg, 0, len(segs))
	used := 0.0
	for _, s := range segs {
		w := t.width(s.Text)
		if used+w <= maxW {
			out = append(out, s)
			used += w
			continue
		}
		if cut := t.fit(s.Text, maxW-used); cut != "" {
			out = append(out, Seg{Text: cut, Color: s.Color})
		}
		break
	}
	return out
}

func hex(v uint32) color.NRGBA {
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

func alpha(c color.NRGBA, a float64) color.NRGBA {
	c.A = uint8(a*255 + .5)
	return c
}

// secColors are the killboard's security tiers (the .sec-N classes'
// fallback colours), tier 10 for 1.0 down to tier 0 for null.
var secColors = [11]color.NRGBA{
	hex(0xc0508a), hex(0xb33a3a), hex(0xd0262c), hex(0xce440f), hex(0xdc6d07),
	hex(0xf3fd82), hex(0x71e554), hex(0x60dba3), hex(0x4ecef8), hex(0x3a9aeb), hex(0x2c74e0),
}

// SecColor is a security tier's colour (the tier secRound gives).
func SecColor(tier int) color.NRGBA {
	if tier < 0 || tier > 10 {
		return textC
	}
	return secColors[tier]
}
