// Terminal art: the wordmark and the rotating bolt, drawn from the
// same outlines the site uses. Both are rasterized with solid
// quadrant blocks, so strokes have no dot gaps. A quadrant pixel is
// half a cell wide and half a cell tall, which on a typical 1:2 cell
// makes it twice as tall as it is wide; the scales below account
// for that.
package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// quadrants maps a 4-bit cover mask (1 top left, 2 top right,
// 4 bottom left, 8 bottom right) to its block character.
var quadrants = []rune(" ▘▝▀▖▌▞▛▗▚▐▜▄▙▟█")

type point struct{ x, y float64 }

type rgb struct{ r, g, b float64 }

func hexRGB(s string) rgb {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return rgb{float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255)}
}

func (c rgb) mix(o rgb, t float64) rgb {
	return rgb{c.r + (o.r-c.r)*t, c.g + (o.g-c.g)*t, c.b + (o.b-c.b)*t}
}

func (c rgb) scale(k float64) rgb {
	return rgb{math.Min(255, c.r*k), math.Min(255, c.g*k), math.Min(255, c.b*k)}
}

// hex rounds to a coarse step, so neighboring cells share a color
// and the frame carries fewer escape codes.
func (c rgb) hex() string {
	q := func(v float64) int {
		n := int(math.Round(v/8)) * 8
		if n > 255 {
			n = 255
		}
		return n
	}
	return fmt.Sprintf("#%02x%02x%02x", q(c.r), q(c.g), q(c.b))
}

// parseOutline reads an SVG path made of M, L and Z commands into
// its closed subpaths.
func parseOutline(d string) [][]point {
	var out [][]point
	var cur []point
	for _, tok := range strings.FieldsFunc(d, func(r rune) bool { return r == 'L' }) {
		tok = strings.TrimSpace(tok)
		for strings.Contains(tok, "M") || strings.Contains(tok, "Z") {
			if i := strings.IndexAny(tok, "MZ"); tok[i] == 'Z' {
				if head := strings.TrimSpace(tok[:i]); head != "" {
					cur = append(cur, parsePoint(head))
				}
				out = append(out, cur)
				cur = nil
				tok = tok[i+1:]
			} else {
				tok = tok[i+1:]
			}
		}
		if tok = strings.TrimSpace(tok); tok != "" {
			cur = append(cur, parsePoint(tok))
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func parsePoint(s string) point {
	f := strings.Fields(s)
	if len(f) != 2 {
		return point{}
	}
	x, _ := strconv.ParseFloat(f[0], 64)
	y, _ := strconv.ParseFloat(f[1], 64)
	return point{x, y}
}

// insideOutline applies the even-odd rule, which is how the letter
// counters and the signal nodes read as cut-outs or separate shapes.
func insideOutline(subpaths [][]point, x, y float64) bool {
	in := false
	for _, poly := range subpaths {
		for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
			a, b := poly[i], poly[j]
			if (a.y > y) != (b.y > y) && x < (b.x-a.x)*(y-a.y)/(b.y-a.y)+a.x {
				in = !in
			}
		}
	}
	return in
}

func outlineBounds(subpaths [][]point) (minX, minY, maxX, maxY float64) {
	minX, minY = math.Inf(1), math.Inf(1)
	maxX, maxY = math.Inf(-1), math.Inf(-1)
	for _, poly := range subpaths {
		for _, p := range poly {
			minX, maxX = math.Min(minX, p.x), math.Max(maxX, p.x)
			minY, maxY = math.Min(minY, p.y), math.Max(maxY, p.y)
		}
	}
	return
}

// canvas is a grid of quadrant pixels, two per cell each way.
type canvas struct {
	cols, rows int
	on         []bool
	color      []rgb
}

func newCanvas(cols, rows int) *canvas {
	n := cols * rows * 4
	return &canvas{cols: cols, rows: rows, on: make([]bool, n), color: make([]rgb, n)}
}

func (c *canvas) set(x, y int, col rgb) {
	if x < 0 || y < 0 || x >= c.cols*2 || y >= c.rows*2 {
		return
	}
	i := y*c.cols*2 + x
	c.on[i], c.color[i] = true, col
}

// lines turns the canvas into styled text. Empty pixels stay
// unpainted, so the art sits on whatever background the terminal
// has. Runs of one color share one escape sequence.
func (c *canvas) lines() []string {
	out := make([]string, c.rows)
	for row := 0; row < c.rows; row++ {
		var b strings.Builder
		var run strings.Builder
		runHex := ""
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runHex == "" {
				b.WriteString(run.String())
			} else {
				b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(runHex)).Render(run.String()))
			}
			run.Reset()
		}
		for col := 0; col < c.cols; col++ {
			mask, n := 0, 0.0
			var sum rgb
			for bit, off := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				i := (row*2+off[1])*c.cols*2 + col*2 + off[0]
				if c.on[i] {
					mask |= 1 << bit
					sum.r += c.color[i].r
					sum.g += c.color[i].g
					sum.b += c.color[i].b
					n++
				}
			}
			hex := ""
			if mask != 0 {
				hex = rgb{sum.r / n, sum.g / n, sum.b / n}.hex()
			}
			if hex != runHex && !(mask == 0 && run.Len() > 0 && runHex == "") {
				flush()
				runHex = hex
			}
			run.WriteRune(quadrants[mask])
		}
		flush()
		out[row] = b.String()
	}
	return out
}

// ---------------------------------------------------------------
// Wordmark
// ---------------------------------------------------------------

// wordmarkColors are the default scheme's per-letter fills from
// style.css: white EVE, then the ember ramp across SYNAPSE.
var wordmarkColors = []string{
	"#ffffff", "#ffffff", "#ffffff",
	"#ffc36c", "#ffa551", "#ff8835", "#ff6a1a", "#f35d18", "#e85017", "#dc4315",
}

// wordmarkSize is one hinted size: its stroke in pixels fixes the
// width, and the row count is the nearest that keeps the bars even.
type wordmarkSize struct{ stroke, cols, rows int }

// wordmarkSizes run smallest to largest. The stroke of the real
// drawing is 104 units of 5745 wide and 647 tall.
var wordmarkSizes = []wordmarkSize{
	{stroke: 2, cols: 56, rows: 3},
	{stroke: 3, cols: 84, rows: 5},
	{stroke: 4, cols: 112, rows: 6},
}

// unhintY maps a pixel row back to a drawing y. The mapping puts the
// three horizontal bars of the E on whole rows at equal thickness,
// the way a font hints its stems; everything between is linear.
func unhintY(v float64, px int) float64 {
	bar := math.Max(1, math.Round(104*float64(px)/647))
	midTop := math.Round((float64(px) - bar) / 2)
	from := []float64{-636, -532, -364, -261, -93, 11}
	to := []float64{0, bar, midTop, midTop + bar, float64(px) - bar, float64(px)}
	for i := 1; i < len(to); i++ {
		if v <= to[i] {
			if to[i] == to[i-1] {
				return from[i]
			}
			t := (v - to[i-1]) / (to[i] - to[i-1])
			return from[i-1] + (from[i]-from[i-1])*t
		}
	}
	return from[len(from)-1]
}

// renderWordmark rasterizes the real letter outlines at one size.
// Each letter's box snaps to whole pixels, so its stems stay crisp
// and every letter keeps the drawing's spacing to within a pixel.
func renderWordmark(sz wordmarkSize) []string {
	c := newCanvas(sz.cols, sz.rows)
	const inkLeft, inkRight = 61.0, 5806.0
	pxW, pxH := sz.cols*2, sz.rows*2
	scale := float64(pxW) / (inkRight - inkLeft)
	const ss = 6
	for li, d := range wordmarkPaths {
		sub := parseOutline(d)
		minX, _, maxX, _ := outlineBounds(sub)
		x0 := math.Round((minX - inkLeft) * scale)
		x1 := math.Round((maxX - inkLeft) * scale)
		if x1 <= x0 {
			x1 = x0 + 1
		}
		col := hexRGB(wordmarkColors[li])
		for py := 0; py < pxH; py++ {
			for px := int(x0); px < int(x1); px++ {
				hits := 0
				for sy := 0; sy < ss; sy++ {
					y := unhintY(float64(py)+(float64(sy)+.5)/ss, pxH)
					for sx := 0; sx < ss; sx++ {
						t := (float64(px) + (float64(sx)+.5)/ss - x0) / (x1 - x0)
						if insideOutline(sub, minX+(maxX-minX)*t, y) {
							hits++
						}
					}
				}
				if hits*2 >= ss*ss {
					c.set(px, py, col)
				}
			}
		}
	}
	return c.lines()
}

// ---------------------------------------------------------------
// Bolt
// ---------------------------------------------------------------

// The bolt from internal/app/static/logo.svg, in its 64-unit box.
var (
	boltUpper     = []point{{44.8, 1.85}, {11.8, 34.85}, {27.8, 34.85}}
	boltUpperLit  = []point{{44.8, 1.85}, {27.8, 34.85}, {21.8, 34.85}}
	boltLower     = []point{{36.2, 29.15}, {52.2, 29.15}, {19.2, 62.15}}
	boltLowerDark = []point{{36.2, 29.15}, {42.2, 29.15}, {19.2, 62.15}}
)

const (
	boltFrameCount = 36
	boltHalfWidth  = 20.2  // widest reach from the spin axis at x=32
	boltHalfHeight = 30.15 // half the drawing's height, about y=32
	boltDepth      = 5.0   // thickness, so the edge-on frame stays solid
)

// Logo gradient stops.
var (
	boltUpA = hexRGB("#ffe2a0")
	boltUpB = hexRGB("#ff7a26")
	boltDnA = hexRGB("#ff6a1a")
	boltDnB = hexRGB("#c8360f")
	boltLit = hexRGB("#fff3d6")
	boltHot = hexRGB("#fff6e6")
)

func insideTri(t []point, x, y float64) bool {
	return insideOutline([][]point{t}, x, y)
}

// boltColor is the logo's flat color at a point on its face, or
// false off the bolt.
func boltColor(x, y float64) (rgb, bool) {
	// The gradients run along (-.6, 1) across the 64-unit box.
	g := math.Max(0, math.Min(1, ((64*.8-x)*.6+y)/(64*1.2)))
	switch {
	case insideTri(boltUpper, x, y):
		c := boltUpA.mix(boltUpB, g)
		if insideTri(boltUpperLit, x, y) {
			c = c.mix(boltLit, .55)
		}
		return c, true
	case insideTri(boltLower, x, y):
		c := boltDnA.mix(boltDnB, g)
		if insideTri(boltLowerDark, x, y) {
			c = c.scale(.78)
		}
		return c, true
	}
	return rgb{}, false
}

// boltCols is the fixed width of a bolt that is rows tall. It is the
// same for every frame, and the spin axis is its exact center, so
// the bolt turns in place without drifting.
func boltCols(rows int) int {
	k := float64(rows*2) / (boltHalfHeight * 2)
	cols := int(math.Ceil((boltHalfWidth + boltDepth/2) * 2 * k))
	if cols%2 != 0 {
		cols++
	}
	return cols
}

// renderBolt draws one frame of the bolt turning about its vertical
// axis, as a solid slab with lit faces and darker side walls.
func renderBolt(rows, frame int) []string {
	cols := boltCols(rows)
	c := newCanvas(cols, rows)
	theta := 2 * math.Pi * float64(frame) / boltFrameCount
	sin, cos := math.Sincos(theta)
	pxH := rows * 2
	pxW := cols * 2
	kY := float64(pxH) / (boltHalfHeight * 2) // pixels per unit, down
	kX := kY * 2                              // quadrant pixels are half as wide
	const ss = 3
	const step = 1.0 / ss

	type sample struct {
		z   float64
		col rgb
		n   int
	}
	buf := make([]sample, pxW*pxH*ss*ss)
	for i := range buf {
		buf[i].z = math.Inf(-1)
	}
	put := func(wx, wy, wz float64, col rgb) {
		// Rotate about the vertical axis through the center.
		rx := wx*cos + wz*sin
		rz := -wx*sin + wz*cos
		sx := int(math.Floor((rx*kX + float64(pxW)/2) * ss))
		sy := int(math.Floor((wy*kY + float64(pxH)/2) * ss))
		if sx < 0 || sy < 0 || sx >= pxW*ss || sy >= pxH*ss {
			return
		}
		i := sy*pxW*ss + sx
		if rz > buf[i].z {
			buf[i].z, buf[i].col = rz, col
			buf[i].n = 1
		}
	}

	// Sample the slab finely enough to leave no holes at any angle.
	du := step / (kX + 1e-9) * .7
	dv := step / kY * .7
	faceLight := .84 + .16*math.Abs(cos)
	wallLight := .62 + .38*math.Abs(sin)
	for wy := -boltHalfHeight; wy <= boltHalfHeight; wy += dv {
		for wx := -boltHalfWidth; wx <= boltHalfWidth; wx += du {
			col, ok := boltColor(wx+32, wy+32)
			if !ok {
				continue
			}
			put(wx, wy, boltDepth/2, col.scale(faceLight))
			put(wx, wy, -boltDepth/2, col.scale(faceLight*.92))
			// A point near the outline carries the side wall.
			edge := false
			for _, o := range [][2]float64{{du * 2, 0}, {-du * 2, 0}, {0, dv * 2}, {0, -dv * 2}} {
				if _, in := boltColor(wx+32+o[0], wy+32+o[1]); !in {
					edge = true
					break
				}
			}
			if edge {
				for wz := -boltDepth / 2; wz <= boltDepth/2; wz += du {
					put(wx, wy, wz, boltDnA.mix(col, .45).scale(wallLight))
				}
			}
		}
	}

	// Resolve to quadrant pixels: a pixel is on when at least a
	// third of its samples hit, which keeps the bolt's thin tips,
	// and takes their average color.
	for py := 0; py < pxH; py++ {
		for px := 0; px < pxW; px++ {
			var sum rgb
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					s := buf[(py*ss+sy)*pxW*ss+px*ss+sx]
					if s.n > 0 {
						sum.r += s.col.r
						sum.g += s.col.g
						sum.b += s.col.b
						hits++
					}
				}
			}
			if hits*3 >= ss*ss {
				n := float64(hits)
				c.set(px, py, rgb{sum.r / n, sum.g / n, sum.b / n})
			}
		}
	}

	// The synapse node sits on the spin axis, so it holds still
	// while the bolt turns around it: a hot core in a warm halo,
	// centered on the middle four pixels.
	cx, cy := pxW/2, pxH/2
	halo := boltUpA.mix(boltHot, .35)
	for _, o := range [][2]int{{-2, -1}, {1, -1}, {-2, 0}, {1, 0}, {-1, -2}, {0, -2}, {-1, 1}, {0, 1}} {
		if rows >= 12 {
			c.set(cx+o[0], cy+o[1], halo)
		}
	}
	for _, o := range [][2]int{{-1, -1}, {0, -1}, {-1, 0}, {0, 0}} {
		c.set(cx+o[0], cy+o[1], boltHot)
	}
	return c.lines()
}

// ---------------------------------------------------------------
// Cache
// ---------------------------------------------------------------

// artCache keeps rendered art, so a frame is drawn once per size.
var artCache = struct {
	sync.Mutex
	bolt map[[2]int][]string
	word map[int][]string
}{bolt: map[[2]int][]string{}, word: map[int][]string{}}

func cachedBolt(rows, frame int) []string {
	artCache.Lock()
	defer artCache.Unlock()
	k := [2]int{rows, frame}
	if v, ok := artCache.bolt[k]; ok {
		return v
	}
	v := renderBolt(rows, frame)
	artCache.bolt[k] = v
	return v
}

func cachedWordmark(sz wordmarkSize) []string {
	artCache.Lock()
	defer artCache.Unlock()
	if v, ok := artCache.word[sz.cols]; ok {
		return v
	}
	v := renderWordmark(sz)
	artCache.word[sz.cols] = v
	return v
}
