package app

import (
	"context"
	"fmt"
	"time"
)

const entityDirectoryMaxAge = 24 * time.Hour

type entityDirectoryEntry struct {
	ID     int64
	Name   string
	Ticker string
	OrgID  int64
	Kind   string // "pilot", "corporation", "alliance", "structure"
}

type entityDirectorySnapshot struct {
	entities map[int64]entityDirectoryEntry
}

type entityDirectoryState = memSnapshot[entityDirectorySnapshot]

func (app *Application) entityDirectorySnapshot() *entityDirectorySnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	snap, due := app.entityDir.current(entityDirectoryMaxAge)
	if due {
		app.kickEntityDirectoryBuild()
	}
	return snap
}

func (app *Application) kickEntityDirectoryBuild() {
	if ctx, ok := app.snapshotBuildContext(); ok {
		app.entityDir.kick(ctx, "entity directory", app.buildEntityDirectory)
	}
}

func (app *Application) buildEntityDirectory(ctx context.Context) error {
	pilotRows, err := app.queries.ListSearchIndexPilotRecords(ctx)
	if err != nil {
		return fmt.Errorf("entity directory: pilots: %w", err)
	}
	corpRows, err := app.queries.ListSearchIndexCorporationRecords(ctx)
	if err != nil {
		return fmt.Errorf("entity directory: corps: %w", err)
	}
	allianceRows, err := app.queries.ListSearchIndexAllianceRecords(ctx)
	if err != nil {
		return fmt.Errorf("entity directory: alliances: %w", err)
	}
	structRows, err := app.queries.ListResolvedStructureNames(ctx)
	if err != nil {
		return fmt.Errorf("entity directory: structures: %w", err)
	}

	snap := &entityDirectorySnapshot{
		entities: make(map[int64]entityDirectoryEntry, len(pilotRows)+len(corpRows)+len(allianceRows)+len(structRows)),
	}

	for _, row := range pilotRows {
		if name, _, ok := searchPilotEntry(row.Payload); ok && name != "" {
			snap.entities[row.CharacterID] = entityDirectoryEntry{
				ID:   row.CharacterID,
				Name: name,
				Kind: "pilot",
			}
		}
	}
	for _, row := range corpRows {
		if name, ticker, ok := searchCorpEntry(row.Payload); ok && name != "" {
			snap.entities[row.CorporationID] = entityDirectoryEntry{
				ID:     row.CorporationID,
				Name:   name,
				Ticker: ticker,
				Kind:   "corporation",
			}
		}
	}
	for _, row := range allianceRows {
		if name, ticker, ok := searchAllianceEntry(row.Payload); ok && name != "" {
			snap.entities[row.AllianceID] = entityDirectoryEntry{
				ID:     row.AllianceID,
				Name:   name,
				Ticker: ticker,
				Kind:   "alliance",
			}
		}
	}
	for _, row := range structRows {
		if row.Name != "" {
			snap.entities[row.StructureID] = entityDirectoryEntry{
				ID:   row.StructureID,
				Name: row.Name,
				Kind: "structure",
			}
		}
	}

	app.entityDir.swap(snap)
	return nil
}

func (app *Application) noteEntity(id int64, entry entityDirectoryEntry, drop bool) {
	if app.cfg.lowRAMMode {
		return
	}
	s := &app.entityDir
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		return
	}
	if drop {
		delete(s.snap.entities, id)
	} else {
		s.snap.entities[id] = entry
	}
}

func (app *Application) entityLookup(id int64) (entityDirectoryEntry, bool) {
	snap := app.entityDirectorySnapshot()
	if snap == nil {
		return entityDirectoryEntry{}, false
	}
	e, ok := snap.entities[id]
	return e, ok
}

func (app *Application) pilotName(id int64) (string, bool) {
	if e, ok := app.entityLookup(id); ok && e.Kind == "pilot" && e.Name != "" {
		return e.Name, true
	}
	return "", false
}

func (app *Application) corpName(id int64) (string, bool) {
	if e, ok := app.entityLookup(id); ok && e.Kind == "corporation" && e.Name != "" {
		return e.Name, true
	}
	return "", false
}

func (app *Application) allianceName(id int64) (string, bool) {
	if e, ok := app.entityLookup(id); ok && e.Kind == "alliance" && e.Name != "" {
		return e.Name, true
	}
	return "", false
}
