package app

import (
	"context"
	"sync"
	"time"

	"evesynapse/internal/logging"
)

// memSnapshot holds one resident in-memory snapshot and its
// background build lifecycle: the live snapshot, when it was built,
// whether a build is running, and whether the snapshot was marked
// stale. Readers take mu.RLock to read snap; a build swaps snap whole.
// The zero value is ready to use.
type memSnapshot[S any] struct {
	mu       sync.RWMutex
	snap     *S
	builtAt  time.Time
	building bool
	stale    bool
}

// current returns the resident snapshot (nil when none has landed)
// and reports whether a rebuild is due: there is no snapshot, it was
// marked stale, or it is older than maxAge.
func (m *memSnapshot[S]) current(maxAge time.Duration) (*S, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	due := m.snap == nil || m.stale || time.Since(m.builtAt) > maxAge
	return m.snap, due
}

// kick starts build in the background unless one is already running.
// A failed build is logged under what and leaves the old snapshot in
// place.
func (m *memSnapshot[S]) kick(ctx context.Context, what string, build func(context.Context) error) {
	m.mu.Lock()
	if m.building {
		m.mu.Unlock()
		return
	}
	m.building = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			m.building = false
			m.mu.Unlock()
		}()
		if err := build(ctx); err != nil {
			logging.Warnf("%s: build: %v", what, err)
		}
	}()
}

// swapLocked installs a freshly built snapshot and clears the stale
// mark. The caller holds mu.
func (m *memSnapshot[S]) swapLocked(snap *S) {
	m.snap = snap
	m.builtAt = time.Now()
	m.stale = false
}

// swap installs a freshly built snapshot and clears the stale mark.
func (m *memSnapshot[S]) swap(snap *S) {
	m.mu.Lock()
	m.swapLocked(snap)
	m.mu.Unlock()
}

// markStale makes the next read start a rebuild.
func (m *memSnapshot[S]) markStale() {
	m.mu.Lock()
	m.stale = true
	m.mu.Unlock()
}

// snapshotBuildContext returns the context background snapshot
// builds run under. It reports false in low-RAM mode
// (LOW_RAM_MODE=1), which never builds a snapshot, and when there is
// no worker context, so no background lifecycle: the SQL paths serve.
func (app *Application) snapshotBuildContext() (context.Context, bool) {
	if app.cfg.lowRAMMode || app.workerCtx == nil {
		return nil, false
	}
	return app.workerCtx, true
}
