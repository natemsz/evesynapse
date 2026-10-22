package app

// ---------------------------------------------------------------------------
// The universe map (schema 072): the payload the map page draws,
// built from the SDE's geometry once per import and stored, never
// computed at render.
//
// Three layouts ride in one payload. The universe view is the true
// top-down picture: every known-space system at its SDE x and z
// (z flipped so galactic north is up), scaled into a 0..10000 frame.
// Beside it sits the Improved Projection: the same picture with
// crowding nudged apart inside a per-system displacement budget
// (layoutProjection), so the geography -- densities, voids, cluster
// shapes -- survives at every scale and only overlaps give way.
// The region view is the tactical one, computed rather than
// hand-drawn: each region's systems start from the same true
// positions, fitted to their own 0..1000 frame, then pushed apart
// until no two crowd each other (a fixed-step relaxation anchored to
// the true positions, deterministic, so a layout never shifts
// between builds of the same dump). Where a system sits relative to
// its neighbours stays the true picture; only overlaps give way.
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

	// The Improved Projection: the universe nudged out of its
	// own crowding inside per-system budgets (layoutProjection),
	// precomputed here like every other layout and carried
	// beside the true positions, so the client only ever
	// chooses between two stored frames.
	proj := layoutProjection(uni, S.Cons)
	for i := range kept {
		S.PX[i], S.PY[i] = int(math.Round(proj[i][0])), int(math.Round(proj[i][1]))
	}

	// Each region: its label point and its own decluttered frame.
	members := make([][]int, len(p.Regions))
	for i := range kept {
		members[S.Region[i]] = append(members[S.Region[i]], i)
	}
	for ri, idxs := range members {
		if len(idxs) == 0 {
			continue
		}
		cx, cy := 0.0, 0.0
		pcx, pcy := 0.0, 0.0
		local := make([][2]float64, len(idxs))
		pos := make(map[int]int, len(idxs))
		for k, i := range idxs {
			cx += float64(S.X[i])
			cy += float64(S.Y[i])
			pcx += float64(S.PX[i])
			pcy += float64(S.PY[i])
			local[k] = pts[i]
			pos[i] = k
		}
		p.Regions[ri].X = int(math.Round(cx / float64(len(idxs))))
		p.Regions[ri].Y = int(math.Round(cy / float64(len(idxs))))
		p.Regions[ri].PX = int(math.Round(pcx / float64(len(idxs))))
		p.Regions[ri].PY = int(math.Round(pcy / float64(len(idxs))))
		var edges [][2]int
		for k, i := range idxs {
			for _, j := range adj[i] {
				if kj, ok := pos[j]; ok && k < kj {
					edges = append(edges, [2]int{k, kj})
				}
			}
		}
		laid := layoutRegion(local, edges)
		w, h := 0.0, 0.0
		for k, i := range idxs {
			S.RX[i], S.RY[i] = int(math.Round(laid[k][0])), int(math.Round(laid[k][1]))
			w, h = math.Max(w, laid[k][0]), math.Max(h, laid[k][1])
		}
		p.Regions[ri].W, p.Regions[ri].H = int(math.Ceil(w)), int(math.Ceil(h))
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

// universeMinSpacing is the closest two systems may sit in the
// Improved Projection: at the universe's fitted size this is a
// few screen pixels -- dots stop overlapping, and at reading
// zoom neighbouring names have room -- without demanding so
// much space that the anchored layout tears the map apart.
func universeMinSpacing(n int) float64 {
	if n < 2 {
		return 60
	}
	return math.Min(math.Max(universeFrame/math.Sqrt(float64(n))*0.62, 42), 95)
}

// The anchored projection's tuning. A system's displacement
// budget comes from its own crowding: count out to
// projCrowdNeighbours neighbours, ask how large a disc packed at
// the minimum spacing would need to be to hold them, and let the
// system roam a little beyond that growth. A system in open
// space has no crowd to hold, so its budget is the bare slack
// and it effectively stays home.
const (
	// projCrowdNeighbours bounds how many neighbours' worth of
	// crowding one system answers for: crowds bigger than this
	// are geography, and no single system is displaced on
	// their account.
	projCrowdNeighbours = 256
	// projPackPerPoint is the area, in units of the minimum
	// spacing squared, one system needs in a hexagonal packing.
	projPackPerPoint = 0.866
	// projCapBase is every system's displacement slack, in
	// minimum spacings: enough to step out of a single overlap.
	projCapBase = 0.18
	// projCapSlack is the headroom over the estimated growth a
	// crowded neighbourhood needs.
	projCapSlack = 1.3
	// projCapCeiling is the absolute displacement budget, as a
	// fraction of the frame: no system crosses the map.
	projCapCeiling = 0.28
	// projHomePull is how fast a system nothing is crowding
	// returns home each step.
	projHomePull = 0.08
	// projDriftShare is how strongly a system follows its
	// constellation's mean displacement each step, so a
	// constellation pushed by its neighbours travels as a
	// group instead of scattering.
	projDriftShare = 0.02
	// projRelax damps each step's accumulated pair corrections:
	// a pile resolves over several steps instead of every pair
	// correction landing at full strength in one.
	projRelax = 0.7
	projSteps = 520
	// projReliefs bounds the budget-relief passes after the
	// main solve; projStuckFrac is the spacing fraction below
	// which a pair counts as still crowded, and
	// projReliefGrow is how much a stuck system's budget
	// grows per pass.
	projReliefs    = 3
	projStuckFrac  = 0.85
	projReliefGrow = 1.3
)

// projBudgets returns each system's displacement budget for the
// anchored projection, from the crowding around its true
// position: for every crowd size m up to projCrowdNeighbours,
// the disc that holds m neighbours (and the system) packed at
// minD has radius minD*sqrt((m+1)*projPackPerPoint/pi); where
// that radius exceeds the distance to the m-th neighbour at
// home, the neighbourhood must grow by the difference, and the
// largest such growth over all m is the system's need.
func projBudgets(anchor [][2]float64, minD float64) []float64 {
	n := len(anchor)
	caps := make([]float64, n)
	ds := make([]float64, 0, n-1)
	for i := 0; i < n; i++ {
		ds = ds[:0]
		for j := 0; j < n; j++ {
			if j != i {
				ds = append(ds, math.Hypot(anchor[j][0]-anchor[i][0], anchor[j][1]-anchor[i][1]))
			}
		}
		sort.Float64s(ds)
		need := 0.0
		mMax := projCrowdNeighbours
		if len(ds) < mMax {
			mMax = len(ds)
		}
		for m := 1; m <= mMax; m++ {
			radius := minD * math.Sqrt(float64(m+1)*projPackPerPoint/math.Pi)
			if grow := radius - ds[m-1]; grow > need {
				need = grow
			}
		}
		c := minD*projCapBase + projCapSlack*need
		if ceil := projCapCeiling * universeFrame; c > ceil {
			c = ceil
		}
		caps[i] = c
	}
	return caps
}

// layoutProjection is the universe's tactical picture, built by
// nudging the true positions (already fitted to universeFrame)
// out of their own crowding -- never by re-laying the universe
// out. Each system holds a displacement budget from the
// crowding around its home (projBudgets): pairs closer than
// the minimum spacing have the gap between them closed,
// sharing the move by mobility (a system with budget to spare
// does the moving), every system is clamped to its budget,
// and a system nothing is crowding returns home. Repulsion is
// local by construction -- only pairs already closer than the
// spacing interact -- so voids stay void and the density
// pattern that makes the map recognisable survives;
// constellations also share their mean displacement, so a
// group pushed by a denser neighbour travels together.
// Nothing pulls gate links toward a standard length and
// nothing equilibrates the whole field: the projected map is
// the true map with its overlaps relieved, at every scale.
// Where a pile still crowds at the end of a solve, the
// budgets of the systems in it grow and the solve resumes --
// the estimate yields to the evidence, in that pile only.
// Deterministic: buckets are built and walked in index order,
// steps are fixed, and coincident systems separate along an
// index-fixed direction.
func layoutProjection(true2D [][2]float64, groups []int) [][2]float64 {
	n := len(true2D)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return [][2]float64{{universeFrame / 2, universeFrame / 2}}
	}
	anchor := make([][2]float64, n)
	copy(anchor, true2D)
	pos := make([][2]float64, n)
	copy(pos, anchor)
	minD := universeMinSpacing(n)
	caps := projBudgets(anchor, minD)
	// Constellation member lists, in first-member order, for
	// drift sharing.
	members := make(map[int][]int)
	var groupOrder []int
	for i, g := range groups {
		if g < 0 {
			continue
		}
		if _, ok := members[g]; !ok {
			groupOrder = append(groupOrder, g)
		}
		members[g] = append(members[g], i)
	}

	solve := func(steps int) {
		// Mobility: the share of a pair's correction a system
		// takes, from its budget. A system deep in budget
		// moves; one near its limit makes its neighbour do
		// more of the work.
		mobility := make([]float64, n)
		for i := range caps {
			mobility[i] = caps[i] / (caps[i] + 0.5*minD)
		}
		type cellKey [2]int
		crowded := make([]bool, n)
		delta := make([][2]float64, n)
		for step := 0; step < steps; step++ {
			buckets := make(map[cellKey][]int, n*2)
			for i := range pos {
				k := cellKey{int(math.Floor(pos[i][0] / minD)), int(math.Floor(pos[i][1] / minD))}
				buckets[k] = append(buckets[k], i)
			}
			for i := range crowded {
				crowded[i] = false
				delta[i] = [2]float64{}
			}
			// Corrections accumulate against the step's
			// starting positions and land together,
			// damped: a system in a pile feels the sum of
			// its neighbours' pushes as one net shove
			// instead of a sequence of full corrections
			// that fling it from pair to pair.
			for i := 0; i < n; i++ {
				ci, cj := int(math.Floor(pos[i][0]/minD)), int(math.Floor(pos[i][1]/minD))
				for gx := ci - 1; gx <= ci+1; gx++ {
					for gy := cj - 1; gy <= cj+1; gy++ {
						for _, j := range buckets[cellKey{gx, gy}] {
							if j <= i {
								continue
							}
							dx, dy := pos[j][0]-pos[i][0], pos[j][1]-pos[i][1]
							d := math.Hypot(dx, dy)
							if d >= minD {
								continue
							}
							if d < 1e-6 {
								a := float64(i*7+j*13) * 0.618
								dx, dy, d = math.Cos(a), math.Sin(a), 1
							}
							move := (minD - d) * projRelax
							wi, wj := mobility[i], mobility[j]
							mi := move * wi / (wi + wj)
							mj := move - mi
							ux, uy := dx/d, dy/d
							delta[i][0] -= ux * mi
							delta[i][1] -= uy * mi
							delta[j][0] += ux * mj
							delta[j][1] += uy * mj
							crowded[i], crowded[j] = true, true
						}
					}
				}
			}
			for i := range pos {
				pos[i][0] += delta[i][0]
				pos[i][1] += delta[i][1]
			}
			// The springs work the early steps, while the
			// layout is still finding its shape; the last
			// steps are pure spacing, so a crowded core
			// finishes resolving instead of fighting the
			// pull home.
			settled := step >= steps-steps/3
			if !settled {
				for i := range pos {
					if crowded[i] {
						continue
					}
					pos[i][0] += (anchor[i][0] - pos[i][0]) * projHomePull
					pos[i][1] += (anchor[i][1] - pos[i][1]) * projHomePull
				}
			}
			for _, g := range groupOrder {
				idxs := members[g]
				if len(idxs) < 2 || settled {
					continue
				}
				var mx, my float64
				for _, i := range idxs {
					mx += pos[i][0] - anchor[i][0]
					my += pos[i][1] - anchor[i][1]
				}
				mx /= float64(len(idxs))
				my /= float64(len(idxs))
				for _, i := range idxs {
					pos[i][0] += (anchor[i][0] + mx - pos[i][0]) * projDriftShare
					pos[i][1] += (anchor[i][1] + my - pos[i][1]) * projDriftShare
				}
			}
			for i := range pos {
				dx, dy := pos[i][0]-anchor[i][0], pos[i][1]-anchor[i][1]
				if d := math.Hypot(dx, dy); d > caps[i] {
					pos[i][0] = anchor[i][0] + dx/d*caps[i]
					pos[i][1] = anchor[i][1] + dy/d*caps[i]
				}
			}
		}
	}

	solve(projSteps)
	// Budget relief: a pile the estimate under-budgeted still
	// crowds when the solve settles. Grow the budgets of the
	// systems in such pairs and solve again, a few times at
	// most; everywhere else the original budgets stand.
	for relief := 0; relief < projReliefs; relief++ {
		stuck := crowdedSystems(pos, minD, projStuckFrac)
		if len(stuck) == 0 {
			break
		}
		for _, i := range stuck {
			caps[i] *= projReliefGrow
			if ceil := projCapCeiling * universeFrame; caps[i] > ceil {
				caps[i] = ceil
			}
		}
		solve(projSteps / 3)
	}
	return fitFrame(pos, universeFrame)
}

// crowdedSystems returns the systems still in a pair closer
// than frac*minD, in index order.
func crowdedSystems(pos [][2]float64, minD, frac float64) []int {
	n := len(pos)
	limit := frac * minD
	type cellKey [2]int
	buckets := make(map[cellKey][]int, n*2)
	for i := range pos {
		k := cellKey{int(math.Floor(pos[i][0] / minD)), int(math.Floor(pos[i][1] / minD))}
		buckets[k] = append(buckets[k], i)
	}
	stuck := make([]bool, n)
	for i := 0; i < n; i++ {
		ci, cj := int(math.Floor(pos[i][0]/minD)), int(math.Floor(pos[i][1]/minD))
		for gx := ci - 1; gx <= ci+1; gx++ {
			for gy := cj - 1; gy <= cj+1; gy++ {
				for _, j := range buckets[cellKey{gx, gy}] {
					if j <= i {
						continue
					}
					dx, dy := pos[j][0]-pos[i][0], pos[j][1]-pos[i][1]
					if math.Hypot(dx, dy) < limit {
						stuck[i], stuck[j] = true, true
					}
				}
			}
		}
	}
	var out []int
	for i, s := range stuck {
		if s {
			out = append(out, i)
		}
	}
	return out
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
