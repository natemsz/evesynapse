package card

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

// ProfileW and ProfileH are the share sheet's size: a 4:5 portrait,
// the shape chat apps and phone feeds show largest.
const ProfileW, ProfileH = 1200, 1500

// Profile is the share sheet: the large card a pilot hands out by
// link or as a download, with more than the 1200x630 summary card
// has room for. It draws only what it is handed; the caller keeps
// it to public record and killboard facts.
type Profile struct {
	// Kind is the tag at the header's right.
	Kind string
	// Picture is the portrait (cropped to a square).
	Picture image.Image
	Title   string
	// Lines are the secondary lines under the name (corporation,
	// alliance), one each.
	Lines []string
	// Chips are short labelled facts under the lines (security
	// status, birthday).
	Chips []Stat
	// Figures is the large tile row; Minor the smaller row under
	// it. Up to four each.
	Figures []Stat
	Minor   []Stat
	// Ships is the left panel: the hulls flown most, with a bar
	// for each one's share.
	ShipsLabel string
	Ships      []ProfileRow
	// Hours is the right panel's 24-hour histogram, each 0 to 1,
	// hour 0 first; HoursNote names the busy window.
	HoursLabel string
	Hours      []float64
	HoursNote  string
	// Facts are short labelled values in one row under the
	// histogram, a column each.
	Facts []Detail
	// Foot is the address at the bottom left; Note the line at
	// the bottom right.
	Foot string
	Note string
}

// ProfileRow is one row of a profile panel: an icon, a name, a
// count and the row's share of the top row.
type ProfileRow struct {
	Icon  image.Image
	Name  string
	Count string
	Frac  float64
}

// The sheet's geometry, in sheet pixels.
const (
	pMargin   = 64.0
	pHeadMid  = 84.0
	pRuleY    = 148.0
	pHeroY    = 196.0
	pHeroSide = 340.0
	pColX     = pMargin + pHeroSide + 52
	pTileY    = 588.0
	pTileH    = 150.0
	pMinorY   = pTileY + pTileH + 20
	pMinorH   = 112.0
	pPanelY   = pMinorY + pMinorH + 44
	pPanelBot = 1370.0
	pFootBase = 1436.0
	pTileGap  = 18.0
)

// RenderProfile draws a share sheet.
func RenderProfile(p Profile) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, ProfileW, ProfileH))
	field(img)
	const right = ProfileW - pMargin

	drawMark(img, pMargin, pHeadMid-30, 60)
	drawWordmark(img, pMargin+80, pHeadMid, 22)
	if p.Kind != "" {
		tag := newText(display, 17, 5)
		tag.draw(img, right-tag.width(p.Kind), pHeadMid+17.0/2, p.Kind, hiC)
	}
	fill(img, rect(pMargin, pRuleY, right, pRuleY+2), linear(pMargin, 0, right, 0, alpha(ruleGold, .6), alpha(ruleEmber, .6)))

	// Hero: the portrait panel and the identity column.
	drawFramedPicture(img, p.Picture, pMargin, pHeroY, pHeroSide, 16)
	avail := right - pColX
	y := pHeroY + 78
	if p.Title != "" {
		capPx := 64.0
		name := newText(display, capPx, 0)
		for name.width(p.Title) > avail && capPx > 40 {
			capPx -= 2
			name = newText(display, capPx, 0)
		}
		name.draw(img, pColX, y, name.fit(p.Title, avail), textBright)
	}
	y += 58
	line := newText(body, 26, 0)
	for i, l := range p.Lines {
		col := mutedC
		if i == 0 {
			col = textC
		}
		line.draw(img, pColX, y, line.fit(l, avail), col)
		y += 44
	}
	if len(p.Chips) > 0 {
		drawChips(img, p.Chips, pColX, y-4, right, pHeroY+pHeroSide)
	}

	drawTiles(img, p.Figures, pTileY, pTileH, 46, right)
	drawTiles(img, p.Minor, pMinorY, pMinorH, 32, right)

	// Lower panels: ships at the left, hours and facts at the right.
	mid := pMargin + (right-pMargin)*0.52
	if len(p.Ships) > 0 {
		drawShipPanel(img, p.ShipsLabel, p.Ships, pMargin, pPanelY, mid-24, pPanelBot)
	}
	hx0 := mid + 24
	if len(p.Ships) == 0 {
		hx0 = pMargin
	}
	drawHoursPanel(img, p, hx0, pPanelY, right, pPanelBot)

	foot := newText(body, 19, 0)
	foot.draw(img, pMargin, pFootBase, p.Foot, muted2C)
	if p.Note != "" {
		note := newText(body, 17, 0)
		note.draw(img, right-note.width(p.Note), pFootBase, p.Note, muted2C)
	}
	return img
}

// ProfilePNG draws a share sheet and encodes it.
func ProfilePNG(p Profile) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, RenderProfile(p)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawFramedPicture draws a picture cropped square into a rounded
// frame with the accent hairline; without one, the logo mark.
func drawFramedPicture(img *image.RGBA, pic image.Image, x0, y0, side, r float64) {
	x1, y1 := x0+side, y0+side
	fill(img, roundRect(x0, y0, x1, y1, r), solid(alpha(accentC, .45)))
	const inset = 2.0
	drawPicture(img, pic, x0+inset, y0+inset, side-2*inset, r-inset)
	if pic == nil {
		cx, cy := x0+side/2, y0+side/2
		drawMark(img, cx-side*0.22, cy-side*0.22, side*0.44)
	}
}

// drawPicture fills a rounded square with a picture cropped to fill
// it (panel colour where there is none).
func drawPicture(img *image.RGBA, pic image.Image, x0, y0, side, r float64) {
	inner := roundRect(x0, y0, x0+side, y0+side, r)
	if pic == nil {
		fill(img, inner, solid(panelC))
		return
	}
	n := int(math.Round(side))
	sb := pic.Bounds()
	crop := sb
	if sb.Dx() > sb.Dy() {
		off := (sb.Dx() - sb.Dy()) / 2
		crop = image.Rect(sb.Min.X+off, sb.Min.Y, sb.Min.X+off+sb.Dy(), sb.Max.Y)
	} else if sb.Dy() > sb.Dx() {
		off := (sb.Dy() - sb.Dx()) / 2
		crop = image.Rect(sb.Min.X, sb.Min.Y+off, sb.Max.X, sb.Min.Y+off+sb.Dx())
	}
	scaled := image.NewRGBA(image.Rect(0, 0, n, n))
	xdraw.Draw(scaled, scaled.Bounds(), image.NewUniform(panelC), image.Point{}, xdraw.Src)
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), pic, crop, xdraw.Over, nil)
	ox, oy := int(x0), int(y0)
	fill(img, inner, func(x, y float64) color.NRGBA {
		px, py := int(x)-ox, int(y)-oy
		if px < 0 || py < 0 || px >= n || py >= n {
			return panelC
		}
		i := scaled.PixOffset(px, py)
		return color.NRGBA{scaled.Pix[i], scaled.Pix[i+1], scaled.Pix[i+2], 0xff}
	})
}

// drawChips sets labelled facts in rows of framed pills, wrapping
// to a new row at the right edge; a pill with no row left above
// bottom is not drawn.
func drawChips(img *image.RGBA, chips []Stat, x, y, right, bottom float64) {
	label := newText(display, 13, 3)
	val := newText(display, 22, 0)
	const padX, h = 16.0, 48.0
	x0 := x
	for _, c := range chips {
		lw := label.width(c.Label)
		vw := 0.0
		for _, s := range c.Value {
			vw += val.width(s.Text)
		}
		w := padX + lw + 12 + vw + padX
		if x+w > right && x > x0 {
			x, y = x0, y+h+14
		}
		if y+h > bottom || x+w > right {
			return
		}
		fill(img, roundRect(x, y, x+w, y+h, 10), solid(alpha(textC, .14)))
		fill(img, roundRect(x+1, y+1, x+w-1, y+h-1, 9), solid(alpha(panelC, .92)))
		label.draw(img, x+padX, y+h/2+13.0/2, c.Label, muted2C)
		vx := x + padX + lw + 12
		for _, s := range c.Value {
			col := s.Color
			if col == (color.NRGBA{}) {
				col = textBright
			}
			val.draw(img, vx, y+h/2+22.0/2, s.Text, col)
			vx += val.width(s.Text)
		}
		x += w + 14
	}
}

// drawTiles draws up to four tiles on a four-column grid, each a
// label over a figure, edged at the left in the accent, the way
// the fitting tool's glance tiles read.
func drawTiles(img *image.RGBA, stats []Stat, y0, h, figCap, right float64) {
	n := min(len(stats), 4)
	if n == 0 {
		return
	}
	// Four columns always, so a short row lines up with the one
	// above it instead of stretching.
	const cols = 4
	w := (right - pMargin - pTileGap*(cols-1)) / cols
	label := newText(display, 14, 3.5)
	fig := newText(display, figCap, 0)
	for i := range n {
		st := stats[i]
		x0 := pMargin + float64(i)*(w+pTileGap)
		drawTile(img, x0, y0, x0+w, y0+h, accentC)
		label.draw(img, x0+26, y0+22+14, st.Label, muted2C)
		segs := fitSegs(fig, st.Value, w-48)
		fx := x0 + 26
		for _, s := range segs {
			c := s.Color
			if c == (color.NRGBA{}) {
				c = hiC
			}
			fig.draw(img, fx, y0+h-26, s.Text, c)
			fx += fig.width(s.Text)
		}
	}
}

// drawTile is one tile: a hairline frame on a faint fill, with a
// wash of its colour from the top left and a solid left edge.
func drawTile(img *image.RGBA, x0, y0, x1, y1 float64, k color.NRGBA) {
	const r = 10.0
	fill(img, roundRect(x0, y0, x1, y1, r), solid(alpha(textC, .10)))
	fill(img, roundRect(x0+1, y0+1, x1-1, y1-1, r-1), solid(alpha(panelC, .94)))
	fill(img, roundRect(x0+1, y0+1, x1-1, y1-1, r-1), linear(x0, y0, x0+(x1-x0)*.7, y1, alpha(k, .16), alpha(k, 0)))
	fill(img, roundRect(x0, y0, x0+4, y1, 2), solid(k))
}

// drawShipPanel lists rows with icons, names, counts and bars.
func drawShipPanel(img *image.RGBA, title string, rows []ProfileRow, x0, y0, x1, y1 float64) {
	drawPanelFrame(img, x0, y0, x1, y1)
	label := newText(display, 15, 3.5)
	label.draw(img, x0+28, y0+44, title, muted2C)
	name := newText(body, 22, 0)
	count := newText(display, 22, 0)
	const icon, rowH = 48.0, 74.0
	y := y0 + 76
	for _, r := range rows {
		if y+icon > y1-20 {
			break
		}
		drawPicture(img, r.Icon, x0+28, y, icon, 8)
		tx := x0 + 28 + icon + 18
		cw := count.width(r.Count)
		name.draw(img, tx, y+22, name.fit(r.Name, x1-28-cw-16-tx), textC)
		count.draw(img, x1-28-cw, y+22, r.Count, textBright)
		by := y + 35
		fill(img, roundRect(tx, by, x1-28, by+8, 4), solid(alpha(textC, .08)))
		if f := math.Max(0, math.Min(1, r.Frac)); f > 0 {
			fill(img, roundRect(tx, by, tx+math.Max(8, (x1-28-tx)*f), by+8, 4), solid(hiC))
		}
		y += rowH
	}
}

// drawHoursPanel draws the 24-hour histogram, its axis and note,
// then the labelled facts under it.
func drawHoursPanel(img *image.RGBA, p Profile, x0, y0, x1, y1 float64) {
	drawPanelFrame(img, x0, y0, x1, y1)
	label := newText(display, 15, 3.5)
	y := y0 + 44
	if len(p.Hours) == 24 {
		label.draw(img, x0+28, y, p.HoursLabel, muted2C)
		const barTop, barH = 76.0, 150.0
		bx0, bx1 := x0+28, x1-28
		gap := 4.0
		bw := (bx1 - bx0 - gap*23) / 24
		base := y0 + barTop + barH
		for h, f := range p.Hours {
			x := bx0 + float64(h)*(bw+gap)
			fill(img, roundRect(x, y0+barTop, x+bw, base, 3), solid(alpha(textC, .05)))
			if f > 0 {
				top := base - math.Max(4, barH*math.Min(1, f))
				fill(img, roundRect(x, top, x+bw, base, 3), linear(0, top, 0, base, hiC, alpha(accentC, .55)))
			}
		}
		axis := newText(display, 13, 1)
		for _, h := range []int{0, 6, 12, 18} {
			t := []string{"00", "06", "12", "18"}[h/6]
			axis.draw(img, bx0+float64(h)*(bw+gap), base+28, t, muted2C)
		}
		axis.draw(img, bx1-axis.width("24"), base+28, "24", muted2C)
		y = base + 70
		if p.HoursNote != "" {
			note := newText(body, 21, 0)
			note.draw(img, x0+28, y, note.fit(p.HoursNote, x1-x0-56), textC)
			y += 56
		}
	}
	// The facts share one row, a column each.
	if n := len(p.Facts); n > 0 && y+40 <= y1 {
		lineT := newText(display, 26, 0)
		colW := (x1 - x0 - 56) / float64(n)
		for i, f := range p.Facts {
			fx := x0 + 28 + float64(i)*colW
			label.draw(img, fx, y, f.Label, muted2C)
			lineT.draw(img, fx, y+38, lineT.fit(f.Text, colW-16), textBright)
		}
	}
}

// drawPanelFrame is a panel's hairline frame on a faint fill.
func drawPanelFrame(img *image.RGBA, x0, y0, x1, y1 float64) {
	const r = 14.0
	fill(img, roundRect(x0, y0, x1, y1, r), solid(alpha(textC, .10)))
	fill(img, roundRect(x0+1, y0+1, x1-1, y1-1, r-1), solid(alpha(panelC, .9)))
}
