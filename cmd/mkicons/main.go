// Command mkicons draws every raster icon from the logo's SVG files,
// so that changing the logo is replacing those files and running this:
//
//	go run ./cmd/mkicons
//
// Three drawings live in internal/app/static and are served as they
// are:
//
//	logo.svg     the mark alone, shown in the page header
//	icon.svg     the mark on its rounded tile: the app icon
//	favicon.svg  the simplified tile that stays legible at 16 pixels
//
// From icon.svg and favicon.svg it writes, beside them:
//
//	favicon.ico            16 and 32 pixels from favicon.svg, 48 from icon.svg
//	icon-192.png           the installable app's icons, corners clear
//	icon-512.png
//	icon-maskable-512.png  the tile to every edge, the mark kept inside
//	                       the middle that a mask never crops
//	apple-touch-icon.png   180 pixels, the tile to every edge (iOS
//	                       rounds the corners itself)
//	badge-96.png           the mark alone in white on nothing: the small
//	                       monochrome picture a phone shows beside a
//	                       notification
//
// It reads the kind of SVG these files are: a 64-unit viewBox holding
// <rect>, <path> (absolute M, L, C and Z) and <circle> elements, each
// filled with a plain hex colour or a <linearGradient>. It says so and
// stops if a file uses anything else; it does not guess. It is never
// deployed.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// point is a position in the drawing's own coordinates.
type point struct{ x, y float64 }

type rgb struct{ r, g, b float64 }

// gradient is a <linearGradient> in the default units: its line runs
// between two points of the filled shape's own bounding box.
type gradient struct {
	from, to point
	offsets  []float64
	colors   []rgb
}

// at is the gradient's colour at t along its line.
func (g *gradient) at(t float64) rgb {
	if t <= g.offsets[0] {
		return g.colors[0]
	}
	for i := 1; i < len(g.offsets); i++ {
		if t <= g.offsets[i] {
			a, b := g.colors[i-1], g.colors[i]
			f := (t - g.offsets[i-1]) / (g.offsets[i] - g.offsets[i-1])
			return rgb{a.r + (b.r-a.r)*f, a.g + (b.g-a.g)*f, a.b + (b.b-a.b)*f}
		}
	}
	return g.colors[len(g.colors)-1]
}

// shape is one filled outline: closed loops of straight segments
// (curves already flattened), filled by the even-odd rule.
type shape struct {
	fill  rgb
	grad  *gradient // set: the fill is this gradient, not fill
	loops [][]point
	// tile: this is the <rect> behind the mark.
	tile bool
}

// drawing is a parsed SVG file and its viewBox.
type drawing struct {
	width, height float64
	shapes        []shape
}

// variant is one way of drawing a file.
type variant struct {
	// square draws the tile to every edge instead of with its rounded
	// corners.
	square bool
	// mark scales everything but the tile about the centre.
	mark float64
	// badge leaves the tile out and draws the mark in white.
	badge bool
}

func main() {
	dir := filepath.Join("internal", "app", "static")
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := run(dir); err != nil {
		fmt.Fprintln(os.Stderr, "mkicons:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	read := func(name string) (*drawing, error) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		d, err := parseDrawing(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return d, nil
	}
	icon, err := read("icon.svg")
	if err != nil {
		return err
	}
	favicon, err := read("favicon.svg")
	if err != nil {
		return err
	}

	plain := variant{mark: 1}
	files := map[string][]byte{}
	for _, out := range []struct {
		name string
		from *drawing
		size int
		how  variant
	}{
		{"icon-192.png", icon, 192, plain},
		{"icon-512.png", icon, 512, plain},
		// A mask may crop to a circle four fifths of the icon across.
		{"icon-maskable-512.png", icon, 512, variant{square: true, mark: 0.8}},
		{"apple-touch-icon.png", icon, 180, variant{square: true, mark: 1}},
		{"badge-96.png", favicon, 96, variant{badge: true, mark: 1.25}},
	} {
		data, err := encodePNG(out.from.render(out.size, out.how))
		if err != nil {
			return err
		}
		files[out.name] = data
	}
	var ico [][]byte
	for _, entry := range []struct {
		from *drawing
		size int
	}{{favicon, 16}, {favicon, 32}, {icon, 48}} {
		data, err := encodePNG(entry.from.render(entry.size, plain))
		if err != nil {
			return err
		}
		ico = append(ico, data)
	}
	files["favicon.ico"] = encodeICO([]int{16, 32, 48}, ico)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(dir, name), len(files[name]))
	}
	return nil
}

var (
	viewBoxRe  = regexp.MustCompile(`viewBox="\s*0[ ,]+0[ ,]+([0-9.]+)[ ,]+([0-9.]+)\s*"`)
	elementRe  = regexp.MustCompile(`(?s)<(rect|path|circle)\b([^>]*?)/>`)
	gradientRe = regexp.MustCompile(`(?s)<linearGradient\b([^>]*)>(.*?)</linearGradient>`)
	stopRe     = regexp.MustCompile(`(?s)<stop\b([^>]*?)/>`)
	otherRe    = regexp.MustCompile(`<(ellipse|polygon|polyline|line|g|use|text|image|radialGradient|mask|clipPath|filter|style)\b`)
	attrRe     = regexp.MustCompile(`([a-zA-Z0-9-]+)="([^"]*)"`)
	numberRe   = regexp.MustCompile(`[-+]?(?:[0-9]*\.[0-9]+|[0-9]+)(?:[eE][-+]?[0-9]+)?`)
	hexRe      = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)
	urlRe      = regexp.MustCompile(`^url\(#([^)]+)\)$`)
)

func attributes(raw string) map[string]string {
	attrs := map[string]string{}
	for _, a := range attrRe.FindAllStringSubmatch(raw, -1) {
		attrs[a[1]] = a[2]
	}
	return attrs
}

func hexColor(s string) (rgb, bool) {
	hex := hexRe.FindStringSubmatch(s)
	if hex == nil {
		return rgb{}, false
	}
	v, _ := strconv.ParseUint(hex[1], 16, 32)
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}, true
}

// number reads an attribute that may be absent (then it is fallback).
func number(attrs map[string]string, name string, fallback float64) (float64, error) {
	raw, has := attrs[name]
	if !has {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number", name, raw)
	}
	return v, nil
}

func parseGradients(svg string) (map[string]*gradient, error) {
	out := map[string]*gradient{}
	for _, m := range gradientRe.FindAllStringSubmatch(svg, -1) {
		attrs := attributes(m[1])
		for _, unsupported := range []string{"gradientUnits", "gradientTransform", "spreadMethod", "href"} {
			if _, has := attrs[unsupported]; has {
				return nil, fmt.Errorf("a <linearGradient> has a %s attribute, which this tool does not draw", unsupported)
			}
		}
		g := &gradient{}
		var err error
		coords := []struct {
			name     string
			into     *float64
			fallback float64
		}{{"x1", &g.from.x, 0}, {"y1", &g.from.y, 0}, {"x2", &g.to.x, 1}, {"y2", &g.to.y, 0}}
		for _, c := range coords {
			if *c.into, err = number(attrs, c.name, c.fallback); err != nil {
				return nil, fmt.Errorf("a <linearGradient>: %w", err)
			}
		}
		for _, s := range stopRe.FindAllStringSubmatch(m[2], -1) {
			stop := attributes(s[1])
			if _, has := stop["stop-opacity"]; has {
				return nil, fmt.Errorf("a gradient stop has a stop-opacity, which this tool does not draw")
			}
			offset, err := number(stop, "offset", 0)
			if err != nil {
				return nil, fmt.Errorf("a gradient stop: %w", err)
			}
			c, ok := hexColor(stop["stop-color"])
			if !ok {
				return nil, fmt.Errorf(`a gradient stop has stop-color=%q; a plain colour like "#ff6a1a" is needed`, stop["stop-color"])
			}
			g.offsets = append(g.offsets, offset)
			g.colors = append(g.colors, c)
		}
		if len(g.colors) == 0 || !sort.Float64sAreSorted(g.offsets) {
			return nil, fmt.Errorf("a <linearGradient> needs stops in rising order")
		}
		out[attrs["id"]] = g
	}
	return out, nil
}

func parseDrawing(svg string) (*drawing, error) {
	box := viewBoxRe.FindStringSubmatch(svg)
	if box == nil {
		return nil, fmt.Errorf(`no viewBox="0 0 W H"`)
	}
	d := &drawing{}
	d.width, _ = strconv.ParseFloat(box[1], 64)
	d.height, _ = strconv.ParseFloat(box[2], 64)
	if d.width <= 0 || d.width != d.height {
		return nil, fmt.Errorf("the viewBox is %vx%v; a square one is needed", d.width, d.height)
	}
	if other := otherRe.FindStringSubmatch(svg); other != nil {
		return nil, fmt.Errorf("it uses <%s>; this tool draws only <rect>, <path> and <circle> with plain or linear-gradient fills", other[1])
	}
	gradients, err := parseGradients(svg)
	if err != nil {
		return nil, err
	}
	for _, el := range elementRe.FindAllStringSubmatch(svg, -1) {
		attrs := attributes(el[2])
		for _, unsupported := range []string{"transform", "stroke", "style", "opacity", "fill-opacity"} {
			if _, has := attrs[unsupported]; has {
				return nil, fmt.Errorf("a <%s> has a %s attribute, which this tool does not draw", el[1], unsupported)
			}
		}
		sh := shape{tile: el[1] == "rect"}
		if ref := urlRe.FindStringSubmatch(attrs["fill"]); ref != nil {
			if sh.grad = gradients[ref[1]]; sh.grad == nil {
				return nil, fmt.Errorf("a <%s> is filled with %q, which is not a <linearGradient> in the file", el[1], attrs["fill"])
			}
		} else if c, ok := hexColor(attrs["fill"]); ok {
			sh.fill = c
		} else {
			return nil, fmt.Errorf(`a <%s> has fill=%q; a plain colour like "#ff6a1a" or a linear gradient is needed`, el[1], attrs["fill"])
		}
		switch el[1] {
		case "rect":
			sh.loops, err = rectLoop(attrs)
		case "path":
			sh.loops, err = parsePath(attrs["d"])
		case "circle":
			sh.loops, err = circleLoop(attrs)
		}
		if err != nil {
			return nil, err
		}
		d.shapes = append(d.shapes, sh)
	}
	if len(d.shapes) == 0 {
		return nil, fmt.Errorf("no <rect>, <path> or <circle> found")
	}
	return d, nil
}

// rectLoop is a rectangle, its corners rounded by rx.
func rectLoop(attrs map[string]string) ([][]point, error) {
	var x, y, w, h, rx float64
	var err error
	for _, a := range []struct {
		name string
		into *float64
	}{{"x", &x}, {"y", &y}, {"width", &w}, {"height", &h}, {"rx", &rx}} {
		if *a.into, err = number(attrs, a.name, 0); err != nil {
			return nil, fmt.Errorf("a <rect>: %w", err)
		}
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("a <rect> needs a width and a height")
	}
	rx = math.Min(rx, math.Min(w, h)/2)
	if rx <= 0 {
		return [][]point{{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}}}, nil
	}
	const pieces = 24
	var loop []point
	// Each corner's centre, and the angle its arc starts at.
	for _, c := range []struct{ cx, cy, start float64 }{
		{x + w - rx, y + rx, -math.Pi / 2}, {x + w - rx, y + h - rx, 0},
		{x + rx, y + h - rx, math.Pi / 2}, {x + rx, y + rx, math.Pi},
	} {
		for i := 0; i <= pieces; i++ {
			a := c.start + math.Pi/2*float64(i)/pieces
			loop = append(loop, point{c.cx + rx*math.Cos(a), c.cy + rx*math.Sin(a)})
		}
	}
	return [][]point{loop}, nil
}

// parsePath reads path data made of absolute M, L, C and Z commands
// and returns its closed loops, curves flattened to short lines.
func parsePath(d string) ([][]point, error) {
	var loops [][]point
	var cur []point
	closeLoop := func() {
		if len(cur) > 2 {
			loops = append(loops, cur)
		}
		cur = nil
	}
	rest := d
	for {
		rest = strings.TrimLeft(rest, " \t\r\n,")
		if rest == "" {
			break
		}
		cmd := rest[0]
		rest = rest[1:]
		next := strings.IndexAny(rest, "MLCZmlczHhVvSsQqTtAa")
		args := rest
		if next >= 0 {
			args, rest = rest[:next], rest[next:]
		} else {
			rest = ""
		}
		var nums []float64
		for _, n := range numberRe.FindAllString(args, -1) {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return nil, fmt.Errorf("a path has a number that cannot be read: %q", n)
			}
			nums = append(nums, f)
		}
		switch cmd {
		case 'M':
			if len(nums) < 2 || len(nums)%2 != 0 {
				return nil, fmt.Errorf("a path's M command has %d numbers", len(nums))
			}
			closeLoop()
			for i := 0; i < len(nums); i += 2 {
				cur = append(cur, point{nums[i], nums[i+1]})
			}
		case 'L':
			if len(cur) == 0 || len(nums)%2 != 0 {
				return nil, fmt.Errorf("a path's L command is malformed")
			}
			for i := 0; i < len(nums); i += 2 {
				cur = append(cur, point{nums[i], nums[i+1]})
			}
		case 'C':
			if len(cur) == 0 || len(nums) == 0 || len(nums)%6 != 0 {
				return nil, fmt.Errorf("a path's C command has %d numbers", len(nums))
			}
			for i := 0; i < len(nums); i += 6 {
				p0 := cur[len(cur)-1]
				cur = appendCubic(cur, p0, point{nums[i], nums[i+1]}, point{nums[i+2], nums[i+3]}, point{nums[i+4], nums[i+5]})
			}
		case 'Z', 'z':
			closeLoop()
		default:
			return nil, fmt.Errorf("a path uses the %q command; this tool reads only absolute M, L, C and Z", string(cmd))
		}
	}
	closeLoop()
	if len(loops) == 0 {
		return nil, fmt.Errorf("a path draws nothing")
	}
	return loops, nil
}

// appendCubic adds a cubic Bézier curve as short straight pieces.
func appendCubic(loop []point, p0, p1, p2, p3 point) []point {
	const pieces = 12
	for i := 1; i <= pieces; i++ {
		t := float64(i) / pieces
		u := 1 - t
		a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		loop = append(loop, point{a*p0.x + b*p1.x + c*p2.x + d*p3.x, a*p0.y + b*p1.y + c*p2.y + d*p3.y})
	}
	return loop
}

func circleLoop(attrs map[string]string) ([][]point, error) {
	cx, err1 := strconv.ParseFloat(attrs["cx"], 64)
	cy, err2 := strconv.ParseFloat(attrs["cy"], 64)
	r, err3 := strconv.ParseFloat(attrs["r"], 64)
	if err1 != nil || err2 != nil || err3 != nil || r <= 0 {
		return nil, fmt.Errorf("a <circle> needs cx, cy and r")
	}
	const pieces = 96
	loop := make([]point, 0, pieces)
	for i := 0; i < pieces; i++ {
		a := 2 * math.Pi * float64(i) / pieces
		loop = append(loop, point{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return [][]point{loop}, nil
}

// bounds is the box around a shape's loops.
func bounds(loops [][]point) (min, max point) {
	min = point{math.Inf(1), math.Inf(1)}
	max = point{math.Inf(-1), math.Inf(-1)}
	for _, loop := range loops {
		for _, p := range loop {
			min.x, min.y = math.Min(min.x, p.x), math.Min(min.y, p.y)
			max.x, max.y = math.Max(max.x, p.x), math.Max(max.y, p.y)
		}
	}
	return min, max
}

// render draws the file size pixels a side, on nothing: what no
// shape covers stays clear. Each pixel is sampled on a 4x4 grid, so
// edges come out smooth.
func (d *drawing) render(size int, how variant) *image.NRGBA {
	const sub = 4
	scale := float64(size) / d.width
	centre := d.width / 2
	n := size * size
	// Colour and coverage so far, colour weighted by coverage.
	acc := make([]rgb, n)
	alpha := make([]float64, n)
	weight := 1.0 / (sub * sub)

	for _, sh := range d.shapes {
		if sh.tile && how.badge {
			continue
		}
		loops := sh.loops
		switch {
		case sh.tile && how.square:
			loops = [][]point{{{0, 0}, {d.width, 0}, {d.width, d.height}, {0, d.height}}}
		case !sh.tile && how.mark != 1:
			loops = make([][]point, len(sh.loops))
			for i, loop := range sh.loops {
				for _, p := range loop {
					loops[i] = append(loops[i], point{centre + (p.x-centre)*how.mark, centre + (p.y-centre)*how.mark})
				}
			}
		}
		lo, hi := bounds(loops)
		paint := func(x, y float64) rgb {
			if how.badge {
				return rgb{255, 255, 255}
			}
			if sh.grad == nil {
				return sh.fill
			}
			// The pixel's place in the shape's box, then how far along
			// the gradient's line that is.
			px, py := (x/scale-lo.x)/(hi.x-lo.x), (y/scale-lo.y)/(hi.y-lo.y)
			g := sh.grad
			vx, vy := g.to.x-g.from.x, g.to.y-g.from.y
			return g.at(((px-g.from.x)*vx + (py-g.from.y)*vy) / (vx*vx + vy*vy))
		}

		cover := make([]float64, n)
		var xs []float64
		for row := 0; row < size*sub; row++ {
			y := (float64(row) + 0.5) / sub
			xs = xs[:0]
			for _, loop := range loops {
				for i := range loop {
					a, b := loop[i], loop[(i+1)%len(loop)]
					ay, by := a.y*scale, b.y*scale
					if (ay <= y) == (by <= y) {
						continue // the edge does not cross this row
					}
					ax, bx := a.x*scale, b.x*scale
					xs = append(xs, ax+(y-ay)*(bx-ax)/(by-ay))
				}
			}
			sort.Float64s(xs)
			// Even-odd: inside between each pair of crossings.
			for i := 0; i+1 < len(xs); i += 2 {
				from := int(math.Ceil(xs[i]*sub - 0.5))
				to := int(math.Ceil(xs[i+1]*sub-0.5)) - 1
				if from < 0 {
					from = 0
				}
				if to >= size*sub {
					to = size*sub - 1
				}
				for col := from; col <= to; col++ {
					cover[(row/sub)*size+col/sub] += weight
				}
			}
		}
		for i, c := range cover {
			if c <= 0 {
				continue
			}
			if c > 1 {
				c = 1
			}
			// This shape over what is there already.
			p := paint(float64(i%size)+0.5, float64(i/size)+0.5)
			acc[i].r = p.r*c + acc[i].r*(1-c)
			acc[i].g = p.g*c + acc[i].g*(1-c)
			acc[i].b = p.b*c + acc[i].b*(1-c)
			alpha[i] = c + alpha[i]*(1-c)
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i, a := range alpha {
		if a <= 0 {
			continue
		}
		c := acc[i]
		img.SetNRGBA(i%size, i/size, color.NRGBA{uint8(c.r/a + 0.5), uint8(c.g/a + 0.5), uint8(c.b/a + 0.5), uint8(a*255 + 0.5)})
	}
	return img
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeICO wraps PNG images as an .ico file (PNG entries, which
// every browser that asks for /favicon.ico reads).
func encodeICO(sizes []int, images [][]byte) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(images))})
	offset := 6 + 16*len(images)
	for i, data := range images {
		buf.Write([]byte{byte(sizes[i]), byte(sizes[i]), 0, 0})
		_ = binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32})
		_ = binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(data)), uint32(offset)})
		offset += len(data)
	}
	for _, data := range images {
		buf.Write(data)
	}
	return buf.Bytes()
}
