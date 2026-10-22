package app

// ---------------------------------------------------------------------------
// The universe map (schema 072): the payload the map page draws,
// built from the SDE's geometry once per import and stored, never
// computed at render.
//
// Three layouts ride in one payload. The universe view is the true
// top-down picture: every known-space system at its SDE x and z
// (z flipped so galactic north is up), scaled into a 0..10000 frame.
// The region view is the tactical one, computed rather than
// hand-drawn: each region's systems start from the same true
// positions, fitted to their own 0..1000 frame, then pushed apart
// until no two crowd each other (a fixed-step relaxation anchored to
// the true positions, deterministic, so a layout never shifts
// between builds of the same dump). Where a system sits relative to
// its neighbours stays the true picture; only overlaps give way.
// Beside them sits the Improved Projection: a mosaic of the
// region layouts (layoutMosaic) -- each region keeps its region
// view's shape, set down at its true size on its true centroid --
// with each constellation's footprint first normalized toward
// the size its system count earns (normalizeConstellations), so
// a constellation reads as its own glyph at a weight comparable
// to its neighbours', and a bounded nudge pass (nudgeOverlaps)
// that separates the pairs the tiles leave on top of each other
// across region borders. For the reader who wants the tactical
// reading at universe scale: one shape at two zooms.
//
// Known space only: wormhole space has no stargates to draw, and the
// abyssal and void pockets are not places on a map.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// universeMapVersion is the builder's version: bump it when the
// payload shape or a layout changes, and the stored payload
// rebuilds from the stored SDE without a re-import.
const universeMapVersion = 4

const (
	universeFrame = 10000.0 // the universe view's longest side
	regionFrame   = 1000.0  // a region view's longest side
	// universeMapMinSystems: below this the SDE geometry has not
	// landed yet (an import predating schema 072), and there is no
	// map to build.
	universeMapMinSystems = 1000
)

// universeMapPayload is the map page's whole world, as JSON. The
// system table is columnar (one array per field, index = system),
// which keeps ~5,400 systems compact; gates and region membership
// refer to systems and regions by index. Mirrored as a JSDoc
// typedef in static/map.js; change both together.
type universeMapPayload struct {
	Version int                 `json:"v"`
	Regions []universeMapRegion `json:"regions"`
	Cons    []universeMapCons   `json:"cons"`
	Sys     universeMapSystems  `json:"sys"`
	// Gates is the stargate graph: index pairs, flattened.
	Gates []int `json:"gates"`
}

type universeMapRegion struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// X, Y: the region's label point in the universe frame (its
	// systems' centroid). PX, PY: the same in the projected
	// frame. W, H: its region-view frame.
	X  int `json:"x"`
	Y  int `json:"y"`
	PX int `json:"px"`
	PY int `json:"py"`
	W  int `json:"w"`
	H  int `json:"h"`
}

type universeMapCons struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Region int    `json:"r"`
}

type universeMapSystems struct {
	ID     []int64   `json:"id"`
	Name   []string  `json:"n"`
	X      []int     `json:"x"`  // universe frame
	Y      []int     `json:"y"`  // universe frame
	PX     []int     `json:"px"` // projected frame (Improved Projection)
	PY     []int     `json:"py"` // projected frame
	Sec    []float64 `json:"s"`  // true security, 3 decimals
	Region []int     `json:"r"`  // index into regions
	Cons   []int     `json:"c"`  // index into cons (-1: unknown)
	RX     []int     `json:"rx"` // region frame
	RY     []int     `json:"ry"` // region frame
	// Faction is the owning faction's ID (0: none): the
	// Faction Warfare and sovereignty layers key on it.
	Faction []int64 `json:"f"`
	// Prom is the system's label prominence (mapProminence):
	// at the map's middle zoom tier only the most prominent
	// systems in view are named, best first.
	Prom []int `json:"p"`
}

// Label prominence: a system's claim on being named when the
// map cannot name everyone. Trade hubs outrank everything, a
// station outranks any number of gates, and each gate counts --
// so the order reads hubs, then station systems by how joined
// up they are, then the rest by the same measure.
const (
	promHub     = 1000
	promStation = 100
	promPerGate = 10
)

// tradeHubSystems are the systems of the five market hubs (the
// stations in foresight.go's tradeHubs).
var tradeHubSystems = map[int64]bool{
	30000142: true, // Jita
	30002187: true, // Amarr
	30002659: true, // Dodixie
	30002510: true, // Rens
	30002053: true, // Hek
}

// mapProminence scores one system for label precedence from its
// gate degree, whether it holds a station, and whether it is a
// trade hub.
func mapProminence(degree int, station, hub bool) int {
	p := degree * promPerGate
	if station {
		p += promStation
	}
	if hub {
		p += promHub
	}
	return p
}

// buildUniverseMap lays out the payload from the SDE rows.
// stationSystems holds the IDs of systems with at least one NPC
// station (ListMapStationSystems); it feeds label prominence.
func buildUniverseMap(systems []db.ListMapSystemsRow, regions []db.SdeRegion, cons []db.SdeConstellation, jumps []db.ListMapJumpsRow, stationSystems map[int64]bool) universeMapPayload {
	p := universeMapPayload{Version: universeMapVersion}
	regionIdx := map[int64]int{}
	for _, r := range regions {
		regionIdx[r.RegionID] = len(p.Regions)
		p.Regions = append(p.Regions, universeMapRegion{ID: r.RegionID, Name: r.Name})
	}
	consIdx := map[int64]int{}
	for _, c := range cons {
		ri, ok := regionIdx[c.RegionID]
		if !ok {
			continue
		}
		consIdx[c.ConstellationID] = len(p.Cons)
		p.Cons = append(p.Cons, universeMapCons{ID: c.ConstellationID, Name: c.Name, Region: ri})
	}

	// Keep the systems whose region is on the map, in ID order.
	var kept []db.ListMapSystemsRow
	for _, s := range systems {
		if _, ok := regionIdx[s.RegionID]; ok {
			kept = append(kept, s)
		}
	}
	n := len(kept)
	sysIdx := make(map[int64]int, n)
	S := &p.Sys
	S.ID, S.Name = make([]int64, n), make([]string, n)
	S.X, S.Y = make([]int, n), make([]int, n)
	S.PX, S.PY = make([]int, n), make([]int, n)
	S.Sec, S.Region, S.Cons = make([]float64, n), make([]int, n), make([]int, n)
	S.RX, S.RY, S.Faction = make([]int, n), make([]int, n), make([]int64, n)
	S.Prom = make([]int, n)

	// The universe frame: top-down, x right, z up (flipped for
	// screen y), scaled so the longest side spans universeFrame.
	pts := make([][2]float64, n)
	for i, s := range kept {
		sysIdx[s.SystemID] = i
		pts[i] = [2]float64{s.X, -s.Z}
	}
	uni := fitFrame(pts, universeFrame)
	for i, s := range kept {
		S.ID[i], S.Name[i] = s.SystemID, s.Name
		S.X[i], S.Y[i] = int(math.Round(uni[i][0])), int(math.Round(uni[i][1]))
		S.Sec[i] = math.Round(s.Security*1000) / 1000
		S.Region[i] = regionIdx[s.RegionID]
		S.Cons[i] = -1
		if ci, ok := consIdx[s.ConstellationID]; ok {
			S.Cons[i] = ci
		}
		S.Faction[i] = s.FactionID
	}

	// The stargate graph, by index.
	adj := make([][]int, n)
	for _, j := range jumps {
		a, okA := sysIdx[j.A]
		b, okB := sysIdx[j.B]
		if !okA || !okB || a == b {
			continue
		}
		p.Gates = append(p.Gates, a, b)
		adj[a] = append(adj[a], b)
		adj[b] = append(adj[b], a)
	}

	// Label prominence, from the graph just built and the SDE's
	// station list: precomputed here like every other part of
	// the payload, so the client only ever reads the order.
	for i, s := range kept {
		S.Prom[i] = mapProminence(len(adj[i]), stationSystems[s.SystemID], tradeHubSystems[s.SystemID])
	}

	// Each region: its label point and its own decluttered frame.
	members := make([][]int, len(p.Regions))
	for i := range kept {
		members[S.Region[i]] = append(members[S.Region[i]], i)
	}
	regionLaid := make([][][2]float64, len(p.Regions))
	for ri, idxs := range members {
		if len(idxs) == 0 {
			continue
		}
		cx, cy := 0.0, 0.0
		local := make([][2]float64, len(idxs))
		pos := make(map[int]int, len(idxs))
		for k, i := range idxs {
			cx += float64(S.X[i])
			cy += float64(S.Y[i])
			local[k] = pts[i]
			pos[i] = k
		}
		p.Regions[ri].X = int(math.Round(cx / float64(len(idxs))))
		p.Regions[ri].Y = int(math.Round(cy / float64(len(idxs))))
		var edges [][2]int
		for k, i := range idxs {
			for _, j := range adj[i] {
				if kj, ok := pos[j]; ok && k < kj {
					edges = append(edges, [2]int{k, kj})
				}
			}
		}
		laid := layoutRegion(local, edges)
		regionLaid[ri] = laid
		w, h := 0.0, 0.0
		for k, i := range idxs {
			S.RX[i], S.RY[i] = int(math.Round(laid[k][0])), int(math.Round(laid[k][1]))
			w, h = math.Max(w, laid[k][0]), math.Max(h, laid[k][1])
		}
		p.Regions[ri].W, p.Regions[ri].H = int(math.Ceil(w)), int(math.Ceil(h))
	}

	// The Improved Projection: the regions' layouts assembled
	// into the universe frame (layoutMosaic), precomputed here
	// like every other layout and carried beside the true
	// positions, so the client only ever chooses between two
	// stored frames.
	proj := nudgeOverlaps(layoutMosaic(uni, members, S.Cons, regionLaid))
	for i := range kept {
		S.PX[i], S.PY[i] = int(math.Round(proj[i][0])), int(math.Round(proj[i][1]))
	}
	for ri, idxs := range members {
		if len(idxs) == 0 {
			continue
		}
		pcx, pcy := 0.0, 0.0
		for _, i := range idxs {
			pcx += float64(S.PX[i])
			pcy += float64(S.PY[i])
		}
		p.Regions[ri].PX = int(math.Round(pcx / float64(len(idxs))))
		p.Regions[ri].PY = int(math.Round(pcy / float64(len(idxs))))
	}
	return p
}

// fitFrame scales points so their bounding box starts at the origin
// and its longest side spans size, aspect kept.
func fitFrame(pts [][2]float64, size float64) [][2]float64 {
	out := make([][2]float64, len(pts))
	if len(pts) == 0 {
		return out
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	span := math.Max(maxX-minX, maxY-minY)
	if span <= 0 {
		span = 1
	}
	k := size / span
	for i, p := range pts {
		out[i] = [2]float64{(p[0] - minX) * k, (p[1] - minY) * k}
	}
	return out
}

// layoutRegion is a region's tactical layout: its true positions
// fitted to regionFrame, then relaxed until no two systems sit
// closer than the region's minimum spacing. Each step pushes
// crowded pairs apart, pulls long gate links back toward the
// spacing they would have uncrowded, and pulls every system gently
// back toward its true position, so the picture stays the real one.
// Deterministic: no randomness, a fixed step count, and coincident
// systems separated along a direction fixed by their index.
func layoutRegion(true2D [][2]float64, edges [][2]int) [][2]float64 {
	n := len(true2D)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return [][2]float64{{regionFrame / 2, regionFrame / 2}}
	}
	anchor := fitFrame(true2D, regionFrame)
	pos := make([][2]float64, n)
	copy(pos, anchor)
	minD := regionMinSpacing(n)
	const steps = 320
	for step := 0; step < steps; step++ {
		cool := 1 - float64(step)/steps*0.6
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				dx, dy := pos[j][0]-pos[i][0], pos[j][1]-pos[i][1]
				d := math.Hypot(dx, dy)
				if d >= minD {
					continue
				}
				if d < 1e-6 {
					a := float64(i*7+j*13) * 0.618
					dx, dy, d = math.Cos(a), math.Sin(a), 1
				}
				push := (minD - d) / 2 * 0.5 * cool
				ux, uy := dx/d, dy/d
				pos[i][0] -= ux * push
				pos[i][1] -= uy * push
				pos[j][0] += ux * push
				pos[j][1] += uy * push
			}
		}
		for _, e := range edges {
			a, b := e[0], e[1]
			dx, dy := pos[b][0]-pos[a][0], pos[b][1]-pos[a][1]
			d := math.Hypot(dx, dy)
			if d <= 2.4*minD || d < 1e-6 {
				continue
			}
			pull := (d - 2.4*minD) * 0.02 * cool
			ux, uy := dx/d, dy/d
			pos[a][0] += ux * pull
			pos[a][1] += uy * pull
			pos[b][0] -= ux * pull
			pos[b][1] -= uy * pull
		}
		// The pull home eases off as the layout cools, so the
		// last steps settle spacing rather than fight it.
		home := 0.012 * cool * cool
		for i := range pos {
			pos[i][0] += (anchor[i][0] - pos[i][0]) * home
			pos[i][1] += (anchor[i][1] - pos[i][1]) * home
		}
	}
	return fitFrame(pos, regionFrame)
}

// regionMinSpacing is the closest two systems of an n-system region
// may sit in its tactical layout: wide enough that names fit beside
// each other at the region's fitted zoom.
func regionMinSpacing(n int) float64 {
	return math.Min(math.Max(regionFrame/math.Sqrt(float64(n))*0.8, 45), 160)
}

// mosaicCentroidSpread is how far the mosaic pushes the
// regions' anchors apart, radially from the frame's centre,
// before the tiles are set down: neighbouring tiles clear each
// other a little, and no bearing between two anchors changes --
// the spread is one similarity, and so is the final frame fit.
const mosaicCentroidSpread = 1.05

// The mosaic's schematic step, run on a region's laid-out
// systems before its tile is set down: constellation
// footprints are normalized, then the constellations are
// separated where the new sizes crowd each other.
//
// A constellation's footprint is its RMS radius -- the
// root-mean-square distance of its systems from its centroid.
// The normalization rescales each constellation rigidly about
// its own centroid so its footprint becomes k*sqrt(n) for its
// n systems, with k the region's median per-system density
// (the median of r/sqrt(n) over the region's multi-system
// constellations): the median constellation keeps its size,
// sparse sprawlers shrink toward the common weight, dense
// knots grow into it, and a constellation's internal shape is
// preserved exactly -- only its size changes. The scale
// factor is clamped to [mosaicNormShrink, mosaicNormGrow]:
// the shrink bound is set by measurement on the real payload
// (at 0.85 The Forge's closest pair in the finished payload
// fell to 55 units; at 0.90 it keeps 67), and the grow bound
// is a sanity cap no real constellation reaches (the largest
// factor the rule asks for anywhere is about 1.6).
const (
	mosaicNormShrink = 0.90
	mosaicNormGrow   = 1.9
)

// The separation pass then treats each constellation as a
// rigid body and restores the spacing the rescaling disturbed:
// wherever two systems of different constellations sit closer
// than mosaicSepFloor times the region layout's own minimum
// pair distance, their constellations are pushed apart along
// the pair's axis, each body by half the deficit. The test is
// on system pairs, not on covering discs: in a dense region
// the discs overlap everywhere and disc-driven pushes shove
// glyphs through each other, while pair-driven pushes fire
// only where systems actually crowd. Corrections are computed
// against the step's starting offsets and applied together,
// damped (the accumulated form layoutRegion's cousins use),
// and no constellation moves more than mosaicSepCap in total
// or leaves the region frame. Pairs that stay below the floor
// inside the cap stay close; the pass is a janitor, not a
// solver. What it cannot do is change any distance inside a
// constellation: those are the normalization's, set above.
const (
	mosaicSepFloor = 0.94
	mosaicSepCap   = 80.0
	mosaicSepSteps = 32
	mosaicSepDamp  = 0.5
)

// layoutMosaic assembles the projected frame from the regions'
// own tactical layouts, one tile per region. A region's shape
// in the projection is its region-view layout under one
// uniform scale per region, with each constellation's
// footprint first normalized (normalizeConstellations): the
// shape is set down at the size of the region's true extent,
// centred on the region's true centroid (spread a little for
// clearance, see mosaicCentroidSpread). Relative sizes and
// bearings across the universe stay the true ones, and the
// gate web ties the tiles together. Nothing is re-solved at
// this level: where a system sits inside its constellation
// is the region layout's business, settled there.
// Deterministic: means and extents over fixed member lists,
// a fixed spread, and the shared frame fit.
func layoutMosaic(uni [][2]float64, members [][]int, consOf []int, regionLaid [][][2]float64) [][2]float64 {
	out := make([][2]float64, len(uni))
	if len(uni) == 0 {
		return out
	}
	// The frame's centre, the pivot the anchors spread from.
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range uni {
		minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	ctrX, ctrY := (minX+maxX)/2, (minY+maxY)/2
	for ri, idxs := range members {
		laid := regionLaid[ri]
		if len(idxs) == 0 || len(laid) != len(idxs) {
			continue
		}
		shape := normalizeConstellations(idxs, consOf, laid)
		var cx, cy, rcx, rcy float64
		loX, loY := math.Inf(1), math.Inf(1)
		hiX, hiY := math.Inf(-1), math.Inf(-1)
		for k, i := range idxs {
			cx += uni[i][0]
			cy += uni[i][1]
			rcx += shape[k][0]
			rcy += shape[k][1]
			loX, hiX = math.Min(loX, uni[i][0]), math.Max(hiX, uni[i][0])
			loY, hiY = math.Min(loY, uni[i][1]), math.Max(hiY, uni[i][1])
		}
		m := float64(len(idxs))
		cx, cy, rcx, rcy = cx/m, cy/m, rcx/m, rcy/m
		// The tile's scale: the shape's own frame spans
		// regionFrame on its longest side, so scaling by the
		// region's true extent over regionFrame sets the tile
		// down at the region's true size.
		scale := math.Max(hiX-loX, hiY-loY) / regionFrame
		if scale <= 0 {
			// Every system of the region on one true point:
			// keep the shape at its own scale rather than
			// collapsing it onto the anchor.
			scale = 1
		}
		ax := ctrX + (cx-ctrX)*mosaicCentroidSpread
		ay := ctrY + (cy-ctrY)*mosaicCentroidSpread
		for k, i := range idxs {
			out[i] = [2]float64{ax + scale*(shape[k][0]-rcx), ay + scale*(shape[k][1]-rcy)}
		}
	}
	return fitFrame(out, universeFrame)
}

// normalizeConstellations applies the mosaic's schematic step
// to one region's laid-out systems (see the constants above):
// each constellation is rescaled about its own centroid to
// the footprint its system count earns, the constellations
// are separated where the new sizes crowd each other, and the
// region is refitted to its frame, so the region's extent --
// the tile's size reference -- is the layout's own. Systems
// whose constellation is unknown are bodies of one: no
// footprint to normalize, but the separation can still move
// them. Deterministic: groups in first-appearance order,
// pairs in index order, a fixed step count, no randomness.
func normalizeConstellations(idxs []int, consOf []int, laid [][2]float64) [][2]float64 {
	out := make([][2]float64, len(laid))
	copy(out, laid)
	if len(laid) < 2 {
		return out
	}

	// The region's constellations, in first-appearance order.
	type consGroup struct {
		ks     []int
		cx, cy float64 // centroid in the laid frame
		rms    float64 // footprint in the laid frame
	}
	var groups []*consGroup
	byCons := make(map[int]*consGroup)
	for k, i := range idxs {
		c := -1
		if i < len(consOf) {
			c = consOf[i]
		}
		if c < 0 {
			// Unknown constellation: a group of one, so the
			// normalization leaves the system where it sits.
			c = -k - 2
		}
		g := byCons[c]
		if g == nil {
			g = &consGroup{}
			byCons[c] = g
			groups = append(groups, g)
		}
		g.ks = append(g.ks, k)
	}
	for _, g := range groups {
		for _, k := range g.ks {
			g.cx += laid[k][0]
			g.cy += laid[k][1]
		}
		g.cx /= float64(len(g.ks))
		g.cy /= float64(len(g.ks))
		if len(g.ks) > 1 {
			var ss float64
			for _, k := range g.ks {
				dx, dy := laid[k][0]-g.cx, laid[k][1]-g.cy
				ss += dx*dx + dy*dy
			}
			g.rms = math.Sqrt(ss / float64(len(g.ks)))
		}
	}

	// The region layout's own minimum pair distance: the
	// spacing standard the separation pass restores toward.
	laidMin := math.Inf(1)
	for a := 0; a < len(laid); a++ {
		for b := a + 1; b < len(laid); b++ {
			d := math.Hypot(laid[a][0]-laid[b][0], laid[a][1]-laid[b][1])
			laidMin = math.Min(laidMin, d)
		}
	}

	// The rescaling: k is the median per-system density, so
	// the median constellation's footprint is unchanged.
	var dens []float64
	for _, g := range groups {
		if len(g.ks) >= 2 && g.rms > 0 {
			dens = append(dens, g.rms/math.Sqrt(float64(len(g.ks))))
		}
	}
	if len(dens) > 0 {
		sort.Float64s(dens)
		kmed := dens[len(dens)/2]
		for _, g := range groups {
			if len(g.ks) < 2 || g.rms <= 0 {
				continue
			}
			f := kmed * math.Sqrt(float64(len(g.ks))) / g.rms
			f = math.Min(math.Max(f, mosaicNormShrink), mosaicNormGrow)
			if f == 1 {
				continue
			}
			for _, k := range g.ks {
				out[k] = [2]float64{
					g.cx + (laid[k][0]-g.cx)*f,
					g.cy + (laid[k][1]-g.cy)*f,
				}
			}
		}
	}

	// The separation: rigid constellation bodies pushed apart
	// where systems of different constellations crowd.
	if len(groups) > 1 && laidMin > 0 {
		floor := mosaicSepFloor * laidMin
		bodyOf := make([]int, len(laid))
		startC := make([][2]float64, len(groups))
		for gi, g := range groups {
			var cx, cy float64
			for _, k := range g.ks {
				bodyOf[k] = gi
				cx += out[k][0]
				cy += out[k][1]
			}
			startC[gi] = [2]float64{cx / float64(len(g.ks)), cy / float64(len(g.ks))}
		}
		off := make([][2]float64, len(groups))
		cur := make([][2]float64, len(laid))
		for step := 0; step < mosaicSepSteps; step++ {
			for k := range laid {
				b := bodyOf[k]
				cur[k] = [2]float64{out[k][0] + off[b][0], out[k][1] + off[b][1]}
			}
			var pairs [][2]int
			for a := 0; a < len(laid); a++ {
				for b := a + 1; b < len(laid); b++ {
					if bodyOf[a] == bodyOf[b] {
						continue
					}
					if math.Hypot(cur[b][0]-cur[a][0], cur[b][1]-cur[a][1]) < floor {
						pairs = append(pairs, [2]int{a, b})
					}
				}
			}
			if len(pairs) == 0 {
				break
			}
			corr := make([][2]float64, len(groups))
			for _, pr := range pairs {
				a, b := pr[0], pr[1]
				dx, dy := cur[b][0]-cur[a][0], cur[b][1]-cur[a][1]
				d := math.Hypot(dx, dy)
				var ux, uy, push float64
				if d < 1e-6 {
					// Coincident: split along a direction
					// fixed by the pair's indices, as
					// layoutRegion does.
					ang := float64(a*7+b*13) * 0.618
					ux, uy = math.Cos(ang), math.Sin(ang)
					push = floor / 2 * mosaicSepDamp
				} else {
					ux, uy = dx/d, dy/d
					push = (floor - d) / 2 * mosaicSepDamp
				}
				ba, bb := bodyOf[a], bodyOf[b]
				corr[ba][0] -= ux * push
				corr[ba][1] -= uy * push
				corr[bb][0] += ux * push
				corr[bb][1] += uy * push
			}
			for gi := range groups {
				vx, vy := off[gi][0]+corr[gi][0], off[gi][1]+corr[gi][1]
				if l := math.Hypot(vx, vy); l > mosaicSepCap {
					vx *= mosaicSepCap / l
					vy *= mosaicSepCap / l
				}
				cx := math.Min(math.Max(startC[gi][0]+vx, 0), regionFrame)
				cy := math.Min(math.Max(startC[gi][1]+vy, 0), regionFrame)
				off[gi] = [2]float64{cx - startC[gi][0], cy - startC[gi][1]}
			}
		}
		for k := range out {
			b := bodyOf[k]
			out[k] = [2]float64{out[k][0] + off[b][0], out[k][1] + off[b][1]}
		}
	}
	return fitFrame(out, regionFrame)
}

// The nudge pass: where two regions' tiles meet, a system of
// one can land on top of a system of the other -- the tiles are
// set down independently, at their true sizes, and the true
// map's borders interleave. The pass separates the pairs that
// sit closer than mosaicNudgeFloor, and nothing else: each
// pair splits along its own axis, the corrections of a step
// are all computed against the step's starting positions and
// applied together, damped, and no system moves more than
// mosaicNudgeCap across the whole pass. It is a janitor for
// border pairs, not a solver: pairs it cannot separate inside
// the cap stay close, and systems in no close pair never move.
const (
	mosaicNudgeFloor = 26.0
	mosaicNudgeCap   = 30.0
	mosaicNudgeSteps = 16
	mosaicNudgeDamp  = 0.5
)

func nudgeOverlaps(pos [][2]float64) [][2]float64 {
	n := len(pos)
	out := make([][2]float64, n)
	copy(out, pos)
	if n < 2 {
		return out
	}
	start := make([][2]float64, n)
	copy(start, pos)
	cur := make([][2]float64, n)
	copy(cur, pos)
	for step := 0; step < mosaicNudgeSteps; step++ {
		pairs := closePairs(cur, mosaicNudgeFloor)
		if len(pairs) == 0 {
			break
		}
		corr := make([][2]float64, n)
		for _, pr := range pairs {
			i, j := pr[0], pr[1]
			dx, dy := cur[j][0]-cur[i][0], cur[j][1]-cur[i][1]
			d := math.Hypot(dx, dy)
			var ux, uy, push float64
			if d < 1e-6 {
				// Coincident: split along a direction fixed
				// by the pair's indices, as layoutRegion does.
				a := float64(i*7+j*13) * 0.618
				ux, uy = math.Cos(a), math.Sin(a)
				push = mosaicNudgeFloor / 2 * mosaicNudgeDamp
			} else {
				ux, uy = dx/d, dy/d
				push = (mosaicNudgeFloor - d) / 2 * mosaicNudgeDamp
			}
			corr[i][0] -= ux * push
			corr[i][1] -= uy * push
			corr[j][0] += ux * push
			corr[j][1] += uy * push
		}
		for i := range cur {
			vx := cur[i][0] + corr[i][0] - start[i][0]
			vy := cur[i][1] + corr[i][1] - start[i][1]
			if l := math.Hypot(vx, vy); l > mosaicNudgeCap {
				vx *= mosaicNudgeCap / l
				vy *= mosaicNudgeCap / l
			}
			cur[i] = [2]float64{start[i][0] + vx, start[i][1] + vy}
		}
	}
	copy(out, cur)
	return out
}

// closePairs lists the index pairs closer than floor, in index
// order, found through a bucket grid of floor-sized cells.
func closePairs(pos [][2]float64, floor float64) [][2]int {
	grid := make(map[[2]int][]int)
	for i, p := range pos {
		cell := [2]int{int(math.Floor(p[0] / floor)), int(math.Floor(p[1] / floor))}
		grid[cell] = append(grid[cell], i)
	}
	var pairs [][2]int
	for i, p := range pos {
		cx, cy := int(math.Floor(p[0]/floor)), int(math.Floor(p[1]/floor))
		for gx := cx - 1; gx <= cx+1; gx++ {
			for gy := cy - 1; gy <= cy+1; gy++ {
				for _, j := range grid[[2]int{gx, gy}] {
					if j <= i {
						continue
					}
					if math.Hypot(pos[j][0]-p[0], pos[j][1]-p[1]) < floor {
						pairs = append(pairs, [2]int{i, j})
					}
				}
			}
		}
	}
	sort.Slice(pairs, func(a, b int) bool {
		if pairs[a][0] != pairs[b][0] {
			return pairs[a][0] < pairs[b][0]
		}
		return pairs[a][1] < pairs[b][1]
	})
	return pairs
}

// rebuildUniverseMap builds the payload from the stored SDE and
// stores it, gzipped, with its content hash. A database whose SDE
// predates the geometry has nothing to build yet.
func (app *Application) rebuildUniverseMap(ctx context.Context) error {
	q := app.queries
	systems, err := q.ListMapSystems(ctx)
	if err != nil {
		return fmt.Errorf("list systems: %w", err)
	}
	if len(systems) < universeMapMinSystems {
		return nil
	}
	regions, err := q.ListMapRegions(ctx)
	if err != nil {
		return fmt.Errorf("list regions: %w", err)
	}
	cons, err := q.ListMapConstellations(ctx)
	if err != nil {
		return fmt.Errorf("list constellations: %w", err)
	}
	jumps, err := q.ListMapJumps(ctx)
	if err != nil {
		return fmt.Errorf("list jumps: %w", err)
	}
	stationRows, err := q.ListMapStationSystems(ctx)
	if err != nil {
		return fmt.Errorf("list station systems: %w", err)
	}
	stationSystems := make(map[int64]bool, len(stationRows))
	for _, id := range stationRows {
		stationSystems[id] = true
	}
	started := time.Now()
	payload := buildUniverseMap(systems, regions, cons, jumps, stationSystems)
	gz, etag, err := packMapJSON(payload)
	if err != nil {
		return err
	}
	if err := q.UpsertMapUniverse(ctx, db.UpsertMapUniverseParams{
		Version: universeMapVersion, Etag: etag, Payload: gz,
		Systems: int32(len(payload.Sys.ID)), BuiltAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	logging.Infof("map: universe built (%d systems, %d gates, %d KB gzipped, %s)",
		len(payload.Sys.ID), len(payload.Gates)/2, len(gz)/1024, time.Since(started).Round(time.Millisecond))
	return nil
}

// packMapJSON marshals a map payload, gzips it for storage, and
// hashes the JSON for its etag (which versions its URL).
func packMapJSON(v any) (gz []byte, etag string, err error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("marshal: %w", err)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(raw); err != nil {
		return nil, "", fmt.Errorf("compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, "", fmt.Errorf("compress: %w", err)
	}
	sum := sha256.Sum256(raw)
	return buf.Bytes(), hex.EncodeToString(sum[:8]), nil
}

// ensureUniverseMap rebuilds the stored payload when it is missing
// or was built by an older builder (sdeMaintenance calls it; an
// import rebuilds unconditionally).
func (app *Application) ensureUniverseMap(ctx context.Context) {
	meta, err := app.queries.GetMapUniverseMeta(ctx)
	if err == nil && meta.Version == universeMapVersion {
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("map: read payload meta: %v", err)
		return
	}
	if err := app.rebuildUniverseMap(ctx); err != nil {
		logging.Errorf("map: rebuild: %v", err)
	}
}

// handleUniverseJSON serves the stored payload. Its URL carries the
// content hash (?v=), so a matching request is cached for good; the
// body is stored gzipped and sent as-is to any client that takes
// gzip (every browser), unpacked only for one that does not.
func (app *Application) handleUniverseJSON(w http.ResponseWriter, r *http.Request) {
	row, err := app.queries.GetMapUniversePayload(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "the universe map has not been built yet", http.StatusServiceUnavailable)
			return
		}
		logging.Errorf("map: read payload: %v", err)
		http.Error(w, "map unavailable", http.StatusInternalServerError)
		return
	}
	serveMapJSON(w, r, row.Etag, row.Payload, 300)
}

// serveMapJSON sends a stored gzipped map payload: cached for good
// at its hashed URL (?v= matching the etag), for shortCache seconds
// otherwise; as stored to any client that takes gzip (every
// browser), unpacked for one that does not.
func serveMapJSON(w http.ResponseWriter, r *http.Request, etag string, gz []byte, shortCache int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", `"`+etag+`"`)
	if r.URL.Query().Get("v") == etag {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", shortCache))
	}
	if match := r.Header.Get("If-None-Match"); match == `"`+etag+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		http.Error(w, "map unavailable", http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, zr)
}

// mapRegionLink is one region in the map page's server-rendered
// index: the no-script fallback, and the region jump list.
type mapRegionLink struct {
	ID   int64
	Name string
}

// mapPageView is the /map/ page body.
type mapPageView struct {
	Ready      bool
	PayloadURL string
	// LayersURL is the live layers payload's hashed URL ("" until
	// the worker has built one).
	LayersURL string
	Systems   int
	Regions   []mapRegionLink
	// Route is the reader's side of routing (map_route.go).
	Route mapRouteView
}

// loadMapPageView reads the stored payload's meta and the region
// list; nothing is computed here.
func (app *Application) loadMapPageView(ctx context.Context) *mapPageView {
	v := &mapPageView{}
	if meta, err := app.queries.GetMapUniverseMeta(ctx); err == nil {
		v.Ready = true
		v.PayloadURL = "/map/universe.json?v=" + meta.Etag
		v.Systems = int(meta.Systems)
	}
	if lm, err := app.queries.GetMapLayersMeta(ctx); err == nil {
		v.LayersURL = "/map/layers.json?v=" + lm.Etag
	}
	if rows, err := app.queries.ListMapRegions(ctx); err == nil {
		for _, r := range rows {
			v.Regions = append(v.Regions, mapRegionLink{ID: r.RegionID, Name: r.Name})
		}
		sort.Slice(v.Regions, func(i, j int) bool { return v.Regions[i].Name < v.Regions[j].Name })
	}
	v.Route = app.loadMapRouteView(ctx)
	return v
}

// handleMapPage renders the map's shell: a canvas map.js fills from
// the payload, and a region index that stands on its own without
// script.
func (app *Application) handleMapPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	data.Map = app.loadMapPageView(ctx)
	app.render(ctx, w, http.StatusOK, "map.html", data)
}
