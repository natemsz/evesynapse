package app

// The workbench's scan door: a standalone scan's stored reading
// drives the same page model a report's reading drives. The
// door is built but not routed — the first test exercises the
// builder and the handler directly, the second proves the real
// router does not serve it yet.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/scan"
)

// wbFailTransport answers every request with a 500: the scan
// door reads stored rows only and must never reach it.
type wbFailTransport struct{}

func (wbFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusInternalServerError,
		Header: http.Header{"Retry-After": []string{"0"}},
		Body:   io.NopCloser(strings.NewReader(`{"error":"must not be called"}`))}, nil
}

func seedWorkbenchScan(t *testing.T) (*Application, *db.Queries, db.GetScanRow) {
	t.Helper()
	app, conn, q := buildCorpTestApp(t, wbFailTransport{})
	ctx := context.Background()
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000060, 'Catch')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004759, '1DQ1-A', 10000060, -0.38)`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES (22456, 'Sabre', 420, 0, 1, 'Interdictor hull.')`)
	exec(`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		(22456, 12, 6), (22456, 13, 4), (22456, 14, 5), (22456, 1137, 3), (22456, 600, 2.5), (22456, 4, 16000000)`)
	now := time.Now().UTC()
	for _, p := range []struct {
		id, corp int64
		name     string
	}{{2124000401, 98000999, "Wb Pilot One"}, {2124000402, 98000777, "Wb Pilot Two"}} {
		if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{
			CharacterID: p.id, CorporationID: p.corp, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	res := scanResult{
		Kind: scan.KindDScan, SystemID: 30004759,
		DScan: &dscanReading{
			Items: 3, Ships: 3, ShipsGrid: 3,
			Types: []scanCount{
				{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 2, Grid: 2},
				{ID: 11987, Name: "Guardian", Class: "Logistics", N: 1, Grid: 1},
			},
			Classes: []scanCount{
				{Name: "Interdictor", N: 2, Grid: 2},
				{Name: "Logistics", N: 1, Grid: 1},
			},
		},
		Local: &localReading{
			Pilots: []localPilot{
				{ID: 2124000401, Name: "Wb Pilot One", CorpID: 98000999, AllianceID: 98000001},
				{ID: 2124000402, Name: "Wb Pilot Two", CorpID: 98000777},
			},
			Alliances: []localGroup{
				{ID: 98000001, Name: "Fixture Alliance", N: 1, Corps: []localGroup{{ID: 98000999, Name: "Fixture Corp", N: 1}}},
				{ID: 98000777, Name: "Fixture Corp Two", N: 1, Corp: true},
			},
		},
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	raw := "22456\tAlpha Sabre\tSabre\t850 m\n22456\tBeta Sabre\tSabre\t12 km\n11987\tLogi One\tGuardian\t9 km\n"
	if err := q.InsertScan(ctx, db.InsertScanParams{ID: "wbscan000009", Kind: "dscan", SystemID: 30004759,
		GroupID: "wbscan000009", Raw: raw, Result: string(body), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	row, err := q.GetScan(ctx, "wbscan000009")
	if err != nil {
		t.Fatal(err)
	}
	return app, q, row
}

func TestWorkbenchScanDoorBuilder(t *testing.T) {
	app, _, row := seedWorkbenchScan(t)
	v := app.buildWorkbenchScan(context.Background(), row)
	if !v.FromScan || v.ScanID != "wbscan000009" {
		t.Fatalf("view is not the scan door's: FromScan=%v ScanID=%q", v.FromScan, v.ScanID)
	}
	if v.SysName != "1DQ1-A" || !v.HasDScan || v.Hulls != 3 || v.PilotsN != 2 {
		t.Errorf("view from the stored reading: SysName=%q HasDScan=%v Hulls=%d PilotsN=%d",
			v.SysName, v.HasDScan, v.Hulls, v.PilotsN)
	}
	if len(v.Types) != 2 || v.Types[0].Name != "Sabre" {
		t.Fatalf("types from the reading: %+v", v.Types)
	}
	var sabre *wbHullDrawer
	for i := range v.HullDrawers {
		if v.HullDrawers[i].TypeID == 22456 {
			sabre = &v.HullDrawers[i]
		}
	}
	if sabre == nil {
		t.Fatal("no drawer for the Sabre")
	}
	if sabre.Slots != "5H / 4M / 6L / 3R" || sabre.Warp != "7.5 AU/s" {
		t.Errorf("drawer from stored attributes: slots %q warp %q", sabre.Slots, sabre.Warp)
	}
	if len(sabre.Instances) != 2 || sabre.Instances[0].Dist != "850 m" {
		t.Errorf("drawer instances from the paste: %+v", sabre.Instances)
	}

	// The handler renders the same page (mounted here only —
	// the shipped router does not mount it).
	r := chi.NewRouter()
	r.Use(app.sessions.LoadAndSave)
	r.Get("/intel/workbench/scan/{scanID}/", app.handleWorkbenchScan)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/intel/workbench/scan/wbscan000009/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scan door handler: status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "1DQ1-A") || !strings.Contains(body, "Sabre") {
		t.Errorf("scan door handler: body missing the reading's contents")
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/intel/workbench/scan/nope00000000/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("scan door handler, unknown scan: status %d, want 404", rec.Code)
	}
}

// TestWorkbenchScanDoorDark pins the shipped state the owner
// ordered: the scan door exists in the tree but the real router
// does not serve it — the scan tool keeps its own result page
// until the door is switched on.
func TestWorkbenchScanDoorDark(t *testing.T) {
	app, _, _ := seedWorkbenchScan(t)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/intel/workbench/scan/wbscan000009/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET scan door on the real router: status %d, want 404 (door built, not routed)", rec.Code)
	}
}
