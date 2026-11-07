package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// sdeTopologyMaxAge is the backstop rebuild age for the SDE universe
// topology held in RAM. SDE data is static between imports; invalidation
// (importSDE) triggers a reload when a new SDE is imported.
const sdeTopologyMaxAge = 24 * time.Hour

type sdeSystemEntry struct {
	ID         int64
	Name       string
	RegionID   int64
	RegionName string
	Security   float64
	SecText    string
	SecTier    int
}

type sdeTopologySnapshot struct {
	builtAt   time.Time
	systems   map[int64]sdeSystemEntry
	bySysName map[string]sdeSystemEntry
	regions   map[int64]string
	types     map[int64]db.SdeType
	typeNames map[int64]string
}

type sdeTopologyState struct {
	mu       sync.RWMutex
	snap     *sdeTopologySnapshot
	building bool
	stale    bool
}

func (app *Application) sdeTopologySnapshot() *sdeTopologySnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	s := &app.sdeTopology
	s.mu.RLock()
	snap := s.snap
	stale := snap == nil || s.stale || time.Since(snap.builtAt) > sdeTopologyMaxAge
	s.mu.RUnlock()
	if stale {
		app.kickSDETopologyBuild()
	}
	return snap
}

func (app *Application) kickSDETopologyBuild() {
	if app.cfg.lowRAMMode {
		return
	}
	if app.workerCtx == nil {
		return
	}
	s := &app.sdeTopology
	s.mu.Lock()
	if s.building {
		s.mu.Unlock()
		return
	}
	s.building = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.building = false
			s.mu.Unlock()
		}()
		if err := app.buildSDETopology(app.workerCtx); err != nil {
			logging.Warnf("sde topology: build: %v", err)
		}
	}()
}

func (app *Application) invalidateSDETopology() {
	s := &app.sdeTopology
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
	app.kickSDETopologyBuild()
}

func (app *Application) buildSDETopology(ctx context.Context) error {
	sysRows, err := app.queries.ListSearchIndexSystems(ctx)
	if err != nil {
		return fmt.Errorf("sde topology: systems: %w", err)
	}
	regRows, err := app.queries.ListSearchIndexRegions(ctx)
	if err != nil {
		return fmt.Errorf("sde topology: regions: %w", err)
	}
	typeRows, err := app.queries.ListSDETopologyTypes(ctx)
	if err != nil {
		return fmt.Errorf("sde topology: types: %w", err)
	}

	snap := &sdeTopologySnapshot{
		builtAt:   time.Now(),
		systems:   make(map[int64]sdeSystemEntry, len(sysRows)),
		bySysName: make(map[string]sdeSystemEntry, len(sysRows)),
		regions:   make(map[int64]string, len(regRows)),
		types:     make(map[int64]db.SdeType, len(typeRows)),
		typeNames: make(map[int64]string, len(typeRows)),
	}

	for _, r := range regRows {
		snap.regions[r.RegionID] = r.Name
	}

	for _, s := range sysRows {
		secText, secTier := secRound(s.Security)
		entry := sdeSystemEntry{
			ID:         s.SystemID,
			Name:       s.Name,
			RegionID:   s.RegionID,
			RegionName: s.RegionName,
			Security:   s.Security,
			SecText:    secText,
			SecTier:    secTier,
		}
		snap.systems[s.SystemID] = entry
		snap.bySysName[strings.ToLower(s.Name)] = entry
	}

	for _, t := range typeRows {
		snap.types[t.TypeID] = t
		snap.typeNames[t.TypeID] = t.Name
	}

	s := &app.sdeTopology
	s.mu.Lock()
	s.snap = snap
	s.stale = false
	s.mu.Unlock()
	return nil
}

func (app *Application) sdeSystem(id int64) (sdeSystemEntry, bool) {
	snap := app.sdeTopologySnapshot()
	if snap == nil {
		return sdeSystemEntry{}, false
	}
	entry, ok := snap.systems[id]
	return entry, ok
}

func (app *Application) sdeSystemByName(name string) (sdeSystemEntry, bool) {
	snap := app.sdeTopologySnapshot()
	if snap == nil {
		return sdeSystemEntry{}, false
	}
	entry, ok := snap.bySysName[strings.ToLower(name)]
	return entry, ok
}

func (app *Application) sdeRegion(id int64) (string, bool) {
	snap := app.sdeTopologySnapshot()
	if snap == nil {
		return "", false
	}
	name, ok := snap.regions[id]
	return name, ok
}

func (app *Application) sdeType(id int64) (db.SdeType, bool) {
	snap := app.sdeTopologySnapshot()
	if snap == nil {
		return db.SdeType{}, false
	}
	t, ok := snap.types[id]
	return t, ok
}

func (app *Application) sdeTypeName(id int64) (string, bool) {
	snap := app.sdeTopologySnapshot()
	if snap == nil {
		return "", false
	}
	name, ok := snap.typeNames[id]
	return name, ok
}
