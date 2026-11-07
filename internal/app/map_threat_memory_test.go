package app

import (
	"context"
	"testing"
	"time"
)

// TestThreatMemoryPinsServingAndLowRAM pins the in-memory solar system
// threat heatmap (map_threat_memory.go):
//  1. storeThreatSnapshot populates RAM layer and per-system lookup.
//  2. systemThreat returns the cached score, 1-hour kill count, and camp.
//  3. loadThreat serves directly from memory when fresh (<60s).
//  4. LOW_RAM_MODE=1 returns nil snapshot and disables memory lookup.
func TestThreatMemoryPinsServingAndLowRAM(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	// 1. Before store: systemThreat returns false.
	if _, ok := app.systemThreat(30000142); ok {
		t.Fatalf("systemThreat before store returned ok = true")
	}

	layer := mapLayerThreat{
		At:    now.Unix(),
		ID:    []int64{30000142, 30003504},
		Score: []int{150, 400},
		Hour:  []int{2, 5},
		Last:  []int64{now.Unix(), now.Add(-5 * time.Minute).Unix()},
		Camps: []mapCamp{
			{System: 30003504, Kills: 4, Kind: "alliance", Group: "Hunter Alliance"},
		},
	}

	app.storeThreatSnapshot(now, layer)

	// 2. systemThreat serves from RAM.
	entry, ok := app.systemThreat(30000142)
	if !ok || entry.Score != 150 || entry.Hour1 != 2 {
		t.Fatalf("systemThreat(Jita) = %+v, ok = %v", entry, ok)
	}
	rancer, ok := app.systemThreat(30003504)
	if !ok || rancer.Score != 400 || rancer.Hour1 != 5 || rancer.Camp == nil || rancer.Camp.Group != "Hunter Alliance" {
		t.Fatalf("systemThreat(Rancer) = %+v, ok = %v", rancer, ok)
	}

	// 3. loadThreat serves resident memory when fresh.
	gotLayer, ok := app.loadThreat(ctx, now.Add(10*time.Second))
	if !ok || len(gotLayer.ID) != 2 || gotLayer.Score[0] != 150 {
		t.Fatalf("loadThreat from RAM = %+v, ok = %v", gotLayer, ok)
	}

	// 4. Low-RAM mode: returns nil snapshot and false for systemThreat.
	app.cfg.lowRAMMode = true
	if got := app.threatMemorySnapshot(now); got != nil {
		t.Errorf("threatMemorySnapshot in low-RAM mode = %v, want nil", got)
	}
	if _, ok := app.systemThreat(30000142); ok {
		t.Errorf("systemThreat in low-RAM mode returned ok = true")
	}
	app.cfg.lowRAMMode = false
}
