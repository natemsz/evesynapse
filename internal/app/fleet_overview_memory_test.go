package app

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestFleetOverviewMemoryPinsServingAndLowRAM pins the in-memory multi-character
// fleet overview rollup (fleet_overview_memory.go):
//  1. First buildFleet builds and populates the in-memory rollup.
//  2. Subsequent calls within 30s serve directly from RAM.
//  3. Invalidation clears the cached rollup for that user.
//  4. LOW_RAM_MODE=1 returns nil rollup and bypasses RAM caching.
func TestFleetOverviewMemoryPinsServingAndLowRAM(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	userID := int64(42)
	now := time.Now()

	// 1. Before store: fleetRollup returns nil.
	if got := app.fleetRollup(userID, now); got != nil {
		t.Fatalf("fleetRollup before build returned %v, want nil", got)
	}

	bundles := []*charSnaps{
		{
			ch:          db.Character{CharacterID: 93300001, Name: "Fleet Commander", Tags: "fc"},
			walletKnown: true,
			wallet:      500000000,
			queueKnown:  true,
		},
		{
			ch:          db.Character{CharacterID: 93300002, Name: "Hauler One", Tags: "logi"},
			walletKnown: true,
			wallet:      150000000,
			queueKnown:  true,
		},
	}

	w := app.buildFleet(ctx, userID, bundles)
	if w == nil || len(w.Rows) != 2 || w.TotalISK != esi.FormatISK(650000000) {
		t.Fatalf("buildFleet = %+v", w)
	}

	// 2. Cached in RAM: serves instantly.
	cached := app.fleetRollup(userID, now.Add(5*time.Second))
	if cached == nil || len(cached.Rows) != 2 || cached.TotalISK != esi.FormatISK(650000000) {
		t.Fatalf("cached rollup = %+v", cached)
	}

	// 3. Invalidation clears rollup for user.
	app.invalidateFleetRollup(userID)
	if got := app.fleetRollup(userID, now.Add(10*time.Second)); got != nil {
		t.Fatalf("fleetRollup after invalidate = %v, want nil", got)
	}

	// 4. Low-RAM mode returns nil and bypasses RAM caching.
	app.cfg.lowRAMMode = true
	app.storeFleetRollup(userID, now, w)
	if got := app.fleetRollup(userID, now); got != nil {
		t.Errorf("fleetRollup in low-RAM mode = %v, want nil", got)
	}
	app.cfg.lowRAMMode = false
}
