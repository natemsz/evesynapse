package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestLayoutRegionDeclutters: a region whose true positions pile
// systems on top of each other comes out with every pair at least
// most of the minimum spacing apart, inside the frame, and the same
// on every build.
func TestLayoutRegionDeclutters(t *testing.T) {
	var pts [][2]float64
	for i := 0; i < 60; i++ {
		// Two tight clumps and a few strays: the shape real
		// regions' flattened z axis produces.
		c := float64(i % 2 * 50)
		pts = append(pts, [2]float64{c + float64(i%5)*0.3, c + float64(i%7)*0.2})
	}
	pts = append(pts, [2]float64{300, 10}, [2]float64{-200, 260})
	var edges [][2]int
	for i := 1; i < len(pts); i++ {
		edges = append(edges, [2]int{i - 1, i})
	}
	a := layoutRegion(pts, edges)
	b := layoutRegion(pts, edges)
	minD := regionMinSpacing(len(pts))
	worst := math.Inf(1)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("layout is not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
		if a[i][0] < -1e-6 || a[i][1] < -1e-6 || a[i][0] > regionFrame+1e-6 || a[i][1] > regionFrame+1e-6 {
			t.Errorf("system %d left the frame: %v", i, a[i])
		}
		for j := i + 1; j < len(a); j++ {
			worst = math.Min(worst, math.Hypot(a[i][0]-a[j][0], a[i][1]-a[j][1]))
		}
	}
	// The final refit can shrink the frame a little; anything near
	// the spacing reads apart, a pile-up does not.
	if worst < 0.6*minD {
		t.Errorf("closest pair %.1f apart, want at least %.1f (minimum spacing %.1f)", worst, 0.6*minD, minD)
	}
}

// TestLayoutMosaicPreservesRegionShapes: the projection is a
// mosaic of the regions' own layouts -- inside a constellation,
// every pairwise distance is the region layout's distance under
// one ratio, and the ratio between two constellations of a
// region is the ratio of the normalization factors the rule
// gives them; across regions, the centroids keep their true
// bearings; the dense cores arrive already decluttered by their
// region layouts; and the picture is the same on every build.
func TestLayoutMosaicPreservesRegionShapes(t *testing.T) {
	// Three regions in the universe frame, shaped like real
	// ones: a tight core with an arm, a second core, a sparse
	// handful, plus a one-system region.
	wide := func(cx, cy float64) [][2]float64 {
		var pts [][2]float64
		for i := 0; i < 24; i++ { // the crowded core
			pts = append(pts, [2]float64{cx + float64(i%6)*4, cy + float64(i/6)*4})
		}
		for i := 0; i < 12; i++ { // the arm reaching away
			pts = append(pts, [2]float64{cx + 120 + float64(i)*70, cy + 90 + float64(i)*45})
		}
		return pts
	}
	west := wide(2400, 3800)
	east := wide(6600, 5200)
	var south [][2]float64
	for i := 0; i < 6; i++ {
		south = append(south, [2]float64{4300 + float64(i)*330, 900 + float64(i%2)*700})
	}
	solo := [][2]float64{{1500, 8200}}

	uni := append(append(append(append([][2]float64{}, west...), east...), south...), solo...)
	// Constellations: each wide region is a crowded core and an
	// arm reaching away, the sparse region is two trios, and the
	// lone system belongs to none.
	var consOf []int
	for k := 0; k < 36; k++ {
		if k < 24 {
			consOf = append(consOf, 0) // west core
		} else {
			consOf = append(consOf, 1) // west arm
		}
	}
	for k := 0; k < 36; k++ {
		if k < 24 {
			consOf = append(consOf, 2) // east core
		} else {
			consOf = append(consOf, 3) // east arm
		}
	}
	consOf = append(consOf, 4, 4, 4, 5, 5, 5, -1)
	var members [][]int
	var regionLaid [][][2]float64
	start := 0
	for _, pts := range [][][2]float64{west, east, south, solo} {
		var idxs []int
		var edges [][2]int
		for k := range pts {
			idxs = append(idxs, start+k)
			if k > 0 {
				edges = append(edges, [2]int{k - 1, k})
			}
		}
		members = append(members, idxs)
		regionLaid = append(regionLaid, layoutRegion(pts, edges))
		start += len(pts)
	}

	a := layoutMosaic(uni, members, consOf, regionLaid)
	b := layoutMosaic(uni, members, consOf, regionLaid)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mosaic is not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
		if a[i][0] < -1e-6 || a[i][1] < -1e-6 || a[i][0] > universeFrame+1e-6 || a[i][1] > universeFrame+1e-6 {
			t.Errorf("system %d left the frame: %v", i, a[i])
		}
	}

	// One ratio per constellation: every intra-constellation
	// distance in the mosaic is the region layout's distance
	// times one constant.
	ratioOf := make(map[int]float64)
	for ri, idxs := range members {
		laid := regionLaid[ri]
		groups := make(map[int][]int)
		for k, i := range idxs {
			groups[consOf[i]] = append(groups[consOf[i]], k)
		}
		for c, ks := range groups {
			if len(ks) < 2 || c < 0 {
				continue
			}
			lo, hi := math.Inf(1), math.Inf(-1)
			for x := 0; x < len(ks); x++ {
				for y := x + 1; y < len(ks); y++ {
					ld := math.Hypot(laid[ks[x]][0]-laid[ks[y]][0], laid[ks[x]][1]-laid[ks[y]][1])
					if ld < 1 {
						continue
					}
					md := math.Hypot(a[idxs[ks[x]]][0]-a[idxs[ks[y]]][0], a[idxs[ks[x]]][1]-a[idxs[ks[y]]][1])
					r := md / ld
					lo, hi = math.Min(lo, r), math.Max(hi, r)
				}
			}
			if hi > lo*1.000001 {
				t.Errorf("constellation %d: intra-constellation ratios run %.6f..%.6f, want one uniform scale", c, lo, hi)
			}
			ratioOf[c] = lo
		}
	}

	// The normalization factor the rule gives a constellation,
	// recomputed from the region layout: k is the region's
	// median per-system density, the target footprint is
	// k*sqrt(n), and the factor that takes the laid footprint
	// there is clamped to the band. Within one region every
	// constellation rides the same tile scale, so the ratio of
	// two constellations' distance ratios is the ratio of their
	// factors.
	normFactor := func(ri int, c int) float64 {
		groups := make(map[int][]int)
		for k, i := range members[ri] {
			groups[consOf[i]] = append(groups[consOf[i]], k)
		}
		rmsOf := func(ks []int) float64 {
			var cx, cy float64
			for _, k := range ks {
				cx += regionLaid[ri][k][0]
				cy += regionLaid[ri][k][1]
			}
			cx /= float64(len(ks))
			cy /= float64(len(ks))
			var ss float64
			for _, k := range ks {
				dx, dy := regionLaid[ri][k][0]-cx, regionLaid[ri][k][1]-cy
				ss += dx*dx + dy*dy
			}
			return math.Sqrt(ss / float64(len(ks)))
		}
		var dens []float64
		for _, ks := range groups {
			if len(ks) >= 2 {
				if r := rmsOf(ks); r > 0 {
					dens = append(dens, r/math.Sqrt(float64(len(ks))))
				}
			}
		}
		sort.Float64s(dens)
		kmed := dens[len(dens)/2]
		ks := groups[c]
		f := kmed * math.Sqrt(float64(len(ks))) / rmsOf(ks)
		return math.Min(math.Max(f, mosaicNormShrink), mosaicNormGrow)
	}
	for _, tc := range []struct{ ri, c, ref int }{{0, 1, 0}, {1, 3, 2}} {
		got := ratioOf[tc.c] / ratioOf[tc.ref]
		want := normFactor(tc.ri, tc.c) / normFactor(tc.ri, tc.ref)
		if math.Abs(got-want) > want*1e-6 {
			t.Errorf("constellations %d and %d: distance ratios stand %.6f apart, want the normalization factors' %.6f", tc.c, tc.ref, got, want)
		}
	}

	// The crowded cores arrive decluttered: the west core's
	// systems sat 4 units apart in truth and sit far apart in
	// the mosaic, because the region layout's spacing carried.
	worst := math.Inf(1)
	for x := 0; x < 24; x++ {
		for y := x + 1; y < 24; y++ {
			worst = math.Min(worst, math.Hypot(a[x][0]-a[y][0], a[x][1]-a[y][1]))
		}
	}
	if worst < 25 {
		t.Errorf("west core's closest pair %.1f apart in the mosaic, want the region layout's spacing (>= 25)", worst)
	}

	// Centroid bearings are the true ones: west -> east and
	// west -> south point the same way in both frames.
	centroid := func(pts [][2]float64, idxs []int) [2]float64 {
		var c [2]float64
		for _, i := range idxs {
			c[0] += pts[i][0]
			c[1] += pts[i][1]
		}
		return [2]float64{c[0] / float64(len(idxs)), c[1] / float64(len(idxs))}
	}
	bearing := func(from, to [2]float64) float64 {
		return math.Atan2(-(to[1] - from[1]), to[0]-from[0])
	}
	for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		tb := bearing(centroid(uni, members[pair[0]]), centroid(uni, members[pair[1]]))
		mb := bearing(centroid(a, members[pair[0]]), centroid(a, members[pair[1]]))
		if d := math.Abs(tb - mb); d > 0.001 {
			t.Errorf("centroid bearing %d -> %d off by %.4f rad (true %.4f, mosaic %.4f)", pair[0], pair[1], d, tb, mb)
		}
	}
}

// TestNormalizeConstellationsFootprints: the normalization
// takes each constellation to the footprint its system count
// earns -- a sparse sprawler shrinks (to the shrink bound, no
// further), a dense knot grows, the region's median-density
// constellation keeps its size -- while every constellation's
// internal shape is preserved exactly, lone systems ride
// along untouched, the region keeps its frame extent, and the
// result is the same on every build.
func TestNormalizeConstellationsFootprints(t *testing.T) {
	// One region, four constellations placed far apart, with
	// footprints known exactly: A is a 4-system ring of radius
	// 20, B a 9-system sprawl (eight on a ring of radius 90,
	// one at the centre), D a 6-system hexagon of radius 40,
	// C a lone system.
	var laid [][2]float64
	var consOf []int
	ring := func(cx, cy, r float64, n int, c int) {
		for i := 0; i < n; i++ {
			a := float64(i) * 2 * math.Pi / float64(n)
			laid = append(laid, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
			consOf = append(consOf, c)
		}
	}
	ring(200, 200, 20, 4, 0) // A: RMS 20, density 10
	ring(800, 200, 90, 8, 1) // B's ring...
	laid = append(laid, [2]float64{800, 200})
	consOf = append(consOf, 1) // ...and its centre: RMS sqrt(7200), density 28.28
	ring(800, 800, 40, 6, 2)   // D: RMS 40, density 16.33 -- the median
	laid = append(laid, [2]float64{200, 800})
	consOf = append(consOf, 3) // C: the lone system
	idxs := make([]int, len(laid))
	for i := range idxs {
		idxs[i] = i
	}

	out := normalizeConstellations(idxs, consOf, laid)
	out2 := normalizeConstellations(idxs, consOf, laid)
	for i := range out {
		if out[i] != out2[i] {
			t.Fatalf("normalization is not deterministic at %d: %v vs %v", i, out[i], out2[i])
		}
	}

	rmsOut := func(c int) float64 {
		var pts [][2]float64
		for k, i := range idxs {
			if consOf[i] == c {
				pts = append(pts, out[k])
			}
		}
		var cx, cy float64
		for _, p := range pts {
			cx += p[0]
			cy += p[1]
		}
		cx /= float64(len(pts))
		cy /= float64(len(pts))
		var ss float64
		for _, p := range pts {
			ss += (p[0]-cx)*(p[0]-cx) + (p[1]-cy)*(p[1]-cy)
		}
		return math.Sqrt(ss / float64(len(pts)))
	}
	// The refit to the frame scales every footprint by one
	// common factor, so footprints stand in the rule's ratios
	// to the median constellation D, whose factor is 1:
	// A's target is k*sqrt(4) = 32.66 against its laid 20,
	// B's target is k*sqrt(9) = 49.0 against its laid 84.85 --
	// a factor of 0.58, stopped by the shrink bound at 0.90.
	if got, want := rmsOut(0)/rmsOut(2), (16.3299*2/20)*20/40; math.Abs(got-want) > want*1e-3 {
		t.Errorf("A's footprint stands %.4f of D's, want %.4f (grown toward its target)", got, want)
	}
	if got, want := rmsOut(1)/rmsOut(2), mosaicNormShrink*math.Sqrt(7200)/40; math.Abs(got-want) > want*1e-3 {
		t.Errorf("B's footprint stands %.4f of D's, want %.4f (shrunk to the bound, no further)", got, want)
	}

	// A's internal shape is preserved exactly: every pairwise
	// distance is the laid distance times one constant.
	lo, hi := math.Inf(1), math.Inf(-1)
	for x := 0; x < 4; x++ {
		for y := x + 1; y < 4; y++ {
			ld := math.Hypot(laid[x][0]-laid[y][0], laid[x][1]-laid[y][1])
			od := math.Hypot(out[x][0]-out[y][0], out[x][1]-out[y][1])
			r := od / ld
			lo, hi = math.Min(lo, r), math.Max(hi, r)
		}
	}
	if hi > lo*1.000001 {
		t.Errorf("A's internal distance ratios run %.6f..%.6f, want one uniform scale", lo, hi)
	}

	// The region keeps its frame extent, and the lone system
	// rides the same refit as everything else: its distance to
	// D's centroid scales by the factor D's footprint reports.
	span := 0.0
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range out {
		minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	span = math.Max(maxX-minX, maxY-minY)
	if math.Abs(span-regionFrame) > 1e-6 {
		t.Errorf("region span after normalization %.4f, want the frame %.0f", span, regionFrame)
	}
	refit := rmsOut(2) / 40
	dIn := math.Hypot(laid[19][0]-800, laid[19][1]-800)
	var dc [2]float64
	for k, i := range idxs {
		if consOf[i] == 2 {
			dc[0] += out[k][0]
			dc[1] += out[k][1]
		}
	}
	dc = [2]float64{dc[0] / 6, dc[1] / 6}
	dOut := math.Hypot(out[19][0]-dc[0], out[19][1]-dc[1])
	if math.Abs(dOut/dIn-refit) > refit*1e-3 {
		t.Errorf("lone system rode a factor %.4f, want the common refit %.4f", dOut/dIn, refit)
	}
}

// TestNormalizeConstellationsSeparation: where the rescaling
// grows two constellations into each other, the separation
// pushes the bodies apart -- rigidly, each internal distance
// untouched -- until their closest systems clear the floor
// the region layout's own spacing sets, moving no body past
// the cap and never touching a body in no crowd.
func TestNormalizeConstellationsSeparation(t *testing.T) {
	// Two dense 4-system crosses (radius 15) whose centres sit
	// 52 apart, flanked by two median-density hexagons that set
	// the calibration, and two lone anchors pinning the frame's
	// corners so the refit is the identity.
	var laid [][2]float64
	var consOf []int
	cross := func(cx, cy float64, c int) {
		for _, d := range [][2]float64{{15, 0}, {-15, 0}, {0, 15}, {0, -15}} {
			laid = append(laid, [2]float64{cx + d[0], cy + d[1]})
			consOf = append(consOf, c)
		}
	}
	ring := func(cx, cy, r float64, n int, c int) {
		for i := 0; i < n; i++ {
			a := float64(i) * 2 * math.Pi / float64(n)
			laid = append(laid, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
			consOf = append(consOf, c)
		}
	}
	cross(400, 500, 0)       // A: density 7.5, grows to the grow bound
	cross(452, 500, 1)       // B: same; the pair's closest systems start 22 apart
	ring(850, 150, 40, 6, 2) // E: density 16.33, the median
	ring(150, 850, 40, 6, 3) // F: the same
	laid = append(laid, [2]float64{0, 0}, [2]float64{1000, 1000})
	consOf = append(consOf, 4, 5) // the anchors
	idxs := make([]int, len(laid))
	for i := range idxs {
		idxs[i] = i
	}

	out := normalizeConstellations(idxs, consOf, laid)
	out2 := normalizeConstellations(idxs, consOf, laid)
	for i := range out {
		if out[i] != out2[i] {
			t.Fatalf("normalization is not deterministic at %d: %v vs %v", i, out[i], out2[i])
		}
	}

	// The floor: 0.94 of the laid minimum pair distance. The
	// laid minimum is the crosses' chord, 15*sqrt(2).
	floor := mosaicSepFloor * 15 * math.Sqrt(2)
	closest := math.Inf(1)
	for x := 0; x < 4; x++ {
		for y := 4; y < 8; y++ {
			d := math.Hypot(out[x][0]-out[y][0], out[x][1]-out[y][1])
			closest = math.Min(closest, d)
		}
	}
	if closest < floor*0.98 {
		t.Errorf("A and B's closest systems sit %.2f apart after separation, want the floor %.2f", closest, floor)
	}

	// A is rigid at its grown size: every internal distance is
	// the laid distance times the grow bound.
	for x := 0; x < 4; x++ {
		for y := x + 1; y < 4; y++ {
			ld := math.Hypot(laid[x][0]-laid[y][0], laid[x][1]-laid[y][1])
			od := math.Hypot(out[x][0]-out[y][0], out[x][1]-out[y][1])
			if math.Abs(od/ld-mosaicNormGrow) > mosaicNormGrow*1e-9 {
				t.Errorf("A's pair %d-%d scaled %.6f, want the rigid grow bound %.2f", x, y, od/ld, mosaicNormGrow)
			}
		}
	}

	// A's body moved no further than the cap, and the anchors
	// -- in no crowd -- did not move at all.
	var ac [2]float64
	for x := 0; x < 4; x++ {
		ac[0] += out[x][0]
		ac[1] += out[x][1]
	}
	if d := math.Hypot(ac[0]/4-400, ac[1]/4-500); d > mosaicSepCap {
		t.Errorf("A's centre moved %.1f, past the cap %.0f", d, mosaicSepCap)
	}
	if out[20] != laid[20] || out[21] != laid[21] {
		t.Errorf("anchors moved: %v and %v, want them at %v and %v", out[20], out[21], laid[20], laid[21])
	}
}

// TestNudgeOverlapsSeparatesClosePairs: the nudge pass takes
// the pairs the mosaic's tiles leave on top of each other -- a
// border pair two units apart, a coincident pair, a chain of
// three -- and separates every one of them past the overlap
// band, moving no system further than the cap, never touching
// a system that sits in no close pair, and landing on the same
// picture every time.
func TestNudgeOverlapsSeparatesClosePairs(t *testing.T) {
	pos := [][2]float64{
		{5000, 5000}, {5002, 5000}, // the border pair, 2 apart
		{7000, 7000}, {7000, 7000}, // the coincident pair
		{2000, 2000}, {2015, 2000}, {2030, 2000}, // the chain, 15 apart
		{100, 100}, // the loner, in no close pair
	}
	a := nudgeOverlaps(pos)
	b := nudgeOverlaps(pos)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("nudge is not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
		if d := math.Hypot(a[i][0]-pos[i][0], a[i][1]-pos[i][1]); d > mosaicNudgeCap+1e-9 {
			t.Errorf("system %d moved %.1f, past the cap %.0f", i, d, mosaicNudgeCap)
		}
		for j := i + 1; j < len(a); j++ {
			if d := math.Hypot(a[j][0]-a[i][0], a[j][1]-a[i][1]); d < 20 {
				t.Errorf("systems %d and %d still %.1f apart after the nudge, want out of the overlap band (>= 20)", i, j, d)
			}
		}
	}
	if a[7] != pos[7] {
		t.Errorf("the loner moved to %v, want it untouched at %v", a[7], pos[7])
	}
}

// TestBuildUniverseMap: the payload carries every known-space
// system in both frames, the gate graph by index, each region's
// label point and frame, and drops anything outside the listed
// regions.
func TestBuildUniverseMap(t *testing.T) {
	regions := []db.SdeRegion{{RegionID: 10000001, Name: "Derelik"}, {RegionID: 10000002, Name: "The Forge"}}
	cons := []db.SdeConstellation{{ConstellationID: 20000001, Name: "Kimotoro", RegionID: 10000002}}
	systems := []db.ListMapSystemsRow{
		{SystemID: 30000142, Name: "Jita", RegionID: 10000002, ConstellationID: 20000001, Security: 0.9459, X: 1e16, Z: -2e16},
		{SystemID: 30000144, Name: "Perimeter", RegionID: 10000002, ConstellationID: 20000001, Security: 0.95, X: 1.1e16, Z: -2.05e16},
		{SystemID: 30000001, Name: "Tanoo", RegionID: 10000001, Security: 0.8584, X: -5e16, Z: 3e16},
		{SystemID: 31000001, Name: "J100001", RegionID: 11000001, Security: -0.99, X: 0, Z: 0},
	}
	jumps := []db.ListMapJumpsRow{{A: 30000142, B: 30000144}, {A: 30000001, B: 30000142}, {A: 30000142, B: 31000001}}
	p := buildUniverseMap(systems, regions, cons, jumps, nil)
	if len(p.Sys.ID) != 3 {
		t.Fatalf("systems = %v, want the three known-space ones", p.Sys.ID)
	}
	if len(p.Gates) != 4 {
		t.Errorf("gates = %v, want two pairs (the wormhole link dropped)", p.Gates)
	}
	jita, tanoo := -1, -1
	for i, id := range p.Sys.ID {
		switch id {
		case 30000142:
			jita = i
		case 30000001:
			tanoo = i
		}
		if p.Sys.X[i] < 0 || p.Sys.X[i] > universeFrame || p.Sys.Y[i] < 0 || p.Sys.Y[i] > universeFrame {
			t.Errorf("system %d outside the universe frame: %d,%d", id, p.Sys.X[i], p.Sys.Y[i])
		}
		if p.Sys.PX[i] < 0 || p.Sys.PX[i] > universeFrame || p.Sys.PY[i] < 0 || p.Sys.PY[i] > universeFrame {
			t.Errorf("system %d outside the projected frame: %d,%d", id, p.Sys.PX[i], p.Sys.PY[i])
		}
	}
	for _, rg := range p.Regions {
		if rg.PX < 0 || rg.PX > universeFrame || rg.PY < 0 || rg.PY > universeFrame {
			t.Errorf("region %s projected label point outside the frame: %d,%d", rg.Name, rg.PX, rg.PY)
		}
	}
	if p.Sys.Sec[jita] != 0.946 || p.Regions[p.Sys.Region[jita]].Name != "The Forge" || p.Cons[p.Sys.Cons[jita]].Name != "Kimotoro" {
		t.Errorf("Jita's record: sec %v region %d cons %d", p.Sys.Sec[jita], p.Sys.Region[jita], p.Sys.Cons[jita])
	}
	// North is up: Tanoo (larger z) sits above Jita on screen.
	if !(p.Sys.Y[tanoo] < p.Sys.Y[jita]) {
		t.Errorf("z is not flipped: Tanoo y %d, Jita y %d", p.Sys.Y[tanoo], p.Sys.Y[jita])
	}
	if forge := p.Regions[p.Sys.Region[jita]]; forge.W <= 0 && forge.H <= 0 {
		t.Errorf("The Forge has no region frame: %+v", forge)
	}
}

// TestBuildUniverseMapProminence: the payload carries each
// system's label prominence -- hubs above station systems above
// the rest, gate degree ordering within a class -- so the map's
// middle zoom tier names the systems that matter first.
func TestBuildUniverseMapProminence(t *testing.T) {
	if got := mapProminence(0, false, false); got != 0 {
		t.Errorf("bare isolated system = %d, want 0", got)
	}
	if got := mapProminence(3, false, false); got != 30 {
		t.Errorf("degree only = %d, want 30 (10 a gate)", got)
	}
	if got := mapProminence(0, true, false); got != 100 {
		t.Errorf("station only = %d, want 100", got)
	}
	if got := mapProminence(2, true, true); got != 1120 {
		t.Errorf("hub with a station and two gates = %d, want 1120", got)
	}

	regions := []db.SdeRegion{{RegionID: 10000002, Name: "The Forge"}}
	systems := []db.ListMapSystemsRow{
		{SystemID: 30000142, Name: "Jita", RegionID: 10000002, X: 1e16, Z: -1e16},
		{SystemID: 30001001, Name: "Station", RegionID: 10000002, X: 2e16, Z: -1e16},
		{SystemID: 30001002, Name: "Junction", RegionID: 10000002, X: 3e16, Z: -1e16},
		{SystemID: 30001003, Name: "Leaf", RegionID: 10000002, X: 4e16, Z: -1e16},
		{SystemID: 30001004, Name: "Isolated", RegionID: 10000002, X: 5e16, Z: -1e16},
	}
	jumps := []db.ListMapJumpsRow{
		{A: 30000142, B: 30001001}, {A: 30000142, B: 30001002},
		{A: 30001001, B: 30001002}, {A: 30001002, B: 30001003},
	}
	stations := map[int64]bool{30001001: true}
	p := buildUniverseMap(systems, regions, nil, jumps, stations)
	want := map[int64]int{
		30000142: 1020, // the hub, two gates
		30001001: 120,  // a station, two gates
		30001002: 30,   // three gates, no station: below the station system
		30001003: 10,   // one gate
		30001004: 0,    // no gates
	}
	if len(p.Sys.Prom) != len(p.Sys.ID) {
		t.Fatalf("prominence covers %d of %d systems", len(p.Sys.Prom), len(p.Sys.ID))
	}
	for i, id := range p.Sys.ID {
		if p.Sys.Prom[i] != want[id] {
			t.Errorf("system %d prominence = %d, want %d", id, p.Sys.Prom[i], want[id])
		}
	}
}

// TestMapPageAndPayload: with geometry imported, the payload builds,
// the page points at its content-hashed URL, and the payload serves
// gzipped (cached for good at that URL) or plain.
func TestMapPageAndPayload(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000043, 'Domain')`)
	exec(`INSERT INTO sde_constellations (constellation_id, name, region_id) VALUES (20000020, 'Kimotoro', 10000002)`)
	// Enough systems to count as imported geometry: a grid split
	// between two regions, chained by gates.
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security, constellation_id, x, y, z)
		SELECT 30000000 + g, 'S' || g, CASE WHEN g % 2 = 0 THEN 10000002 ELSE 10000043 END,
			(g % 10) / 10.0, 20000020, (g % 40) * 1e15, 0, (g / 40) * 1e15
		FROM generate_series(1, 1100) AS g`)
	exec(`INSERT INTO sde_system_jumps (from_system_id, to_system_id)
		SELECT 30000000 + g, 30000001 + g FROM generate_series(1, 1099) AS g`)

	if err := app.rebuildUniverseMap(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	meta, err := app.queries.GetMapUniverseMeta(ctx)
	if err != nil || meta.Systems != 1100 || meta.Version != universeMapVersion {
		t.Fatalf("stored payload: %+v %v", meta, err)
	}

	get := func(path string, gz bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	page := get("/map/", false)
	if page.Code != http.StatusOK {
		t.Fatalf("GET /map/ = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{`data-src="/map/universe.json?v=` + meta.Etag + `"`, `data-map-region="10000043"`, "The Forge", `class="map-swatch sec sec-10"`} {
		if !strings.Contains(body, want) {
			t.Errorf("map page missing %q", want)
		}
	}

	rec := get("/map/universe.json?v="+meta.Etag, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("payload: %d %v", rec.Code, rec.Header())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	var p universeMapPayload
	if err := json.Unmarshal(raw, &p); err != nil || len(p.Sys.ID) != 1100 || len(p.Gates) != 2*1099 {
		t.Fatalf("payload decode: %v, %d systems, %d gate ints", err, len(p.Sys.ID), len(p.Gates))
	}
	plain := get("/map/universe.json", false)
	if plain.Header().Get("Content-Encoding") != "" || !json.Valid(plain.Body.Bytes()) {
		t.Errorf("plain payload must be unencoded JSON")
	}
	if strings.Contains(plain.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("an unversioned URL must not be cached for good")
	}
}

// TestMapPageBeforeGeometry: before the SDE carries the geometry the
// page explains itself and still lists every region.
func TestMapPageBeforeGeometry(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/map/", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "data-src=") ||
		!strings.Contains(body, "has not imported its star geometry yet") || !strings.Contains(body, "The Forge") {
		t.Errorf("map page before geometry: %d", rec.Code)
	}
}
