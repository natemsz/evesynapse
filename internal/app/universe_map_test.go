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
	groups := make([]int, len(pts))
	for i := range groups {
		if i < 300 {
			groups[i] = 0 // the core is one constellation
		} else {
			groups[i] = 1 // the rim is another
		}
	}
	a := layoutProjection(pts, groups)
	b := layoutProjection(pts, groups)
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

// TestLayoutProjectionBudgetsAndLocality: in the anchored
// projection no system travels beyond its displacement budget
// (relief growth included), a system alone in a void does not
// move toward a distant cluster, and the void stays void.
func TestLayoutProjectionBudgetsAndLocality(t *testing.T) {
	// Corner posts pin the frame, so the layout's final refit
	// is the identity and output displacement is the solver's
	// own. They are far from everything and never move.
	pts := [][2]float64{{0, 0}, {10000, 0}, {0, 10000}, {10000, 10000}}
	groups := []int{-1, -1, -1, -1}
	// A dense cluster: 60 systems in a 90-unit patch.
	for i := 0; i < 60; i++ {
		pts = append(pts, [2]float64{3000 + float64(i%8)*12, 3000 + float64(i/8)*12})
		groups = append(groups, 0)
	}
	// One system alone in the void, thousands of units away.
	lone := len(pts)
	pts = append(pts, [2]float64{7200, 6400})
	groups = append(groups, -1)

	out := layoutProjection(pts, groups)
	out2 := layoutProjection(pts, groups)
	minD := universeMinSpacing(len(pts))
	budgets := projBudgets(pts, minD)
	maxBudget := func(i int) float64 {
		b := budgets[i]
		for r := 0; r < projReliefs; r++ {
			b *= projReliefGrow
		}
		if c := projCapCeiling * universeFrame; b > c {
			b = c
		}
		return b
	}
	for i := range pts {
		if out[i] != out2[i] {
			t.Fatalf("projection is not deterministic at %d", i)
		}
		d := math.Hypot(out[i][0]-pts[i][0], out[i][1]-pts[i][1])
		if d > maxBudget(i)+1e-6 {
			t.Errorf("system %d moved %.1f, beyond its budget %.1f (relief included)", i, d, maxBudget(i))
		}
	}
	if d := math.Hypot(out[lone][0]-pts[lone][0], out[lone][1]-pts[lone][1]); d > 0.2*minD {
		t.Errorf("void system moved %.1f toward the cluster; want it home (<= %.1f)", d, 0.2*minD)
	}
	nearest := math.Inf(1)
	for i := range pts {
		if i == lone {
			continue
		}
		nearest = math.Min(nearest, math.Hypot(out[i][0]-out[lone][0], out[i][1]-out[lone][1]))
	}
	if nearest < 2500 {
		t.Errorf("the void filled in: lone system's nearest neighbour is %.0f away", nearest)
	}
	// The cluster itself still declutters.
	worst := math.Inf(1)
	for i := 4; i < lone; i++ {
		for j := i + 1; j < lone; j++ {
			worst = math.Min(worst, math.Hypot(out[i][0]-out[j][0], out[i][1]-out[j][1]))
		}
	}
	if worst < 0.6*minD {
		t.Errorf("cluster's closest pair %.1f, want at least %.1f", worst, 0.6*minD)
	}
}

// TestLayoutProjectionConstellationCohesion: a constellation's
// projected shape stays recognisably its true one -- its spread
// grows only as much as its crowding demands, its centroid
// stays in the neighbourhood, and its internal distances hold.
func TestLayoutProjectionConstellationCohesion(t *testing.T) {
	var pts [][2]float64
	var groups []int
	// Corner posts pin the frame (see the budgets test).
	pts = append(pts, [2]float64{0, 0}, [2]float64{10000, 0}, [2]float64{0, 10000}, [2]float64{10000, 10000})
	groups = append(groups, -1, -1, -1, -1)
	// A sparse background grid the constellations sit in.
	for gx := 0; gx < 20; gx++ {
		for gy := 0; gy < 20; gy++ {
			pts = append(pts, [2]float64{float64(gx)*500 + 250, float64(gy)*500 + 250})
			groups = append(groups, -1)
		}
	}
	// Constellation A: a centre and a ring, mildly crowded.
	aStart := len(pts)
	pts = append(pts, [2]float64{3500, 3500})
	groups = append(groups, 0)
	for k := 0; k < 6; k++ {
		a := float64(k) * math.Pi / 3
		pts = append(pts, [2]float64{3500 + 95*math.Cos(a), 3500 + 95*math.Sin(a)})
		groups = append(groups, 0)
	}
	// Constellation B: a bent chain, the shape a real
	// constellation's gate path makes.
	bStart := len(pts)
	for k := 0; k < 7; k++ {
		pts = append(pts, [2]float64{6400 + float64(k)*68, 6100 + float64(k*k)*9})
		groups = append(groups, 1)
	}

	out := layoutProjection(pts, groups)
	rms := func(src [][2]float64, from, to int) (float64, [2]float64) {
		var c [2]float64
		for i := from; i < to; i++ {
			c[0] += src[i][0]
			c[1] += src[i][1]
		}
		n := float64(to - from)
		c[0] /= n
		c[1] /= n
		var s float64
		for i := from; i < to; i++ {
			s += (src[i][0]-c[0])*(src[i][0]-c[0]) + (src[i][1]-c[1])*(src[i][1]-c[1])
		}
		return math.Sqrt(s / n), c
	}
	for _, tc := range []struct {
		name     string
		from, to int
	}{
		{"ring", aStart, bStart},
		{"chain", bStart, len(pts)},
	} {
		rt, ct := rms(pts, tc.from, tc.to)
		rl, cl := rms(out, tc.from, tc.to)
		if ratio := rl / rt; ratio < 0.75 || ratio > 1.8 {
			t.Errorf("%s constellation spread ratio %.2f, want its shape kept (0.75..1.8)", tc.name, ratio)
		}
		if d := math.Hypot(cl[0]-ct[0], cl[1]-ct[1]); d > 150 {
			t.Errorf("%s constellation centroid moved %.0f, want it in its neighbourhood (<= 150)", tc.name, d)
		}
	}
	// The chain's internal distances hold: its bent shape is
	// the landmark, not just its size.
	var errs []float64
	for i := bStart; i < len(pts); i++ {
		for j := i + 1; j < len(pts); j++ {
			dt := math.Hypot(pts[i][0]-pts[j][0], pts[i][1]-pts[j][1])
			dl := math.Hypot(out[i][0]-out[j][0], out[i][1]-out[j][1])
			errs = append(errs, math.Abs(dl/dt-1))
		}
	}
	sort.Float64s(errs)
	if med := errs[len(errs)/2]; med > 0.35 {
		t.Errorf("chain's internal distances: median error %.2f, want its shape kept (<= 0.35)", med)
	}
}
