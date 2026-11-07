package app

import (
	"sync"
	"time"
)

// systemThreatEntry holds one system's threat figures and camp state in RAM.
type systemThreatEntry struct {
	SystemID int64
	Score    int
	Hour1    int
	LastKill time.Time
	Camp     *mapCamp
}

// threatSnapshot holds the in-memory computed threat layer and per-system lookup.
type threatSnapshot struct {
	builtAt time.Time
	layer   mapLayerThreat
	bySys   map[int64]systemThreatEntry
}

// threatMemoryState holds the Application's resident threat snapshot and build state.
type threatMemoryState struct {
	mu   sync.RWMutex
	snap *threatSnapshot
}

// threatMemorySnapshot returns the resident snapshot when fresh (under 60s).
// Low-RAM mode (LOW_RAM_MODE=1) always returns nil.
func (app *Application) threatMemorySnapshot(now time.Time) *threatSnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	s := &app.threatState
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()
	if snap != nil && now.Sub(snap.builtAt) < 60*time.Second {
		return snap
	}
	return nil
}

// storeThreatSnapshot caches a newly computed threat layer in RAM.
func (app *Application) storeThreatSnapshot(now time.Time, layer mapLayerThreat) {
	if app.cfg.lowRAMMode {
		return
	}
	snap := &threatSnapshot{
		builtAt: now,
		layer:   layer,
		bySys:   make(map[int64]systemThreatEntry, len(layer.ID)),
	}
	campBySys := make(map[int64]*mapCamp, len(layer.Camps))
	for i := range layer.Camps {
		c := layer.Camps[i]
		campBySys[c.System] = &c
	}
	for i, sysID := range layer.ID {
		var lk time.Time
		if i < len(layer.Last) && layer.Last[i] > 0 {
			lk = time.Unix(layer.Last[i], 0).UTC()
		}
		var score, hour int
		if i < len(layer.Score) {
			score = layer.Score[i]
		}
		if i < len(layer.Hour) {
			hour = layer.Hour[i]
		}
		snap.bySys[sysID] = systemThreatEntry{
			SystemID: sysID,
			Score:    score,
			Hour1:    hour,
			LastKill: lk,
			Camp:     campBySys[sysID],
		}
	}
	s := &app.threatState
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// systemThreat returns one solar system's live threat and camp from RAM.
func (app *Application) systemThreat(systemID int64) (systemThreatEntry, bool) {
	if app.cfg.lowRAMMode {
		return systemThreatEntry{}, false
	}
	s := &app.threatState
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snap == nil {
		return systemThreatEntry{}, false
	}
	entry, ok := s.snap.bySys[systemID]
	return entry, ok
}
