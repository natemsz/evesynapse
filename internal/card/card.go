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
	// Picture fills the panel at the left (cropped to a square).
	// Without one the panel shows Badge, Bars or the logo mark
	// (Mark), in that order of preference, or stays empty.
	Picture image.Image
	Badge   *Badge
	Bars    []Bar
	Mark    bool
	// Title is the name line; Subtitle the secondary line under it.
	Title    string
	Subtitle string
	// Stats is the figure row; empty draws no row.
	Stats []Stat
	// Detail is an optional labelled line under the figures, for a
	// value too long to read as a figure (a pilot's name).
	Detail *Detail
	// Spark is an optional labelled value line in the band under
	// the figures (a market card's price history). It shares the
	// Detail line's band, so a card carries one or the other.
	Spark *Spark
}

// Badge is one large figure centred in the panel with a caption
// under it (a system's security status).
type Badge struct {
	Text    string
	Color   color.NRGBA
	Caption string
}

// Bar is one row of a proportion chart in the panel: a label and
// count over a bar filled to Frac (0 to 1) in its colour.
type Bar struct {
	Label string
	Count string
	Frac  float64
	Color color.NRGBA
}

// Detail is a labelled line of secondary text.
type Detail struct {
	Label string
	Text  string
}

// Spark is a labelled line of values, oldest first, drawn scaled
// to its band (a price history: one value per day).
type Spark struct {
	Label  string
	Values []float64
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
	// maxUpscale bounds how far a picture is enlarged to fill the
	// panel.
	maxUpscale = 2.6
	// The detail line sits under the figures.
	detailLabel = 492.0
	detailBase  = 524.0
	// The sparkline's band under its caption, in the same space.
	sparkTop = 504.0
	sparkBot = 554.0
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

	drawPanel(img, c)

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
		// With no figure row the detail line takes its place.
		lb, tb := detailLabel, detailBase
		if len(c.Stats) == 0 {
			lb, tb = labelBase, labelBase+(detailBase-detailLabel)
		}
		label := newText(display, labelCap, 3)
		label.draw(img, colX, lb, d.Label, muted2C)
		line := newText(body, subCap, 0)
		line.draw(img, colX, tb, line.fit(d.Text, avail), textC)
	}
	if s := c.Spark; s != nil && c.Detail == nil && len(s.Values) >= 2 {
		if s.Label != "" {
			label := newText(display, labelCap, 3)
			label.draw(img, colX, detailLabel, s.Label, muted2C)
		}
		drawSpark(img, s.Values, colX, right, sparkTop, sparkBot)
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

// drawPanel draws the panel: a rounded square on panel glass with
// an accent hairline, holding the picture cropped to fill it, or
// without one the badge, the bars or the logo mark.
func drawPanel(img *image.RGBA, c Card) {
	x0, y0 := margin, panelY
	x1, y1 := x0+panelSide, y0+panelSide
	fill(img, roundRect(x0, y0, x1, y1, panelR), solid(alpha(accentC, .45)))
	const inset = 2.0
	ix0, iy0, ix1, iy1 := x0+inset, y0+inset, x1-inset, y1-inset
	inner := roundRect(ix0, iy0, ix1, iy1, panelR-inset)
	pic := c.Picture
	if pic == nil {
		fill(img, inner, solid(panelC))
		cx, cy := (ix0+ix1)/2, (iy0+iy1)/2
		switch {
		case c.Badge != nil:
			drawBadge(img, *c.Badge, cx, cy)
		case len(c.Bars) > 0:
			drawBars(img, c.Bars, ix0+36, ix1-36, cy)
		case c.Mark:
			drawMark(img, cx-75, cy-75, 150)
		}
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
	// A small picture (a 64px item icon) is not blown up past
	// maxUpscale: it sits centred on the panel instead.
	dst := scaled.Bounds()
	if s := crop.Dx(); s > 0 && float64(side)/float64(s) > maxUpscale {
		d := int(float64(s) * maxUpscale)
		off := (side - d) / 2
		dst = image.Rect(off, off, off+d, off+d)
	}
	xdraw.CatmullRom.Scale(scaled, dst, pic, crop, xdraw.Over, nil)
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

// drawBadge sets a large figure centred on (cx, cy) with its caption
// under it.
func drawBadge(img *image.RGBA, b Badge, cx, cy float64) {
	const figPx, gap = 96.0, 34.0
	fig := newText(display, figPx, 0)
	text := fig.fit(b.Text, panelSide-48)
	col := b.Color
	if col == (color.NRGBA{}) {
		col = hiC
	}
	top := cy - (figPx+gap+labelCap)/2
	fig.draw(img, cx-fig.width(text)/2, top+figPx, text, col)
	if b.Caption != "" {
		caption := newText(display, labelCap, 3)
		caption.draw(img, cx-caption.width(b.Caption)/2, top+figPx+gap+labelCap, b.Caption, muted2C)
	}
}

// drawBars draws a proportion chart between x0 and x1, centred on
// cy: per row a label with its count at the right, over a track
// filled to the row's fraction.
func drawBars(img *image.RGBA, bars []Bar, x0, x1, cy float64) {
	const rowH, countCap, barGap, barH = 76.0, 22.0, 12.0, 10.0
	label := newText(display, labelCap, 3)
	count := newText(display, countCap, 0)
	block := float64(len(bars))*rowH - (rowH - countCap - barGap - barH)
	y := cy - block/2
	for _, b := range bars {
		base := y + countCap
		label.draw(img, x0, base, b.Label, muted2C)
		count.draw(img, x1-count.width(b.Count), base, b.Count, textBright)
		by := base + barGap
		fill(img, roundRect(x0, by, x1, by+barH, barH/2), solid(alpha(textC, .08)))
		if f := math.Max(0, math.Min(1, b.Frac)); f > 0 {
			w := math.Max(barH, (x1-x0)*f)
			fill(img, roundRect(x0, by, x0+w, by+barH, barH/2), solid(b.Color))
		}
		y += rowH
	}
}

// drawSpark draws a value line across the band x0..x1, y0..y1: a
// soft fill under the line, the line itself in the figure colour
// and a dot on the newest value. Values scale to the band with a
// margin at both ends, so a flat series sits mid-band and a nearly
// flat one still shows its movement.
func drawSpark(img *image.RGBA, values []float64, x0, x1, y0, y1 float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	pad := (hi - lo) * .12
	if pad == 0 {
		pad = math.Max(1, math.Abs(hi)*.02)
	}
	lo, hi = lo-pad, hi+pad
	pts := make([]pt, len(values))
	for i, v := range values {
		pts[i] = pt{
			x0 + (x1-x0)*float64(i)/float64(len(values)-1),
			y1 - (v-lo)/(hi-lo)*(y1-y0),
		}
	}
	under := append(append([]pt{}, pts...), pt{x1, y1}, pt{x0, y1})
	fill(img, shape{under}, linear(0, y0, 0, y1, alpha(hiC, .20), alpha(hiC, .02)))
	const w = 2.6
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b.x-a.x, b.y-a.y
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*w/2, dx/l*w/2
		fill(img, shape{{{a.x + nx, a.y + ny}, {b.x + nx, b.y + ny}, {b.x - nx, b.y - ny}, {a.x - nx, a.y - ny}}}, solid(hiC))
		fill(img, circle(a.x, a.y, w/2), solid(hiC))
	}
	last := pts[len(pts)-1]
	fill(img, circle(last.x, last.y, 3.4), solid(hiC))
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
