package app

import (
	"context"
	"testing"
	"time"
)

// TestThreatMemoryPinsServingAndLowRAM pins the in-memory solar system
// threat heatmap (map_threat_memory.go): loadThreat serves a stored
// layer from memory while it is fresh (under 60s), and LOW_RAM_MODE=1
// keeps no snapshot.
func TestThreatMemoryPinsServingAndLowRAM(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

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

	// loadThreat serves resident memory when fresh.
	gotLayer, ok := app.loadThreat(ctx, now.Add(10*time.Second))
	if !ok || len(gotLayer.ID) != 2 || gotLayer.Score[0] != 150 {
		t.Fatalf("loadThreat from RAM = %+v, ok = %v", gotLayer, ok)
	}

	// Low-RAM mode keeps no snapshot.
	app.cfg.lowRAMMode = true
	if got := app.threatMemorySnapshot(now); got != nil {
		t.Errorf("threatMemorySnapshot in low-RAM mode = %v, want nil", got)
	}
	app.cfg.lowRAMMode = false
}
