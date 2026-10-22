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

// TestLayoutProjectionDeclutters: the Improved Projection takes
// piled-up systems to readable spacing across the whole universe
// frame, keeps everyone near home, and is the same on every build.
func TestLayoutProjectionDeclutters(t *testing.T) {
	// A dense core (a few hundred systems in a tight patch, the
	// empire problem), a sparse rim, all in the universe frame.
	var pts [][2]float64
	for i := 0; i < 300; i++ {
		pts = append(pts, [2]float64{5000 + float64(i%20)*3, 5000 + float64(i/20)*3})
	}
	for i := 0; i < 60; i++ {
		a := float64(i) / 60 * 2 * math.Pi
		pts = append(pts, [2]float64{5000 + math.Cos(a)*4200, 5000 + math.Sin(a)*4200})
	}
	var edges [][2]int
	for i := 1; i < len(pts); i++ {
		edges = append(edges, [2]int{i - 1, i})
	}
	a := layoutProjection(pts, edges)
	b := layoutProjection(pts, edges)
	minD := universeMinSpacing(len(pts))
	worst := math.Inf(1)
	moveSum, moveMax := 0.0, 0.0
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("projection is not deterministic at %d: %v vs %v", i, a[i], b[i])
		}
		if a[i][0] < -1e-6 || a[i][1] < -1e-6 || a[i][0] > universeFrame+1e-6 || a[i][1] > universeFrame+1e-6 {
			t.Errorf("system %d left the frame: %v", i, a[i])
		}
		// Near home is measured against the input as given; the
		// layout's final refit only makes this stricter here,
		// since the test patch does not fill the frame.
		moveSum += math.Hypot(a[i][0]-pts[i][0], a[i][1]-pts[i][1])
		moveMax = math.Max(moveMax, math.Hypot(a[i][0]-pts[i][0], a[i][1]-pts[i][1]))
		for j := i + 1; j < len(a); j++ {
			worst = math.Min(worst, math.Hypot(a[i][0]-a[j][0], a[i][1]-a[j][1]))
		}
	}
	if worst < 0.6*minD {
		t.Errorf("closest pair %.1f apart, want at least %.1f (minimum spacing %.1f)", worst, 0.6*minD, minD)
	}
	// Near home: the core above starts at 3-unit spacing and
	// must expand ~30x just to reach spacing, so its mean move
	// is necessarily a few percent of the frame. Anything
	// beyond 8% would mean the layout, not the crowding, is
	// deciding where systems live.
	if mean := moveSum / float64(len(pts)); mean > 800 {
		t.Errorf("mean move %.0f, want everyone near home (<= 800 of a %.0f frame)", mean, universeFrame)
	}
	if moveMax > 2500 {
		t.Errorf("worst move %.0f, want no system flung (<= 2500)", moveMax)
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
	p := buildUniverseMap(systems, regions, cons, jumps)
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
