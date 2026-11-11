package app

import (
	"sync"
	"time"
)

// threatSnapshot holds the in-memory computed threat layer.
type threatSnapshot struct {
	builtAt time.Time
	layer   mapLayerThreat
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
	snap := &threatSnapshot{builtAt: now, layer: layer}
	s := &app.threatState
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}
