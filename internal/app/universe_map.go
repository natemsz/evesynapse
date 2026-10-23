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
// Beside them sits the Improved Projection: the SDE's own
// authored 2D layout (position2D, schema 085), the positions the
// game's flat map draws, carried into the same frame by one
// global similarity (layoutProjected). The geometry is CCP's,
// published in the dump: nothing is re-solved, contracted or
// nudged here. A system whose dump row carries no position2D
// keeps its universe-frame position in the projection.
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
const universeMapVersion = 6

const (
	universeFrame = 10000.0 // the universe view's longest side
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
	// frame.
	X  int `json:"x"`
	Y  int `json:"y"`
	PX int `json:"px"`
	PY int `json:"py"`
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
	S.Faction = make([]int64, n)
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

	// Each region: its label point, the centroid of its systems
	// in the universe frame. (The projected frame's label point
	// is set with the projection below.)
	members := make([][]int, len(p.Regions))
	for i := range kept {
		members[S.Region[i]] = append(members[S.Region[i]], i)
	}
	for ri, idxs := range members {
		if len(idxs) == 0 {
			continue
		}
		cx, cy := 0.0, 0.0
		for _, i := range idxs {
			cx += float64(S.X[i])
			cy += float64(S.Y[i])
		}
		p.Regions[ri].X = int(math.Round(cx / float64(len(idxs))))
		p.Regions[ri].Y = int(math.Round(cy / float64(len(idxs))))
	}

	// The Improved Projection: the SDE's authored 2D layout
	// (position2D), carried into the universe frame by one
	// global similarity (layoutProjected), precomputed here
	// like every other layout and carried beside the true
	// positions, so the client only ever chooses between two
	// stored frames.
	auth := make([][2]float64, n)
	for i, s := range kept {
		if s.Position2dX != 0 || s.Position2dY != 0 {
			auth[i] = [2]float64{s.Position2dX, -s.Position2dY}
		} else {
			// No authored position: layoutProjected places
			// the system from its flat position instead.
			auth[i] = [2]float64{math.NaN(), math.NaN()}
		}
	}
	proj := layoutProjected(auth, uni)
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

// layoutProjected carries the authored 2D positions into the
// universe frame under one global similarity: a uniform scale
// and a translation, the frame fit of the authored points, so
// every pairwise distance in the projection is the authored
// distance times one constant. A system with no authored
// position (NaN in auth) is placed by the matching similarity
// between the flat positions and the authored ones: at the
// flat point carried through the two frame fits, so it lands
// where its flat position sits inside the authored picture.
// Systems the dump authors placed exactly on top of each other
// land together; the authored layout is drawn as published,
// not repaired. Deterministic: fixed passes over the points.
func layoutProjected(auth, flat [][2]float64) [][2]float64 {
	out := make([][2]float64, len(auth))
	present := make([][2]float64, 0, len(auth))
	for _, p := range auth {
		if !math.IsNaN(p[0]) {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		copy(out, flat)
		return out
	}
	fit := fitFrame(present, universeFrame)
	k := 0
	for i := range auth {
		if math.IsNaN(auth[i][0]) {
			out[i] = flat[i]
			continue
		}
		out[i] = fit[k]
		k++
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
