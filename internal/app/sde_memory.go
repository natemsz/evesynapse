package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
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
	systems   map[int64]sdeSystemEntry
	bySysName map[string]sdeSystemEntry
	regions   map[int64]string
	types     map[int64]db.SdeType
	typeNames map[int64]string
}

type sdeTopologyState = memSnapshot[sdeTopologySnapshot]

func (app *Application) sdeTopologySnapshot() *sdeTopologySnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	snap, due := app.sdeTopology.current(sdeTopologyMaxAge)
	if due {
		app.kickSDETopologyBuild()
	}
	return snap
}

func (app *Application) kickSDETopologyBuild() {
	if ctx, ok := app.snapshotBuildContext(); ok {
		app.sdeTopology.kick(ctx, "sde topology", app.buildSDETopology)
	}
}

func (app *Application) invalidateSDETopology() {
	app.sdeTopology.markStale()
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
