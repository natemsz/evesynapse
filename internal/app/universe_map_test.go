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
// mosaic of the regions' own layouts -- inside a region, every
// pairwise distance is the region layout's distance under one
// ratio; across regions, the centroids keep their true bearings;
// the dense cores arrive already decluttered by their region
// layouts; and the picture is the same on every build.
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

	a := layoutMosaic(uni, members, regionLaid)
	b := layoutMosaic(uni, members, regionLaid)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mosaic is not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
		if a[i][0] < -1e-6 || a[i][1] < -1e-6 || a[i][0] > universeFrame+1e-6 || a[i][1] > universeFrame+1e-6 {
			t.Errorf("system %d left the frame: %v", i, a[i])
		}
	}

	// One ratio per region: every intra-region distance in the
	// mosaic is the region layout's distance times one constant.
	for ri, idxs := range members {
		laid := regionLaid[ri]
		lo, hi := math.Inf(1), math.Inf(-1)
		for x := 0; x < len(idxs); x++ {
			for y := x + 1; y < len(idxs); y++ {
				ld := math.Hypot(laid[x][0]-laid[y][0], laid[x][1]-laid[y][1])
				if ld < 1 {
					continue
				}
				md := math.Hypot(a[idxs[x]][0]-a[idxs[y]][0], a[idxs[x]][1]-a[idxs[y]][1])
				r := md / ld
				lo, hi = math.Min(lo, r), math.Max(hi, r)
			}
		}
		if hi > lo*1.000001 {
			t.Errorf("region %d: intra-region ratios run %.6f..%.6f, want one uniform scale", ri, lo, hi)
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
