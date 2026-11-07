package app

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestStatPackMemorySnapshotPinsServingAndLowRAM pins the in-memory
// stat pack snapshot (market_stat_pack.go):
//  1. Before any snapshot lands, loadStatPacks serves from SQL.
//  2. Once built, loadStatPacks serves from RAM and matches the SQL path.
//  3. LOW_RAM_MODE=1 returns nil snapshot, starts no builds, and serves SQL.
//  4. A region sweep completion updates the resident snapshot so memory
//     serves the freshly committed figures immediately.
func TestStatPackMemorySnapshotPinsServingAndLowRAM(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	transport.books[10000002] = [][]esi.MarketOrder{{
		{OrderID: 1, TypeID: 34, Price: 10, VolumeRemain: 100},
		{OrderID: 2, TypeID: 35, Price: 50, VolumeRemain: 200},
	}}
	sweepUntilIdle(t, app)

	now := time.Now().UTC()
	if _, err := app.db.ExecContext(ctx, `INSERT INTO true_values (type_id, build_cost, est_sell, sell_samples, sell_source, computed_at) VALUES (34, 5, 12, 4, 'listing', $1)`, now); err != nil {
		t.Fatalf("insert true value: %v", err)
	}
	if err := q.InsertMarketStatsFallback(ctx, db.InsertMarketStatsFallbackParams{
		RegionID: 10000002, TypeID: 34, MedianSell: 11, FivepctSell: 10.5, OrdersetID: 1, SourceAsof: now, FetchedAt: now,
	}); err != nil {
		t.Fatalf("insert fallback: %v", err)
	}

	// 1. Before snapshot builds: SQL path serves.
	app.statPackIdx.mu.Lock()
	app.statPackIdx.snap = nil
	app.statPackIdx.mu.Unlock()

	sqlPacks, err := app.loadStatPacks(ctx, 10000002, []int64{34, 35})
	if err != nil {
		t.Fatalf("loadStatPacks (SQL): %v", err)
	}
	if sqlPacks[34] == nil || sqlPacks[34].Own == nil || sqlPacks[34].Own.TypicalSell != 10 {
		t.Fatalf("SQL pack 34 typical sell want 10, got %+v", sqlPacks[34])
	}
	if sqlPacks[34].Estimate == nil || sqlPacks[34].Estimate.EstSell.Float64 != 12 {
		t.Fatalf("SQL pack 34 estimate want 12, got %+v", sqlPacks[34].Estimate)
	}
	if sqlPacks[34].Fallback == nil || sqlPacks[34].Fallback.MedianSell != 11 {
		t.Fatalf("SQL pack 34 fallback want 11, got %+v", sqlPacks[34].Fallback)
	}
	if sqlPacks[35] == nil || sqlPacks[35].Own == nil || sqlPacks[35].Own.TypicalSell != 50 {
		t.Fatalf("SQL pack 35 typical sell want 50, got %+v", sqlPacks[35])
	}

	// 2. Build snapshot: memory serves and matches SQL.
	if err := app.buildStatPack(ctx); err != nil {
		t.Fatalf("buildStatPack: %v", err)
	}
	memPacks, err := app.loadStatPacks(ctx, 10000002, []int64{34, 35})
	if err != nil {
		t.Fatalf("loadStatPacks (mem): %v", err)
	}
	if memPacks[34].Own.TypicalSell != sqlPacks[34].Own.TypicalSell {
		t.Errorf("mem pack 34 TypicalSell = %v, want %v", memPacks[34].Own.TypicalSell, sqlPacks[34].Own.TypicalSell)
	}
	if memPacks[34].Estimate == nil || memPacks[34].Estimate.EstSell.Float64 != 12 {
		t.Errorf("mem pack 34 Estimate = %v, want 12", memPacks[34].Estimate)
	}
	if memPacks[34].Fallback == nil || memPacks[34].Fallback.MedianSell != 11 {
		t.Errorf("mem pack 34 Fallback = %v, want 11", memPacks[34].Fallback)
	}
	if memPacks[35].Own.TypicalSell != sqlPacks[35].Own.TypicalSell {
		t.Errorf("mem pack 35 TypicalSell = %v, want %v", memPacks[35].Own.TypicalSell, sqlPacks[35].Own.TypicalSell)
	}

	// 3. Low-RAM mode: never builds, returns nil snapshot.
	app.cfg.lowRAMMode = true
	if got := app.statPackSnapshot(); got != nil {
		t.Errorf("statPackSnapshot() in low-RAM mode = %v, want nil", got)
	}
	app.kickStatPackBuild()
	app.statPackIdx.mu.RLock()
	building := app.statPackIdx.building
	app.statPackIdx.mu.RUnlock()
	if building {
		t.Errorf("kickStatPackBuild started a build in low-RAM mode; want none")
	}
	app.cfg.lowRAMMode = false

	// 4. Sweep update refreshes resident memory immediately.
	transport.books[10000002] = [][]esi.MarketOrder{{
		{OrderID: 3, TypeID: 34, Price: 25, VolumeRemain: 100},
		{OrderID: 4, TypeID: 35, Price: 65, VolumeRemain: 200},
	}}
	// Age the fetch state so the region sweeps again
	if _, err := app.db.ExecContext(ctx, "UPDATE market_fetch_state SET attempted_at = $1 WHERE kind = $2",
		time.Now().Add(-2*time.Hour), regionSweepKind(10000002)); err != nil {
		t.Fatalf("age sweep state: %v", err)
	}
	sweepUntilIdle(t, app)

	// Memory was refreshed by completeRegionSweep -> refreshStatPackRegion
	refreshedPacks, err := app.loadStatPacks(ctx, 10000002, []int64{34, 35})
	if err != nil {
		t.Fatalf("loadStatPacks after sweep: %v", err)
	}
	if refreshedPacks[34].Own.TypicalSell != 25 {
		t.Errorf("refreshed pack 34 TypicalSell = %v, want 25", refreshedPacks[34].Own.TypicalSell)
	}
	if refreshedPacks[35].Own.TypicalSell != 65 {
		t.Errorf("refreshed pack 35 TypicalSell = %v, want 65", refreshedPacks[35].Own.TypicalSell)
	}
}
