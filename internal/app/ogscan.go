package app

// ---------------------------------------------------------------------------
// Scan link unfurls (Step 3): GET /intel/scan/{id}/og.png draws a
// 1200x630 preview of a stored reading -- system, headline counts,
// composition -- so a scan link pasted into Discord carries its
// intel with it. The drawing is pure standard library: the canvas
// is an NRGBA, and the letterforms are the site's Rajdhani 700
// baked into ogfont.go as 4-bit alpha bitmaps, drawn scaled down
// only. A reading never changes, so the image is cacheable; a
// corrected system simply draws anew.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
)

// ogGlyphPix is ogfont.go's packed bitmaps, decoded once.
var ogGlyphPix = func() map[rune][]byte {
	out := make(map[rune][]byte, len(ogGlyphs))
	for r, g := range ogGlyphs {
		if g.data == "" {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(g.data); err == nil {
			out[r] = raw
		}
	}
	return out
}()

func (app *Application) handleScanOG(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if !validScanID(id) {
		http.NotFound(w, r)
		return
	}
	row, err := app.queries.GetScan(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var res scanResult
	if json.Unmarshal([]byte(row.Result), &res) != nil {
		http.NotFound(w, r)
		return
	}
	meta := ogMeta{At: row.CreatedAt}
	if res.SystemID > 0 {
		names := app.newKillNames(ctx)
		sys := names.system(res.SystemID)
		meta.System, meta.Region = sys.Name, names.region(sys.RegionID)
		meta.SecText, _ = secRound(sys.Security)
		meta.Sec, meta.HasSec = sys.Security, sys.Name != ""
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, renderScanOG(res, meta)); err != nil {
		logging.Errorf("scans: og image: %v", err)
		http.Error(w, "image failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(buf.Bytes())
}

// ogMeta is what a scan's preview knows beyond its reading.
type ogMeta struct {
	At      time.Time
	System  string
	Region  string
	SecText string
	Sec     float64
	HasSec  bool
}

const (
	ogW, ogH = 1200, 630
	ogMargin = 64
)

var (
	ogBGTop  = color.NRGBA{0x18, 0x12, 0x0a, 0xff}
	ogBGBot  = color.NRGBA{0x0b, 0x08, 0x05, 0xff}
	ogAccent = color.NRGBA{0xff, 0x6a, 0x1a, 0xff}
	ogTextC  = color.NRGBA{0xfa, 0xf3, 0xe6, 0xff}
	ogMuted  = color.NRGBA{0xb3, 0x9a, 0x7d, 0xff}
	ogHot    = color.NRGBA{0xff, 0x54, 0x38, 0xff}
	ogBarPal = []color.NRGBA{
		{0xff, 0x6a, 0x1a, 0xff}, {0xe0, 0x30, 0x1c, 0xff}, {0xff, 0x9a, 0x3c, 0xff},
		{0xd8, 0xa0, 0x22, 0xff}, {0xb3, 0x9a, 0x7d, 0xff}, {0x8a, 0x6f, 0x4d, 0xff},
		{0x6e, 0x5b, 0x40, 0xff}, {0x4d, 0x41, 0x32, 0xff},
	}
)

// renderScanOG draws one reading's preview card.
func renderScanOG(res scanResult, meta ogMeta) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, ogW, ogH))
	for y := 0; y < ogH; y++ {
		c := ogLerp(ogBGTop, ogBGBot, float64(y)/float64(ogH))
		for x := 0; x < ogW; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	ogFill(img, image.Rect(0, 0, ogW, 6), ogAccent)

	kind := scanKindLabels[string(res.Kind)]
	// Header: the tool and the kind on the left, the time on
	// the right.
	ogText(img, ogMargin, 48, "EVESYNAPSE", 24, ogMuted, 3)
	ogText(img, ogMargin+ogTextWidth("EVESYNAPSE", 24, 3)+18, 48, "· "+strings.ToUpper(kind), 24, ogAccent, 3)
	when := meta.At.UTC().Format("2006-01-02 15:04") + " EVE"
	ogText(img, ogW-ogMargin-ogTextWidth(when, 24, 0), 48, when, 24, ogMuted, 0)

	// Headline: the system, or the reading's own title.
	head := meta.System
	if head == "" {
		head = map[scan.Kind]string{
			scan.KindDScan: "Directional scan", scan.KindLocal: "Local scan",
			scan.KindFleet: "Fleet composition", scan.KindProbe: "Probe scan",
		}[res.Kind]
	}
	if meta.Region != "" && meta.System != "" {
		head += " · " + meta.Region
	}
	ogText(img, ogMargin, 108, ogFit(head, 58, ogW-2*ogMargin), 58, ogTextC, 0)

	// The counts line, and the security pill at its right.
	ogText(img, ogMargin, 208, ogFit(ogSummary(res), 32, ogW-2*ogMargin-190), 32, ogTextC, 0)
	if meta.HasSec {
		label := meta.SecText
		w := ogTextWidth(label, 30, 0) + 44
		rect := image.Rect(ogW-ogMargin-w, 204, ogW-ogMargin, 252)
		ogRoundFill(img, rect, 10, color.NRGBA{0x2a, 0x1d, 0x10, 0xff})
		ogText(img, rect.Min.X+22, 210, label, 30, ogSecColor(meta.Sec), 0)
	}

	ogFill(img, image.Rect(ogMargin, 292, ogW-ogMargin, 294), color.NRGBA{0xff, 0x6a, 0x1a, 0x55})

	// The middle: composition for a d-scan, the big groups
	// for a local, the shape of the rest in one line each.
	switch {
	case res.DScan != nil:
		ogDScanBody(img, res.DScan)
	case res.Local != nil:
		y := 348
		for i, g := range res.Local.Alliances {
			if i == 3 {
				break
			}
			name := g.Name
			ogText(img, ogMargin, y, ogFit(name, 30, 760), 30, ogTextC, 0)
			n := plural(g.N, "pilot")
			ogText(img, ogMargin+790, y, n, 30, ogMuted, 0)
			y += 52
		}
		if len(res.Local.Alliances) > 3 {
			ogText(img, ogMargin, y, "+"+plural(len(res.Local.Alliances)-3, "more group"), 24, ogMuted, 0)
		}
	case res.Fleet != nil:
		ogText(img, ogMargin, 348, ogFit(ogTopCounts(res.Fleet.Classes, 4), 30, ogW-2*ogMargin), 30, ogTextC, 0)
		ogText(img, ogMargin, 400, ogFit(ogTopCounts(res.Fleet.Systems, 3), 26, ogW-2*ogMargin), 26, ogMuted, 0)
	case res.Probe != nil:
		ogText(img, ogMargin, 348, ogFit(ogTopCounts(res.Probe.Groups, 4), 30, ogW-2*ogMargin), 30, ogTextC, 0)
	}

	// Footer.
	ogFill(img, image.Rect(ogMargin, ogH-96, ogW-ogMargin, ogH-94), color.NRGBA{0xff, 0x6a, 0x1a, 0x33})
	ogText(img, ogMargin, ogH-72, "evesynapse.app", 22, ogMuted, 0)
	foot := "Unlisted scan · priced and broken down by EveSynapse"
	ogText(img, ogW-ogMargin-ogTextWidth(foot, 22, 0), ogH-72, foot, 22, ogMuted, 0)
	return img
}

// ogSummary is a reading's headline counts line.
func ogSummary(res scanResult) string {
	switch {
	case res.DScan != nil:
		d := res.DScan
		return plural(d.Ships, "ship") + " · " + itoa(d.ShipsGrid) + " on grid · " + itoa(d.Ships-d.ShipsGrid) + " off grid"
	case res.Local != nil:
		return plural(len(res.Local.Pilots)+len(res.Local.Unknown), "pilot") + " · " + plural(len(res.Local.Alliances), "group")
	case res.Fleet != nil:
		return plural(len(res.Fleet.Members), "member") + " · " + pluralCount(int64(len(res.Fleet.Classes)), "ship class", "ship classes")
	case res.Probe != nil:
		s := plural(len(res.Probe.Sigs), "signature")
		if res.Probe.Unresolved > 0 {
			s += " · " + plural(res.Probe.Unresolved, "unscanned")
		}
		return s
	}
	return ""
}

// ogDScanBody draws a d-scan's composition bar with its
// legend, the top hulls, and the stored hull value.
func ogDScanBody(img *image.NRGBA, d *dscanReading) {
	if len(d.Hulls) > 0 && d.Ships > 0 {
		x := ogMargin
		barW := ogW - 2*ogMargin
		used := 0
		for i, h := range d.Hulls {
			if i == len(ogBarPal) {
				break
			}
			w := barW * h.N / d.Ships
			if i == len(d.Hulls)-1 || i == len(ogBarPal)-1 {
				w = barW - used
			}
			if w > 0 {
				ogFill(img, image.Rect(x, 336, x+w, 372), ogBarPal[i])
			}
			x += w
			used += w
		}
		var parts []string
		for i, h := range d.Hulls {
			if i == 3 {
				break
			}
			parts = append(parts, h.Name+" "+itoa(h.N))
		}
		ogText(img, ogMargin, 392, ogFit(strings.Join(parts, " · "), 24, ogW-2*ogMargin), 24, ogMuted, 0)
	}
	if len(d.Notable) > 0 {
		var parts []string
		for i, c := range d.Notable {
			if i == 3 {
				break
			}
			parts = append(parts, c.Name)
		}
		ogText(img, ogMargin, 448, ogFit("Notable: "+strings.Join(parts, " · "), 26, ogW-2*ogMargin), 26, ogHot, 0)
	}
	value := ""
	if d.Value > 0 {
		// The baked font carries no ≈; a tilde says the same
		// thing in the glyphs it has.
		value = "Hull value ~ " + formatISKShort(d.Value) + " ISK"
	}
	if len(d.Types) > 0 {
		maxW := ogW - 2*ogMargin
		if value != "" {
			maxW -= ogTextWidth(value, 28, 0) + 32
		}
		ogText(img, ogMargin, 496, ogFit("Top hulls: "+ogTopCounts(d.Types, 4), 28, maxW), 28, ogTextC, 0)
	}
	if value != "" {
		ogText(img, ogW-ogMargin-ogTextWidth(value, 28, 0), 496, value, 28, ogAccent, 0)
	}
}

// ogTopCounts words a tally list's head: "Crow x14 · Sabre x6".
func ogTopCounts(list []scanCount, n int) string {
	var parts []string
	for i, c := range list {
		if i == n {
			break
		}
		parts = append(parts, c.Name+" x"+itoa(c.N))
	}
	return strings.Join(parts, " · ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ogSecColor is a security status's display color, the
// killboard's tiers as colors.
func ogSecColor(sec float64) color.NRGBA {
	switch {
	case sec >= 0.45:
		return color.NRGBA{0x4f, 0xbf, 0x6a, 0xff}
	case sec >= 0.05:
		return color.NRGBA{0xd8, 0xa0, 0x22, 0xff}
	case sec > -0.05:
		return ogAccent
	default:
		return ogHot
	}
}

func ogLerp(a, b color.NRGBA, t float64) color.NRGBA {
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return color.NRGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}

// ogFill paints a rectangle.
func ogFill(img *image.NRGBA, r image.Rectangle, c color.NRGBA) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

// ogRoundFill paints a rectangle with rounded corners.
func ogRoundFill(img *image.NRGBA, r image.Rectangle, rad int, c color.NRGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cx, cy := x, y
			if cx < r.Min.X+rad {
				cx = r.Min.X + rad
			} else if cx >= r.Max.X-rad {
				cx = r.Max.X - rad - 1
			}
			if cy < r.Min.Y+rad {
				cy = r.Min.Y + rad
			} else if cy >= r.Max.Y-rad {
				cy = r.Max.Y - rad - 1
			}
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad*rad || (x >= r.Min.X+rad && x < r.Max.X-rad) || (y >= r.Min.Y+rad && y < r.Max.Y-rad) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

// ogGlyphFor resolves a rune to its glyph, falling back to
// the question mark and then to nothing.
func ogGlyphFor(r rune) (ogGlyph, []byte, bool) {
	if g, ok := ogGlyphs[r]; ok {
		return g, ogGlyphPix[r], true
	}
	if g, ok := ogGlyphs['?']; ok {
		return g, ogGlyphPix['?'], true
	}
	return ogGlyph{}, nil, false
}

// ogTextWidth measures text at px height without drawing.
func ogTextWidth(text string, px, tracking int) int {
	scale := float64(px) / ogFontBaked
	w := 0.0
	n := 0
	for _, r := range text {
		g, _, ok := ogGlyphFor(r)
		if !ok {
			continue
		}
		w += float64(g.adv) * scale
		n++
	}
	if n > 1 {
		w += float64(tracking * (n - 1))
	}
	return int(w + 0.5)
}

// ogFit shortens text with an ellipsis until it fits maxW.
func ogFit(text string, px, maxW int) string {
	if ogTextWidth(text, px, 0) <= maxW {
		return text
	}
	rs := []rune(text)
	for len(rs) > 0 {
		rs = rs[:len(rs)-1]
		if ogTextWidth(string(rs)+"…", px, 0) <= maxW {
			return string(rs) + "…"
		}
	}
	return ""
}

// ogText draws text at px height with its ascender line at
// y, scaled down from the baked size, and returns the x
// the pen reached.
func ogText(img *image.NRGBA, x, y int, text string, px int, col color.NRGBA, tracking int) int {
	scale := float64(px) / ogFontBaked
	pen := x
	for _, r := range text {
		g, pix, ok := ogGlyphFor(r)
		if !ok {
			continue
		}
		if pix != nil && g.w > 0 && g.h > 0 {
			dx := pen + int(float64(g.x)*scale+0.5)
			dy := y + int(float64(g.y)*scale+0.5)
			dw := int(float64(g.w)*scale + 0.5)
			dh := int(float64(g.h)*scale + 0.5)
			for j := 0; j < dh; j++ {
				sy0 := float64(j) / scale
				sy1 := float64(j+1) / scale
				for i := 0; i < dw; i++ {
					sx0 := float64(i) / scale
					sx1 := float64(i+1) / scale
					a := ogBoxAlpha(pix, g.w, g.h, sx0, sy0, sx1, sy1)
					if a > 0 {
						ogBlend(img, dx+i, dy+j, col, a)
					}
				}
			}
		}
		pen += int(float64(g.adv)*scale+0.5) + tracking
	}
	return pen
}

// ogBoxAlpha averages a glyph bitmap's alpha over a source
// rectangle (the box filter that keeps scaled-down text
// smooth).
func ogBoxAlpha(pix []byte, w, h int, x0, y0, x1, y1 float64) int {
	if x1 > float64(w) {
		x1 = float64(w)
	}
	if y1 > float64(h) {
		y1 = float64(h)
	}
	var sum, area float64
	for sy := int(y0); sy < int(y1+0.999); sy++ {
		if sy < 0 || sy >= h {
			continue
		}
		wy := ogOverlap(y0, y1, float64(sy), float64(sy+1))
		if wy <= 0 {
			continue
		}
		for sx := int(x0); sx < int(x1+0.999); sx++ {
			if sx < 0 || sx >= w {
				continue
			}
			wx := ogOverlap(x0, x1, float64(sx), float64(sx+1))
			if wx <= 0 {
				continue
			}
			idx := sy*w + sx
			v := pix[idx/2]
			if idx%2 == 0 {
				v >>= 4
			} else {
				v &= 0x0f
			}
			sum += float64(v) * 17 * wx * wy
			area += wx * wy
		}
	}
	if area == 0 {
		return 0
	}
	return int(sum/area + 0.5)
}

func ogOverlap(a0, a1, b0, b1 float64) float64 {
	lo := a0
	if b0 > lo {
		lo = b0
	}
	hi := a1
	if b1 < hi {
		hi = b1
	}
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// ogBlend paints col at alpha a (0-255) over the canvas.
func ogBlend(img *image.NRGBA, x, y int, col color.NRGBA, a int) {
	if x < 0 || y < 0 || x >= ogW || y >= ogH {
		return
	}
	af := float64(a) / 255
	dst := img.NRGBAAt(x, y)
	img.SetNRGBA(x, y, color.NRGBA{
		R: uint8(float64(col.R)*af + float64(dst.R)*(1-af) + 0.5),
		G: uint8(float64(col.G)*af + float64(dst.G)*(1-af) + 0.5),
		B: uint8(float64(col.B)*af + float64(dst.B)*(1-af) + 0.5),
		A: 0xff,
	})
}
