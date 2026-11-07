package app

import (
	"context"
	"testing"
)

// TestSDETopologyMemoryPinsServingAndLowRAM pins the in-memory SDE universe
// topology (sde_memory.go):
//  1. Before any snapshot lands, resolution falls back to SQL.
//  2. Once built, systems, regions, and types serve from RAM.
//  3. LOW_RAM_MODE=1 returns nil snapshot and starts no background builds.
//  4. Invalidation marks the snapshot stale.
func TestSDETopologyMemoryPinsServingAndLowRAM(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	// Seed one test system, region, and type into Postgres.
	if _, err := app.db.ExecContext(ctx, `
		INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')
		ON CONFLICT (region_id) DO UPDATE SET name = excluded.name;
		INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9459)
		ON CONFLICT (system_id) DO UPDATE SET name = excluded.name, region_id = excluded.region_id, security = excluded.security;
		INSERT INTO sde_types (type_id, name, published) VALUES (34, 'Tritanium', 1)
		ON CONFLICT (type_id) DO UPDATE SET name = excluded.name, published = excluded.published;
	`); err != nil {
		t.Fatalf("seed SDE: %v", err)
	}

	// 1. Before snapshot builds: SQL path serves.
	app.sdeTopology.mu.Lock()
	app.sdeTopology.snap = nil
	app.sdeTopology.mu.Unlock()

	secText, secTier, ok := app.SystemSecurity(ctx, 30000142)
	if !ok || secText != "0.9" || secTier != 9 {
		t.Fatalf("SystemSecurity (SQL) = (%s, %d, %v), want (0.9, 9, true)", secText, secTier, ok)
	}
	regName, ok := app.SystemRegion(ctx, 30000142)
	if !ok || regName != "The Forge" {
		t.Fatalf("SystemRegion (SQL) = (%s, %v), want (The Forge, true)", regName, ok)
	}

	// 2. Build snapshot: memory serves.
	if err := app.buildSDETopology(ctx); err != nil {
		t.Fatalf("buildSDETopology: %v", err)
	}
	sys, ok := app.sdeSystem(30000142)
	if !ok || sys.Name != "Jita" || sys.SecText != "0.9" || sys.RegionName != "The Forge" {
		t.Fatalf("sdeSystem = %+v, ok %v", sys, ok)
	}
	reg, ok := app.sdeRegion(10000002)
	if !ok || reg != "The Forge" {
		t.Fatalf("sdeRegion = %s, ok %v", reg, ok)
	}
	tName, ok := app.sdeTypeName(34)
	if !ok || tName != "Tritanium" {
		t.Fatalf("sdeTypeName = %s, ok %v", tName, ok)
	}

	// SystemSecurity and SystemRegion read from memory.
	secText, secTier, ok = app.SystemSecurity(ctx, 30000142)
	if !ok || secText != "0.9" || secTier != 9 {
		t.Fatalf("SystemSecurity (RAM) = (%s, %d, %v)", secText, secTier, ok)
	}
	regName, ok = app.SystemRegion(ctx, 30000142)
	if !ok || regName != "The Forge" {
		t.Fatalf("SystemRegion (RAM) = (%s, %v)", regName, ok)
	}

	// 3. Low-RAM mode: returns nil snapshot and starts no builds.
	app.cfg.lowRAMMode = true
	if got := app.sdeTopologySnapshot(); got != nil {
		t.Errorf("sdeTopologySnapshot in low-RAM mode = %v, want nil", got)
	}
	app.kickSDETopologyBuild()
	app.sdeTopology.mu.RLock()
	building := app.sdeTopology.building
	app.sdeTopology.mu.RUnlock()
	if building {
		t.Errorf("kickSDETopologyBuild started a build in low-RAM mode")
	}
	app.cfg.lowRAMMode = false

	// 4. Invalidation marks stale.
	app.invalidateSDETopology()
	app.sdeTopology.mu.RLock()
	stale := app.sdeTopology.stale
	app.sdeTopology.mu.RUnlock()
	if !stale {
		t.Errorf("invalidateSDETopology did not mark state stale")
	}
}
