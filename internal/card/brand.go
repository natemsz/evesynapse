package card

import (
	"image"
	"image/color"
	"math"
)

// The brand drawings, copied verbatim from templates/wordmark.html
// (wordmark-svg and logo-mark); the app's TestCardBrandMatchesWordmark
// fails if the two drift apart. The brand name is never set in a
// font: these outlines are the wordmark.

// WordmarkPaths are the wordmark's letterforms, E V E S Y N A P S E,
// in the template's viewBox (0 -920 7095 1200).
var WordmarkPaths = []string{
	"M150 11V-75H61V-548H150V-636H446V-548H536V-424H417V-511H179V-374H358V-251H179V-113H417V-200H536V-75H446V11Z",
	"M960 11V-75H872V-251H783V-636H901V-288H990V-113H1050V-288H1139V-636H1258V-251H1168V-75H1080V11Z",
	"M1594 11V-75H1505V-548H1594V-636H1890V-548H1980V-424H1861V-511H1623V-374H1802V-251H1623V-113H1861V-200H1980V-75H1890V11Z",
	"M2316 11V-75H2227V-200H2345V-113H2583V-251H2316V-337H2227V-548H2316V-636H2612V-548H2702V-424H2583V-511H2345V-374H2612V-288H2702V-75H2612V11Z",
	"M3126 11V-337H3038V-424H2949V-636H3067V-461H3156V-374H3216V-461H3305V-636H3424V-424H3334V-337H3246V11Z",
	"M3671 11V-636H3789V-548H3877V-461H3968V-200H4027V-636H4146V11H4027V-75H3939V-163H3848V-424H3789V11Z",
	"M4393 11V-461H4482V-548H4570V-636H4690V-548H4778V-461H4868V11H4749V-163H4511V11ZM4511 -288H4749V-424H4660V-511H4600V-424H4511Z",
	"M5115 11V-548H5204V-636H5500V-548H5590V-251H5500V-163H5233V11ZM5233 -288H5471V-511H5233Z",
	"M5926 11V-75H5837V-200H5955V-113H6193V-251H5926V-337H5837V-548H5926V-636H6222V-548H6312V-424H6193V-511H5955V-374H6222V-288H6312V-75H6222V11Z",
	"M6648 11V-75H6559V-548H6648V-636H6944V-548H7034V-424H6915V-511H6677V-374H6856V-251H6677V-113H6915V-200H7034V-75H6944V11Z",
}

// wordmarkColors: EVE solid white, SYNAPSE one solid colour per
// letter stepping gold to ember.
var wordmarkColors = []color.NRGBA{
	hex(0xffffff), hex(0xffffff), hex(0xffffff),
	hex(0xffc36c), hex(0xffa551), hex(0xff8835), hex(0xff6a1a), hex(0xf35d18), hex(0xe85017), hex(0xdc4315),
}

// The wordmark's ink box in its own units: x 61 to 7034, cap top
// -636 to baseline 11.
const (
	wordmarkX0, wordmarkX1   = 61.0, 7034.0
	wordmarkTop, wordmarkBot = -636.0, 11.0
)

// drawWordmark sets the wordmark with its ink box's left edge at x
// and its cap height h, vertically centred on cy. It returns the
// drawn width.
func drawWordmark(dst *image.RGBA, x, cy, h float64) float64 {
	scale := h / (wordmarkBot - wordmarkTop)
	ox := x - wordmarkX0*scale
	oy := cy - h/2 - wordmarkTop*scale
	for i, d := range WordmarkPaths {
		fill(dst, svgPath(d, scale, ox, oy), solid(wordmarkColors[i]))
	}
	return (wordmarkX1 - wordmarkX0) * scale
}

// MarkPaths are the logo mark's four facets in its 64x64 viewBox:
// the upper blade, its highlight, the lower blade, its shade.
var MarkPaths = []string{
	"M44.8 1.85 L11.8 34.85 L27.8 34.85 Z",
	"M44.8 1.85 L27.8 34.85 L21.8 34.85 Z",
	"M36.2 29.15 L52.2 29.15 L19.2 62.15 Z",
	"M36.2 29.15 L42.2 29.15 L19.2 62.15 Z",
}

// drawMark draws the logo mark in a size x size box at (x, y), with
// the template's gradients: each blade's linear gradient runs
// (0.8, 0) to (0.2, 1) of its own bounding box, and the core is a
// radial glow under a solid spark.
func drawMark(dst *image.RGBA, x, y, size float64) {
	s := size / 64
	blade := func(d string, c0, c1 color.NRGBA) {
		sh := svgPath(d, s, x, y)
		fill(dst, sh, bboxLinear(sh, c0, c1))
	}
	blade(MarkPaths[0], hex(0xffe2a0), hex(0xff7a26))
	fill(dst, svgPath(MarkPaths[1], s, x, y), solid(alpha(hex(0xfff3d6), .55)))
	blade(MarkPaths[2], hex(0xff6a1a), hex(0xc8360f))
	fill(dst, svgPath(MarkPaths[3], s, x, y), solid(alpha(hex(0x000000), .22)))
	cx, cy := x+32*s, y+32*s
	glow, glowEdge := alpha(hex(0xffd27a), .85), alpha(hex(0xffd27a), 0)
	r := 4.5 * s
	fill(dst, circle(cx, cy, r), func(px, py float64) color.NRGBA {
		return mix(glow, glowEdge, math.Hypot(px-cx, py-cy)/r)
	})
	fill(dst, circle(cx, cy, 1.8*s), solid(hex(0xfff6e6)))
}

// bboxLinear is SVG's objectBoundingBox gradient from (0.8, 0) to
// (0.2, 1): the stops are placed in the shape's normalised box, so
// the gradient's lines run where the browser draws them.
func bboxLinear(sh shape, c0, c1 color.NRGBA) paint {
	x0, y0, x1, y1 := sh[0][0].x, sh[0][0].y, sh[0][0].x, sh[0][0].y
	for _, sub := range sh {
		for _, q := range sub {
			x0, y0 = min(x0, q.x), min(y0, q.y)
			x1, y1 = max(x1, q.x), max(y1, q.y)
		}
	}
	w, h := x1-x0, y1-y0
	return func(px, py float64) color.NRGBA {
		u, v := (px-x0)/w, (py-y0)/h
		// Project (u, v) onto (0.8, 0) -> (0.2, 1) in box space.
		t := ((u-.8)*-.6 + v*1) / (.36 + 1)
		return mix(c0, c1, t)
	}
}
