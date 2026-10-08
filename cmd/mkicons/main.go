// Command mkicons draws every icon from the one logo file, so that
// changing the logo is replacing internal/app/static/logo.svg and
// running this:
//
//	go run ./cmd/mkicons
//
// From logo.svg it writes, beside it:
//
//	favicon.svg            the logo on the app's dark rounded square
//	favicon.ico            the same at 16, 32 and 48 pixels
//	icon-192.png           the installable app's icons
//	icon-512.png
//	icon-maskable-512.png  with the wider margin a masked icon needs
//	apple-touch-icon.png   180 pixels
//
// The page header shows logo.svg itself.
//
// It reads the kind of SVG a logo export is: filled <path> elements
// whose data uses the absolute M, L, C and Z commands, and <circle>
// elements, each with a plain hex fill. It says so and stops if the
// file uses anything else; it does not guess. It is never deployed.
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

// background is the app's dark tile colour (the manifest's
// background_color and theme_color).
var background = color.RGBA{0x0d, 0x05, 0x03, 0xff}

// point is a position in the logo's own coordinates.
type point struct{ x, y float64 }

// shape is one filled outline: closed loops of straight segments
// (curves already flattened), filled by the even-odd rule.
type shape struct {
	fill  color.RGBA
	loops [][]point
}

// logo is the parsed drawing and its viewBox.
type logo struct {
	width, height float64
	shapes        []shape
	// markup is the drawing's own elements, as written, for embedding
	// in favicon.svg.
	markup string
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
	raw, err := os.ReadFile(filepath.Join(dir, "logo.svg"))
	if err != nil {
		return err
	}
	lg, err := parseLogo(string(raw))
	if err != nil {
		return fmt.Errorf("logo.svg: %w", err)
	}
	// How much of the tile's height the logo takes. A masked icon may
	// be cropped to a circle, so its logo stays inside the middle.
	const plain, masked = 0.74, 0.54

	pngs := map[string][]byte{}
	for _, out := range []struct {
		name string
		size int
		fill float64
	}{
		{"icon-192.png", 192, plain},
		{"icon-512.png", 512, plain},
		{"icon-maskable-512.png", 512, masked},
		{"apple-touch-icon.png", 180, plain},
	} {
		data, err := encodePNG(lg.render(out.size, out.fill))
		if err != nil {
			return err
		}
		pngs[out.name] = data
	}
	var ico [][]byte
	for _, size := range []int{16, 32, 48} {
		data, err := encodePNG(lg.render(size, 0.86))
		if err != nil {
			return err
		}
		ico = append(ico, data)
	}
	pngs["favicon.ico"] = encodeICO([]int{16, 32, 48}, ico)
	pngs["favicon.svg"] = []byte(lg.faviconSVG(0.74))

	names := make([]string, 0, len(pngs))
	for name := range pngs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), pngs[name], 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(dir, name), len(pngs[name]))
	}
	return nil
}

var (
	viewBoxRe = regexp.MustCompile(`viewBox="\s*0[ ,]+0[ ,]+([0-9.]+)[ ,]+([0-9.]+)\s*"`)
	elementRe = regexp.MustCompile(`(?s)<(path|circle)\b([^>]*?)/>`)
	otherRe   = regexp.MustCompile(`<(rect|ellipse|polygon|polyline|line|g|use|text|image|linearGradient|radialGradient|mask|clipPath|filter|style)\b`)
	attrRe    = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
	numberRe  = regexp.MustCompile(`[-+]?(?:[0-9]*\.[0-9]+|[0-9]+)(?:[eE][-+]?[0-9]+)?`)
	hexRe     = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)
)

func parseLogo(svg string) (*logo, error) {
	box := viewBoxRe.FindStringSubmatch(svg)
	if box == nil {
		return nil, fmt.Errorf(`no viewBox="0 0 W H"`)
	}
	lg := &logo{}
	lg.width, _ = strconv.ParseFloat(box[1], 64)
	lg.height, _ = strconv.ParseFloat(box[2], 64)
	if lg.width <= 0 || lg.height <= 0 {
		return nil, fmt.Errorf("an empty viewBox")
	}
	if other := otherRe.FindStringSubmatch(svg); other != nil {
		return nil, fmt.Errorf("it uses <%s>; this tool draws only filled <path> and <circle> elements", other[1])
	}
	var markup []string
	for _, el := range elementRe.FindAllStringSubmatch(svg, -1) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(el[2], -1) {
			attrs[a[1]] = a[2]
		}
		for _, unsupported := range []string{"transform", "stroke", "style", "opacity", "fill-opacity"} {
			if _, has := attrs[unsupported]; has {
				return nil, fmt.Errorf("a <%s> has a %s attribute, which this tool does not draw", el[1], unsupported)
			}
		}
		hex := hexRe.FindStringSubmatch(attrs["fill"])
		if hex == nil {
			return nil, fmt.Errorf(`a <%s> has fill=%q; a plain colour like "#F87A2D" is needed`, el[1], attrs["fill"])
		}
		rgb, _ := strconv.ParseUint(hex[1], 16, 32)
		sh := shape{fill: color.RGBA{uint8(rgb >> 16), uint8(rgb >> 8), uint8(rgb), 0xff}}
		var err error
		switch el[1] {
		case "path":
			sh.loops, err = parsePath(attrs["d"])
		case "circle":
			sh.loops, err = circleLoop(attrs)
		}
		if err != nil {
			return nil, err
		}
		lg.shapes = append(lg.shapes, sh)
		markup = append(markup, strings.TrimSpace(el[0]))
	}
	if len(lg.shapes) == 0 {
		return nil, fmt.Errorf("no <path> or <circle> found")
	}
	lg.markup = strings.Join(markup, "")
	return lg, nil
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

// placement is how the logo sits in a square tile: scaled so its
// height is fill of the tile's, and centred.
func (lg *logo) placement(tile, fill float64) (scale, dx, dy float64) {
	scale = tile * fill / math.Max(lg.width, lg.height)
	return scale, (tile - lg.width*scale) / 2, (tile - lg.height*scale) / 2
}

// render draws the logo on a square dark tile, size pixels a side.
// Each pixel is sampled on a 4x4 grid, so edges come out smooth.
func (lg *logo) render(size int, fill float64) *image.RGBA {
	const sub = 4
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scale, dx, dy := lg.placement(float64(size), fill)

	type rgb struct{ r, g, b float64 }
	n := size * size
	acc := make([]rgb, n)
	for i := range acc {
		acc[i] = rgb{float64(background.R), float64(background.G), float64(background.B)}
	}
	weight := 1.0 / (sub * sub)
	for _, sh := range lg.shapes {
		cover := make([]float64, n)
		var xs []float64
		for row := 0; row < size*sub; row++ {
			y := (float64(row) + 0.5) / sub
			xs = xs[:0]
			for _, loop := range sh.loops {
				for i := range loop {
					a, b := loop[i], loop[(i+1)%len(loop)]
					ay, by := a.y*scale+dy, b.y*scale+dy
					if (ay <= y) == (by <= y) {
						continue // the edge does not cross this row
					}
					ax, bx := a.x*scale+dx, b.x*scale+dx
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
			acc[i].r += (float64(sh.fill.R) - acc[i].r) * c
			acc[i].g += (float64(sh.fill.G) - acc[i].g) * c
			acc[i].b += (float64(sh.fill.B) - acc[i].b) * c
		}
	}
	for i, c := range acc {
		img.SetRGBA(i%size, i/size, color.RGBA{uint8(c.r + 0.5), uint8(c.g + 0.5), uint8(c.b + 0.5), 0xff})
	}
	return img
}

// faviconSVG is the logo on the app's rounded dark tile, as SVG.
func (lg *logo) faviconSVG(fill float64) string {
	const tile = 48
	scale, dx, dy := lg.placement(tile, fill)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d"><rect width="%d" height="%d" rx="10" fill="#0d0503"/><g transform="translate(%.4f %.4f) scale(%.6f)">%s</g></svg>`+"\n",
		tile, tile, tile, tile, dx, dy, scale, lg.markup)
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
