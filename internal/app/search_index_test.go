package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestSearchIndexMatchesSQLPath pins the search index to the SQL
// path it replaces: with the same fixtures, the index answers
// every suggestion pool and every search tier exactly as the SQL
// queries do. The one deliberate difference is asserted at the
// end: past the SQL record window (the first 100 matches by id),
// the index ranks every match instead of the shortlist, so the
// best names win for very common text.
func TestSearchIndexMatchesSQLPath(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	fetched := mustNullTime("2026-10-01T00:00:00Z")

	seedNext1SDE(t, conn)
	stmts := []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000070, 'Parity Reach'), (10000071, 'New Parity Region')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		   (30005001, 'Parity Prime', 10000070, 0.9),
		   (30005002, 'Parity', 10000070, 0.5),
		   (30005003, 'New Parity', 10000071, -0.2)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed map fixtures: %v", err)
		}
	}

	seedPilot := func(id int64, payload pilotPayload) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
			CharacterID: id, Payload: string(raw), State: pilotStateReady, FetchedAt: fetched,
		}); err != nil {
			t.Fatalf("seed pilot record: %v", err)
		}
	}
	seedPilot(91000101, pilotPayload{
		Profile: esi.Character{Name: "Parity Pilot One"},
		Corp:    esi.Corporation{Name: "Parity Holdings"},
	})
	seedPilot(91000102, pilotPayload{Profile: esi.Character{Name: "New Parity Pilot"}})

	// Both stored corporation shapes: the wrapped record and a
	// bare ESI corporation, as the swarm path stores them.
	wrapped, err := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Parity Holdings", Ticker: "PHLD"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 92000101, Payload: string(wrapped), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed wrapped corp record: %v", err)
	}
	bare, err := json.Marshal(esi.Corporation{Name: "New Parity Corp", Ticker: "NPC"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 92000102, Payload: string(bare), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed bare corp record: %v", err)
	}
	wrappedAl, err := json.Marshal(allianceRecordPayload{Alliance: esi.Alliance{Name: "Parity Alliance", Ticker: "PAL"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 93000101, Payload: string(wrappedAl), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed wrapped alliance record: %v", err)
	}
	bareAl, err := json.Marshal(esi.Alliance{Name: "New Parity Alliance", Ticker: "NPA"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 93000102, Payload: string(bareAl), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed bare alliance record: %v", err)
	}

	// The SQL path first: no snapshot exists and this app has no
	// worker context, so nothing builds in the background.
	sqlPools := map[string][]suggestItem{}
	for _, pool := range []string{suggestPoolAll, suggestPoolMarket, suggestPoolPlanner, suggestPoolSkills} {
		sqlPools[pool] = app.suggestTypes(ctx, "trit", pool, 12)
		if len(sqlPools[pool]) == 0 {
			t.Fatalf("suggestTypes pool %q: SQL path returned no hits; the parity check would be empty", pool)
		}
	}
	sqlSet := app.searchAll(ctx, "parity", 0, topbarLimits)
	if len(sqlSet.Systems) == 0 || len(sqlSet.Regions) == 0 || len(sqlSet.Corporations) == 0 ||
		len(sqlSet.Alliances) == 0 || len(sqlSet.Pilots) == 0 {
		t.Fatalf("SQL search set is missing a tier: %+v", sqlSet)
	}

	if err := app.buildSearchIndex(ctx); err != nil {
		t.Fatalf("build search index: %v", err)
	}
	for pool, want := range sqlPools {
		if got := app.suggestTypes(ctx, "trit", pool, 12); !reflect.DeepEqual(got, want) {
			t.Errorf("suggestTypes pool %q: index = %+v, SQL = %+v", pool, got, want)
		}
	}
	if got := app.searchAll(ctx, "parity", 0, topbarLimits); !reflect.DeepEqual(got, sqlSet) {
		t.Errorf("searchAll: index = %+v, SQL = %+v", got, sqlSet)
	}

	// Past the SQL window: 150 more matching pilots, ids rising
	// as names fall, so the first 100 by id hold the worst names.
	for i := 0; i < 150; i++ {
		seedPilot(94000000+int64(i), pilotPayload{
			Profile: esi.Character{Name: fmt.Sprintf("Parity Pilot %03d", 150-i)},
		})
	}
	app.searchIdx.mu.Lock()
	app.searchIdx.snap = nil
	app.searchIdx.mu.Unlock()
	sqlWide := app.searchAll(ctx, "parity", 0, searchPageLimits)
	if len(sqlWide.Pilots) != 60 || sqlWide.Pilots[0].Name != "Parity Pilot 053" {
		t.Fatalf("SQL pilots past the window = %d hits, first %v; want 60 hits starting at Parity Pilot 053 (the first-100-by-id shortlist, ranked)",
			len(sqlWide.Pilots), sqlWide.Pilots)
	}
	if err := app.buildSearchIndex(ctx); err != nil {
		t.Fatalf("rebuild search index: %v", err)
	}
	idxWide := app.searchAll(ctx, "parity", 0, searchPageLimits)
	if len(idxWide.Pilots) != 60 || idxWide.Pilots[0].Name != "Parity Pilot 001" || idxWide.Pilots[59].Name != "Parity Pilot 060" {
		t.Errorf("index pilots past the window = %+v; want 60 hits running Parity Pilot 001 to Parity Pilot 060 (every match ranked)",
			idxWide.Pilots)
	}
}
