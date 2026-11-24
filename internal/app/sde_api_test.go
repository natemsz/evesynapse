package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sdeAPIFixture is the smallest set of SDE API tables an import
// accepts: every required table, one row or two each.
var sdeAPIFixture = map[string]string{
	"types": `{"typeID":587,"groupID":25,"marketGroupID":64,"name":{"en":"Rifter","de":"Rifter"},"published":true,"mass":1067000,"volume":27289,"capacity":140,"portionSize":1,"metaGroupID":1}
{"typeID":3300,"groupID":255,"name":{"en":"Gunnery"},"published":true,"portionSize":1}`,
	"groups":            `{"groupID":25,"categoryID":6,"name":{"en":"Frigate"}}` + "\n" + `{"groupID":255,"categoryID":16,"name":{"en":"Gunnery"}}`,
	"categories":        `{"categoryID":6,"name":{"en":"Ship"}}` + "\n" + `{"categoryID":16,"name":{"en":"Skill"}}`,
	"marketGroups":      `{"marketGroupID":64,"hasTypes":true,"name":{"en":"Standard Frigates"}}`,
	"typeMaterials":     `{"typeID":587,"materials":[{"materialTypeID":34,"quantity":100}]}`,
	"npcStations":       `{"stationID":60000001,"orbitID":40000004,"ownerID":1000002,"operationID":26,"solarSystemID":30000001,"useOperationName":true}`,
	"npcCorporations":   `{"corporationID":1000002,"factionID":500001,"name":{"en":"CBD Corporation"}}`,
	"stationOperations": `{"operationID":26,"operationName":{"en":"Storage"}}`,
	"mapSolarSystems":   `{"solarSystemID":30000001,"name":{"en":"Tanoo"},"regionID":10000001,"constellationID":20000001,"securityStatus":0.85,"securityClass":"B","position":{"x":1,"y":2,"z":3},"position2D":{"x":4,"y":5}}`,
	"mapRegions":        `{"regionID":10000001,"factionID":500007,"name":{"en":"Derelik"}}`,
	"mapPlanets":        `{"planetID":40000002,"solarSystemID":30000001,"celestialIndex":4,"typeID":11,"radius":5060000}`,
	"mapMoons":          `{"moonID":40000004,"orbitID":40000002,"orbitIndex":1,"solarSystemID":30000001,"typeID":14,"radius":270000}`,
	"blueprints":        `{"blueprintTypeID":681,"maxProductionLimit":300,"activities":{"manufacturing":{"time":600,"materials":[{"typeID":38,"quantity":86}],"products":[{"typeID":165,"quantity":1}]}}}`,
	"typeDogma": `{"typeID":587,"dogmaAttributes":[{"attributeID":182,"value":3300},{"attributeID":277,"value":1}],"dogmaEffects":[{"effectID":11,"isDefault":true}]}
{"typeID":3300,"dogmaAttributes":[{"attributeID":275,"value":1},{"attributeID":180,"value":165},{"attributeID":181,"value":166}]}`,
	"dogmaAttributes": `{"attributeID":275,"name":"skillTimeConstant","stackable":true,"highIsGood":true,"defaultValue":0,"published":true}`,
	"dogmaEffects":    `{"effectID":11,"name":"loPower","effectCategoryID":0,"modifierInfo":[{"domain":"shipID","func":"ItemModifier","modifiedAttributeID":9,"modifyingAttributeID":10,"operation":6}]}`,
}

// stubSDEAPI serves a release of the fixture tables, each under a
// version, and counts the table downloads.
type stubSDEAPI struct {
	mu        sync.Mutex
	tables    map[string]string // table -> body
	versions  map[string]string // table -> version
	release   string
	downloads map[string]int // table -> downloads
	srv       *httptest.Server
}

func newStubSDEAPI(t *testing.T) *stubSDEAPI {
	s := &stubSDEAPI{tables: map[string]string{}, versions: map[string]string{}, downloads: map[string]int{}, release: "20261120T165826Z"}
	for name, body := range sdeAPIFixture {
		s.tables[name] = body + "\n"
		s.versions[name] = "v1-" + name
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubSDEAPI) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/tranquility/latest/" {
		tables := map[string]any{}
		for name, v := range s.versions {
			tables[name] = map[string]any{"version": v, "jsonl": map[string]string{"url": s.srv.URL + "/blobs/" + name + "/" + v + ".jsonl"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"release": s.release, "buildNumber": 3724492, "releaseDate": "2026-11-20T16:58:26Z", "tables": tables})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/blobs/"), "/")
	if len(parts) != 2 || s.versions[parts[0]]+".jsonl" != parts[1] {
		http.NotFound(w, r)
		return
	}
	s.downloads[parts[0]]++
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(s.tables[parts[0]]))
	_ = zw.Close()
	w.Header().Set("ETag", `"`+s.versions[parts[0]]+`"`)
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(gz.Bytes())
}

// TestSDEAPIImportDownloadsOnlyChangedTables pins the SDE API import:
// a first import reads every table and stores the rows, the versions
// and a copy of each table; a later release that changes one table
// downloads only that one, reads the rest from the kept copies, and
// stores the change; and once imported, a release counts as unchanged.
func TestSDEAPIImportDownloadsOnlyChangedTables(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	api := newStubSDEAPI(t)
	app.cfg.sdeAPIURL = api.srv.URL + "/tranquility"

	stationName := func() string {
		t.Helper()
		var name string
		if err := conn.QueryRowContext(ctx, "SELECT name FROM sde_stations WHERE station_id = 60000001").Scan(&name); err != nil {
			t.Fatalf("read station: %v", err)
		}
		return name
	}

	if err := app.importSDEFrom(ctx, nil); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if got, want := stationName(), "Tanoo IV - Moon 1 - CBD Corporation Storage"; got != want {
		t.Fatalf("station name = %q, want %q", got, want)
	}
	var kept int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sde_api_blobs").Scan(&kept); err != nil || kept != len(sdeAPIFixture) {
		t.Fatalf("kept copies = %d (%v), want %d", kept, err, len(sdeAPIFixture))
	}
	rel, err := app.fetchSDERelease(ctx)
	if err != nil {
		t.Fatalf("fetch release: %v", err)
	}
	if changed, err := app.sdeAPIChangedTables(ctx, rel); err != nil || len(changed) != 0 {
		t.Fatalf("changed after import = %v (%v), want none", changed, err)
	}

	api.mu.Lock()
	api.release = "20261121T110000Z"
	api.tables["mapSolarSystems"] = strings.Replace(api.tables["mapSolarSystems"], `"Tanoo"`, `"Tanoo Prime"`, 1)
	api.versions["mapSolarSystems"] = "v2-mapSolarSystems"
	api.mu.Unlock()

	rel, err = app.fetchSDERelease(ctx)
	if err != nil {
		t.Fatalf("fetch release: %v", err)
	}
	if changed, err := app.sdeAPIChangedTables(ctx, rel); err != nil || len(changed) != 1 || changed[0] != "mapSolarSystems" {
		t.Fatalf("changed = %v (%v), want [mapSolarSystems]", changed, err)
	}
	if err := app.importSDEFrom(ctx, nil); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if got, want := stationName(), "Tanoo Prime IV - Moon 1 - CBD Corporation Storage"; got != want {
		t.Fatalf("station name after the change = %q, want %q", got, want)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for name, n := range api.downloads {
		want := 1
		if name == "mapSolarSystems" {
			want = 2
		}
		if n != want {
			t.Errorf("%s downloaded %d times, want %d", name, n, want)
		}
	}
}
