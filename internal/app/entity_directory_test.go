package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestEntityDirectoryMemoryPinsServingAndLowRAM pins the in-memory entity
// resolution directory (entity_directory.go):
//  1. Before any snapshot lands, lookups return false.
//  2. Once built, pilots, corps, alliances, and structures serve from RAM.
//  3. Live stores update the resident snapshot immediately.
//  4. LOW_RAM_MODE=1 returns nil snapshot and starts no background builds.
func TestEntityDirectoryMemoryPinsServingAndLowRAM(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()

	now := time.Now().UTC()
	fetched := timeSet(now)

	pilotRaw, _ := json.Marshal(pilotPayload{Profile: esi.Character{Name: "Nate Hunter"}})
	corpRaw, _ := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Hunter Corp", Ticker: "HNTR"}})
	alliRaw, _ := json.Marshal(allianceRecordPayload{Alliance: esi.Alliance{Name: "Hunter Alliance", Ticker: "HNTA"}})

	// Seed pilot, corp, alliance records, and structure names.
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: 93300001, Payload: string(pilotRaw), State: pilotStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("set pilot: %v", err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000001, Payload: string(corpRaw), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("set corp: %v", err)
	}
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 99000001, Payload: string(alliRaw), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("set alliance: %v", err)
	}
	if !app.storeStructureName(ctx, 1030000000001, "Perimeter - Tranquility Trading Tower", esi.StructureResolved, esi.StructureSourceCorp, now) {
		t.Fatalf("store structure failed")
	}

	// 1. Before snapshot builds: nil snapshot.
	app.entityDir.mu.Lock()
	app.entityDir.snap = nil
	app.entityDir.mu.Unlock()

	if _, ok := app.pilotName(93300001); ok {
		t.Errorf("pilotName before build returned ok = true")
	}

	// 2. Build snapshot: memory serves.
	if err := app.buildEntityDirectory(ctx); err != nil {
		t.Fatalf("buildEntityDirectory: %v", err)
	}

	pName, ok := app.pilotName(93300001)
	if !ok || pName != "Nate Hunter" {
		t.Fatalf("pilotName = (%s, %v), want (Nate Hunter, true)", pName, ok)
	}
	cName, ok := app.corpName(98000001)
	if !ok || cName != "Hunter Corp" {
		t.Fatalf("corpName = (%s, %v), want (Hunter Corp, true)", cName, ok)
	}
	aName, ok := app.allianceName(99000001)
	if !ok || aName != "Hunter Alliance" {
		t.Fatalf("allianceName = (%s, %v), want (Hunter Alliance, true)", aName, ok)
	}
	sName, ok := app.structureName(1030000000001)
	if !ok || sName != "Perimeter - Tranquility Trading Tower" {
		t.Fatalf("structureName = (%s, %v), want (Perimeter - Tranquility Trading Tower, true)", sName, ok)
	}

	// killNames uses entity directory in RAM.
	names := app.newKillNames(ctx)
	if got := names.character(93300001); got != "Nate Hunter" {
		t.Errorf("names.character = %s, want Nate Hunter", got)
	}
	if got := names.corp(98000001); got != "Hunter Corp" {
		t.Errorf("names.corp = %s, want Hunter Corp", got)
	}
	if got := names.alliance(99000001); got != "Hunter Alliance" {
		t.Errorf("names.alliance = %s, want Hunter Alliance", got)
	}

	// 3. Live store updates resident snapshot immediately.
	pilot2Raw, _ := json.Marshal(pilotPayload{Profile: esi.Character{Name: "Second Hunter"}})
	if err := app.setPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: 93300002, Payload: string(pilot2Raw), State: pilotStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("set pilot 2: %v", err)
	}
	pName2, ok := app.pilotName(93300002)
	if !ok || pName2 != "Second Hunter" {
		t.Fatalf("pilotName 2 after live store = (%s, %v), want (Second Hunter, true)", pName2, ok)
	}

	// 4. Low-RAM mode: returns nil and starts no builds.
	app.cfg.lowRAMMode = true
	if got := app.entityDirectorySnapshot(); got != nil {
		t.Errorf("entityDirectorySnapshot in low-RAM mode = %v, want nil", got)
	}
	app.kickEntityDirectoryBuild()
	app.entityDir.mu.RLock()
	building := app.entityDir.building
	app.entityDir.mu.RUnlock()
	if building {
		t.Errorf("kickEntityDirectoryBuild started a build in low-RAM mode")
	}
	app.cfg.lowRAMMode = false
}
