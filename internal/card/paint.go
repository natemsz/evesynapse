package card

import (
	"image"
	"image/color"
	"math"
	"strconv"

	"golang.org/x/image/vector"
)

// shape is a filled outline in card pixels: subpaths of points,
// each closed. Holes wind against their outline (the SVG nonzero
// rule the wordmark's letterforms are drawn for).
type shape [][]pt

type pt struct{ x, y float64 }

// paint returns a fill colour (with its alpha) for a pixel centre.
type paint func(x, y float64) color.NRGBA

func solid(c color.NRGBA) paint { return func(float64, float64) color.NRGBA { return c } }

// fill rasterizes a shape with anti-aliasing and composites the
// paint through it.
func fill(dst *image.RGBA, s shape, p paint) {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, sub := range s {
		for _, q := range sub {
			minX, minY = math.Min(minX, q.x), math.Min(minY, q.y)
			maxX, maxY = math.Max(maxX, q.x), math.Max(maxY, q.y)
		}
	}
	box := image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX)), int(math.Ceil(maxY))).Intersect(dst.Bounds())
	if box.Empty() {
		return
	}
	z := vector.NewRasterizer(box.Dx(), box.Dy())
	ox, oy := float64(box.Min.X), float64(box.Min.Y)
	for _, sub := range s {
		if len(sub) < 3 {
			continue
		}
		z.MoveTo(float32(sub[0].x-ox), float32(sub[0].y-oy))
		for _, q := range sub[1:] {
			z.LineTo(float32(q.x-ox), float32(q.y-oy))
		}
		z.ClosePath()
	}
	mask := image.NewAlpha(image.Rect(0, 0, box.Dx(), box.Dy()))
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	for y := 0; y < box.Dy(); y++ {
		for x := 0; x < box.Dx(); x++ {
			a := mask.Pix[y*mask.Stride+x]
			if a == 0 {
				continue
			}
			px, py := box.Min.X+x, box.Min.Y+y
			blend(dst, px, py, p(float64(px)+.5, float64(py)+.5), float64(a)/255)
		}
	}
}

// blend paints c over an opaque canvas pixel at coverage cov.
func blend(dst *image.RGBA, x, y int, c color.NRGBA, cov float64) {
	a := cov * float64(c.A) / 255
	if a <= 0 {
		return
	}
	i := dst.PixOffset(x, y)
	px := dst.Pix[i : i+3 : i+3]
	px[0] = uint8(float64(c.R)*a + float64(px[0])*(1-a) + .5)
	px[1] = uint8(float64(c.G)*a + float64(px[1])*(1-a) + .5)
	px[2] = uint8(float64(c.B)*a + float64(px[2])*(1-a) + .5)
}

// rect is an axis-aligned rectangle shape.
func rect(x0, y0, x1, y1 float64) shape {
	return shape{{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}}
}

// roundRect is a rectangle with circular corners of radius r.
func roundRect(x0, y0, x1, y1, r float64) shape {
	var sub []pt
	corner := func(cx, cy, from float64) {
		for i := 0; i <= 8; i++ {
			a := from + float64(i)*math.Pi/16
			sub = append(sub, pt{cx + r*math.Cos(a), cy + r*math.Sin(a)})
		}
	}
	corner(x1-r, y0+r, -math.Pi/2)
	corner(x1-r, y1-r, 0)
	corner(x0+r, y1-r, math.Pi/2)
	corner(x0+r, y0+r, math.Pi)
	return shape{sub}
}

// circle is a circle as a 48-gon, finer than a pixel at card sizes.
func circle(cx, cy, r float64) shape {
	sub := make([]pt, 48)
	for i := range sub {
		a := float64(i) * 2 * math.Pi / 48
		sub[i] = pt{cx + r*math.Cos(a), cy + r*math.Sin(a)}
	}
	return shape{sub}
}

// linear is a two-stop linear gradient from p0 to p1.
func linear(x0, y0, x1, y1 float64, c0, c1 color.NRGBA) paint {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	return func(x, y float64) color.NRGBA {
		t := ((x-x0)*dx + (y-y0)*dy) / l2
		return mix(c0, c1, t)
	}
}

// mix interpolates two colours (alpha included) at t, clamped.
func mix(a, b color.NRGBA, t float64) color.NRGBA {
	t = math.Max(0, math.Min(1, t))
	m := func(p, q uint8) uint8 { return uint8(float64(p) + (float64(q)-float64(p))*t + .5) }
	return color.NRGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), m(a.A, b.A)}
}

// svgPath reads the absolute M, L, H, V and Z commands an outlined
// SVG path uses (the wordmark's letterforms and the logo mark carry
// nothing else) into subpaths, mapped through scale and offset.
func svgPath(d string, scale, ox, oy float64) shape {
	var s shape
	var sub []pt
	var cur pt
	cmd := byte(0)
	i := 0
	num := func() float64 {
		for i < len(d) && (d[i] == ' ' || d[i] == ',') {
			i++
		}
		j := i
		if j < len(d) && (d[j] == '-' || d[j] == '+') {
			j++
		}
		for j < len(d) && (d[j] == '.' || (d[j] >= '0' && d[j] <= '9')) {
			j++
		}
		v, _ := strconv.ParseFloat(d[i:j], 64)
		i = j
		return v
	}
	flush := func() {
		if len(sub) > 0 {
			s = append(s, sub)
		}
		sub = nil
	}
	put := func(p pt) {
		cur = p
		sub = append(sub, pt{ox + p.x*scale, oy + p.y*scale})
	}
	for i < len(d) {
		c := d[i]
		switch {
		case c == ' ' || c == ',':
			i++
			continue
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
			cmd = c
			i++
			if c == 'Z' || c == 'z' {
				flush()
			}
			continue
		}
		switch cmd {
		case 'M':
			flush()
			x := num()
			put(pt{x, num()})
			cmd = 'L'
		case 'L':
			x := num()
			put(pt{x, num()})
		case 'H':
			put(pt{num(), cur.y})
		case 'V':
			put(pt{cur.x, num()})
		default:
			i++ // a command this reader does not draw
		}
	}
	flush()
	return s
}
