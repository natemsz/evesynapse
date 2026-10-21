package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestMapLayersBuildAndServe: the worker folds the stored public
// snapshots into one layers payload -- kills, jumps, sovereignty with
// named holders, Faction Warfare progress, incursions -- rebuilds it
// only when a source moved, and the page and endpoint serve it.
func TestMapLayersBuildAndServe(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	fetched := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	put := func(kind, payload string) {
		t.Helper()
		if err := q.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
			Kind: kind, Payload: payload, FetchedAt: fetched, CachedUntil: fetched.Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}
	put(esi.GlobalSystemKills, `[{"system_id":30002187,"ship_kills":4,"pod_kills":2,"npc_kills":0},
		{"system_id":30000142,"ship_kills":0,"pod_kills":0,"npc_kills":0},
		{"system_id":30004759,"ship_kills":1,"pod_kills":0,"npc_kills":350}]`)
	put(esi.GlobalSystemJumps, `[{"system_id":30000142,"ship_jumps":3100},{"system_id":30002187,"ship_jumps":0}]`)
	put(esi.GlobalSovMap, `[{"system_id":30004759,"alliance_id":99003581,"corporation_id":1},
		{"system_id":30004760,"alliance_id":99003581,"corporation_id":1},
		{"system_id":30000142,"faction_id":500001},
		{"system_id":30001000}]`)
	put(esi.GlobalFWSystems, `[{"solar_system_id":30002813,"occupier_faction_id":500001,"owner_faction_id":500004,
		"contested":"contested","victory_points":3000,"victory_points_threshold":75000}]`)
	put(esi.GlobalIncursions, `[{"constellation_id":20000001,"faction_id":500019,"has_boss":true,
		"infested_solar_systems":[30000001,30000002],"influence":0.4,"staging_solar_system_id":30000001,
		"state":"established","type":"Incursion"}]`)
	put(esi.GlobalFactions, `[{"faction_id":500001,"name":"Caldari State"}]`)
	app.esi.StoreAllianceName(99003581, "Fraternity.")

	app.refreshMapLayers(ctx)
	meta, err := q.GetMapLayersMeta(ctx)
	if err != nil {
		t.Fatalf("no layers built: %v", err)
	}
	// Nothing moved: no rebuild.
	app.refreshMapLayers(ctx)
	if again, _ := q.GetMapLayersMeta(ctx); !again.BuiltAt.Equal(meta.BuiltAt) {
		t.Errorf("an unchanged source must not rebuild the layers")
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	rec := get("/map/layers.json?v=" + meta.Etag)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("layers: %d %v", rec.Code, rec.Header())
	}
	var p mapLayersPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Kills.ID) != 2 || p.Kills.At != fetched.Unix() {
		t.Errorf("kills keep only systems with any: %+v", p.Kills)
	}
	if len(p.Jumps.ID) != 1 || p.Jumps.N[0] != 3100 {
		t.Errorf("jumps: %+v", p.Jumps)
	}
	if len(p.Alliances) != 1 || p.Alliances[0].Name != "Fraternity." || len(p.Sov.ID) != 3 {
		t.Errorf("sov: one named holder over two systems, one faction system: %+v %+v", p.Alliances, p.Sov)
	}
	if len(p.FW.ID) != 1 || p.FW.Pct[0] != 4 || p.FW.State[0] != "contested" {
		t.Errorf("fw: %+v", p.FW)
	}
	if len(p.Inc.List) != 1 || p.Inc.List[0].Staging != 30000001 || !p.Inc.List[0].Boss {
		t.Errorf("incursions: %+v", p.Inc)
	}

	// A source moving rebuilds, and the alliance name resolving
	// later counts as a move too.
	fetched = fetched.Add(time.Hour)
	put(esi.GlobalSystemJumps, `[{"system_id":30000142,"ship_jumps":2900}]`)
	app.refreshMapLayers(ctx)
	if moved, _ := q.GetMapLayersMeta(ctx); moved.Etag == meta.Etag {
		t.Errorf("a moved source must rebuild the layers")
	}
}

// TestMapPageNamesLayers: the map page hands map.js the layers'
// hashed URL once they exist, and the picker renders either way.
func TestMapPageNamesLayers(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	page := func() string {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/map/", nil))
		return rec.Body.String()
	}
	if body := page(); strings.Contains(body, "data-layers=") || !strings.Contains(body, `data-map-layer="sov"`) {
		t.Errorf("before layers: no URL, picker present")
	}
	if err := q.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
		Kind: esi.GlobalSystemJumps, Payload: `[{"system_id":30000142,"ship_jumps":10}]`,
		FetchedAt: time.Now().UTC(), CachedUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	app.refreshMapLayers(ctx)
	meta, err := q.GetMapLayersMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if body := page(); !strings.Contains(body, `data-layers="/map/layers.json?v=`+meta.Etag+`"`) {
		t.Errorf("the page must name the layers' hashed URL")
	}
}
