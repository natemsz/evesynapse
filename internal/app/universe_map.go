package app

// ---------------------------------------------------------------------------
// The universe map (schema 072): the payload the map page draws,
// built from the SDE's geometry once per import and stored, never
// computed at render.
//
// Two layouts ride in one payload. The universe view is the true
// top-down picture: every known-space system at its SDE x and z
// (z flipped so galactic north is up), scaled into a 0..10000 frame.
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
const universeMapVersion = 1

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
	// systems' centroid). W, H: its region-view frame.
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
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
	Sec    []float64 `json:"s"`  // true security, 3 decimals
	Region []int     `json:"r"`  // index into regions
	Cons   []int     `json:"c"`  // index into cons (-1: unknown)
	RX     []int     `json:"rx"` // region frame
	RY     []int     `json:"ry"` // region frame
	// Faction is the owning faction's ID (0: none): the
	// Faction Warfare and sovereignty layers key on it.
	Faction []int64 `json:"f"`
}

// buildUniverseMap lays out the payload from the SDE rows.
func buildUniverseMap(systems []db.ListMapSystemsRow, regions []db.SdeRegion, cons []db.SdeConstellation, jumps []db.ListMapJumpsRow) universeMapPayload {
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
	S.Sec, S.Region, S.Cons = make([]float64, n), make([]int, n), make([]int, n)
	S.RX, S.RY, S.Faction = make([]int, n), make([]int, n), make([]int64, n)

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
	started := time.Now()
	payload := buildUniverseMap(systems, regions, cons, jumps)
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
