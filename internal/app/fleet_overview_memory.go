package app

import (
	"sync"
	"time"
)

// fleetRollupMaxAge is the backstop TTL for the multi-character fleet
// overview rollup held in RAM (30 seconds).
const fleetRollupMaxAge = 30 * time.Second

type fleetRollupSnapshot struct {
	builtAt time.Time
	widget  *fleetWidget
}

type fleetOverviewState struct {
	mu      sync.RWMutex
	rollups map[int64]*fleetRollupSnapshot // keyed by userID
}

// fleetRollup returns the cached fleet overview widget for userID from RAM.
// Returns nil in low-RAM mode (LOW_RAM_MODE=1) or when absent/expired.
func (app *Application) fleetRollup(userID int64, now time.Time) *fleetWidget {
	if app.cfg.lowRAMMode {
		return nil
	}
	s := &app.fleetOverview
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rollups == nil {
		return nil
	}
	snap, ok := s.rollups[userID]
	if !ok || snap == nil || now.Sub(snap.builtAt) > fleetRollupMaxAge {
		return nil
	}
	return snap.widget
}

// storeFleetRollup stores the assembled fleetWidget for userID in RAM.
func (app *Application) storeFleetRollup(userID int64, now time.Time, widget *fleetWidget) {
	if app.cfg.lowRAMMode || widget == nil {
		return
	}
	s := &app.fleetOverview
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rollups == nil {
		s.rollups = make(map[int64]*fleetRollupSnapshot)
	}
	s.rollups[userID] = &fleetRollupSnapshot{
		builtAt: now,
		widget:  widget,
	}
}

// invalidateFleetRollup removes the cached rollup for a user when character
// links or settings change.
func (app *Application) invalidateFleetRollup(userID int64) {
	s := &app.fleetOverview
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rollups != nil {
		delete(s.rollups, userID)
	}
}
