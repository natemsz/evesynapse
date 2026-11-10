package app

// The stat pack read side: one loader every market surface goes
// through for a set of types in one region. It reads the stored
// rows only (the swept stat row with its full statistics set and
// trend figures, the stored true-value estimate, the labeled
// Fuzzwork row) in batch reads and hands each type back with all
// three, ready for the house price order (resolveAppraisalPriceMode).
// Appraisal reads through it today; baskets, the LP store and
// alerts join it, so no surface re-reads the pricing tables its own
// way or re-derives a figure the sweep already stored.
//
// The derived stat pack is held in RAM as an in-memory snapshot,
// following the search index pattern (search_index.go): built
// once at worker startup in the background, resident between
// rebuilds, refreshed on sweep completion, gated by LOW_RAM_MODE=1,
// with the existing Postgres queries serving as the fallback
// before the snapshot lands.

import (
	"context"
	"fmt"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// statPackMaxAge is the backstop rebuild age: a snapshot older
// than this is rebuilt in the background on the next read.
// Freshness comes from the sweep that builds the stored table,
// plus invalidation when the stored table changes, so the snapshot
// stays resident and serving between rebuilds.
const statPackMaxAge = 24 * time.Hour

type statPackKey struct {
	regionID int64
	typeID   int64
}

type beatRecord struct {
	LocationID int64
	RegionID   int64
	TypeID     int64
	IsBuyOrder int64
	At         time.Time
}

// statPackSnapshot is the in-memory copy of the market derived stat
// pack: swept region stats, station stats, and beats held in RAM
// and served without touching Postgres on render.
type statPackSnapshot struct {
	builtAt      time.Time
	regionStats  map[statPackKey]db.MarketRegionStat
	byType       map[int64][]db.MarketRegionStat
	stationStats map[stationKey]db.MarketStationStat
	beats        []beatRecord
	estimates    map[int64]db.TrueValue
	fallbacks    map[statPackKey]db.MarketStatsFallback
}

// statPackIndexState holds the Application's resident snapshot and
// build lifecycle.
type statPackIndexState struct {
	mu       sync.RWMutex
	snap     *statPackSnapshot
	building bool
	stale    bool
}

// statPackSnapshot returns the live snapshot, kicking a background
// rebuild when there is none or it has gone stale. It returns nil
// when no snapshot has landed yet or in low-RAM mode (LOW_RAM_MODE=1),
// so callers serve the SQL fallback.
// A render never falls back to SQL just because the snapshot is old
// while one is resident: SQL is the fallback only when no snapshot has
// landed yet.
func (app *Application) statPackSnapshot() *statPackSnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	idx := &app.statPackIdx
	idx.mu.RLock()
	snap := idx.snap
	stale := snap == nil || idx.stale || time.Since(snap.builtAt) > statPackMaxAge
	idx.mu.RUnlock()
	if stale {
		app.kickStatPackBuild()
	}
	return snap
}

// kickStatPackBuild starts one background build when none is
// running. It needs the worker context: without one there is no
// background lifecycle and the SQL path serves. In low-RAM mode
// (LOW_RAM_MODE=1) it never starts a build.
func (app *Application) kickStatPackBuild() {
	if app.cfg.lowRAMMode {
		return
	}
	if app.workerCtx == nil {
		return
	}
	idx := &app.statPackIdx
	idx.mu.Lock()
	if idx.building {
		idx.mu.Unlock()
		return
	}
	idx.building = true
	idx.mu.Unlock()
	go func() {
		defer func() {
			idx.mu.Lock()
			idx.building = false
			idx.mu.Unlock()
		}()
		if err := app.buildStatPack(app.workerCtx); err != nil {
			logging.Warnf("stat pack: build: %v", err)
		}
	}()
}

// buildStatPack reads the market derived tables and swaps the
// in-memory snapshot whole.
func (app *Application) buildStatPack(ctx context.Context) error {
	regionRows, err := app.queries.ListStatPackRegionStats(ctx)
	if err != nil {
		return fmt.Errorf("stat pack: region stats: %w", err)
	}
	stationRows, err := app.queries.ListStatPackStationStats(ctx)
	if err != nil {
		return fmt.Errorf("stat pack: station stats: %w", err)
	}
	beatRows, err := app.queries.ListStatPackBestBeats(ctx)
	if err != nil {
		return fmt.Errorf("stat pack: best beats: %w", err)
	}
	tvRows, err := app.queries.ListTrueValues(ctx)
	if err != nil {
		return fmt.Errorf("stat pack: true values: %w", err)
	}
	fbRows, err := app.queries.ListStatPackFallbackStats(ctx)
	if err != nil {
		return fmt.Errorf("stat pack: fallback stats: %w", err)
	}

	snap := &statPackSnapshot{
		builtAt:      time.Now(),
		regionStats:  make(map[statPackKey]db.MarketRegionStat, len(regionRows)),
		byType:       make(map[int64][]db.MarketRegionStat),
		stationStats: make(map[stationKey]db.MarketStationStat, len(stationRows)),
		beats:        make([]beatRecord, len(beatRows)),
		estimates:    make(map[int64]db.TrueValue, len(tvRows)),
		fallbacks:    make(map[statPackKey]db.MarketStatsFallback, len(fbRows)),
	}
	for _, r := range regionRows {
		k := statPackKey{regionID: r.RegionID, typeID: r.TypeID}
		snap.regionStats[k] = r
		snap.byType[r.TypeID] = append(snap.byType[r.TypeID], r)
	}
	for _, s := range stationRows {
		snap.stationStats[stationKey{LocationID: s.LocationID, TypeID: s.TypeID}] = s
	}
	for i, b := range beatRows {
		snap.beats[i] = beatRecord{
			LocationID: b.LocationID,
			RegionID:   b.RegionID,
			TypeID:     b.TypeID,
			IsBuyOrder: b.IsBuyOrder,
			At:         b.At,
		}
	}
	for _, tv := range tvRows {
		snap.estimates[tv.TypeID] = tv
	}
	for _, fb := range fbRows {
		snap.fallbacks[statPackKey{regionID: fb.RegionID, typeID: fb.TypeID}] = fb
	}

	idx := &app.statPackIdx
	idx.mu.Lock()
	idx.snap = snap
	idx.stale = false
	idx.mu.Unlock()
	return nil
}

// refreshStatPackFallbacks updates the resident snapshot's fallback
// stats table after an external fallback import lands.
func (app *Application) refreshStatPackFallbacks(ctx context.Context) {
	if app.cfg.lowRAMMode {
		return
	}
	fbRows, err := app.queries.ListStatPackFallbackStats(ctx)
	if err != nil {
		logging.Warnf("stat pack: refresh fallback stats: %v", err)
		return
	}
	idx := &app.statPackIdx
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.snap == nil {
		return
	}
	idx.snap.fallbacks = make(map[statPackKey]db.MarketStatsFallback, len(fbRows))
	for _, fb := range fbRows {
		idx.snap.fallbacks[statPackKey{regionID: fb.RegionID, typeID: fb.TypeID}] = fb
	}
}

// noteStatPackTrueValue records one newly computed true-value estimate
// directly into the resident snapshot.
func (app *Application) noteStatPackTrueValue(row db.TrueValue) {
	if app.cfg.lowRAMMode {
		return
	}
	idx := &app.statPackIdx
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.snap == nil {
		return
	}
	if idx.snap.estimates == nil {
		idx.snap.estimates = make(map[int64]db.TrueValue)
	}
	idx.snap.estimates[row.TypeID] = row
}

// refreshStatPackRegion updates the resident snapshot with the fresh
// rows for regionID just committed by a completed sweep, so the snapshot
// never serves staler data than SQL even before a full backstop rebuild
// runs.
func (app *Application) refreshStatPackRegion(ctx context.Context, regionID int64) {
	if app.cfg.lowRAMMode {
		return
	}
	rRows, err := app.queries.ListMarketRegionStatsByRegion(ctx, regionID)
	if err != nil {
		logging.Warnf("stat pack: refresh region %d stats: %v", regionID, err)
		return
	}
	sRows, err := app.queries.ListMarketStationStatsByRegion(ctx, regionID)
	if err != nil {
		logging.Warnf("stat pack: refresh region %d station stats: %v", regionID, err)
		return
	}

	idx := &app.statPackIdx
	idx.mu.Lock()
	if idx.snap == nil {
		if !idx.building && app.workerCtx != nil {
			idx.building = true
			go func() {
				defer func() {
					idx.mu.Lock()
					idx.building = false
					idx.mu.Unlock()
				}()
				_ = app.buildStatPack(app.workerCtx)
			}()
		}
		idx.mu.Unlock()
		return
	}

	snap := idx.snap
	for k := range snap.regionStats {
		if k.regionID == regionID {
			delete(snap.regionStats, k)
		}
	}
	for _, r := range rRows {
		snap.regionStats[statPackKey{regionID: r.RegionID, typeID: r.TypeID}] = r
	}
	snap.byType = make(map[int64][]db.MarketRegionStat, len(snap.regionStats))
	for _, r := range snap.regionStats {
		snap.byType[r.TypeID] = append(snap.byType[r.TypeID], r)
	}
	for k, s := range snap.stationStats {
		if s.RegionID == regionID {
			delete(snap.stationStats, k)
		}
	}
	for _, s := range sRows {
		snap.stationStats[stationKey{LocationID: s.LocationID, TypeID: s.TypeID}] = s
	}
	idx.stale = false
	idx.mu.Unlock()
	// The snapshot is current; fan the refreshed rows out to the
	// live market streams. The publish runs after the unlock so a
	// subscriber can never hold up a reader of the snapshot, and
	// it formats an event only for a row a stream is watching.
	for _, r := range rRows {
		app.publishMarketStatRow(r)
	}
}

// refreshStatPackTypes updates the resident snapshot after a sweep
// that wrote only some items: the written items' region rows are
// read back (their trend figures are derived in SQL), their station
// rows come from the sweep itself, and the removed items leave. The
// rest of the snapshot is untouched.
func (app *Application) refreshStatPackTypes(ctx context.Context, regionID int64, changed, gone []int64, stations []db.MarketStationStat) {
	if app.cfg.lowRAMMode {
		return
	}
	var rRows []db.MarketRegionStat
	if len(changed) > 0 {
		rows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{RegionID: regionID, Column2: changed})
		if err != nil {
			logging.Warnf("stat pack: refresh region %d types: %v", regionID, err)
			app.statPackIdx.mu.Lock()
			app.statPackIdx.stale = true
			app.statPackIdx.mu.Unlock()
			return
		}
		rRows = make([]db.MarketRegionStat, 0, len(rows))
		for _, r := range rows {
			rRows = append(rRows, db.MarketRegionStat(r))
		}
	}

	if !app.patchStatPackTypes(regionID, changed, gone, rRows, stations) {
		return // no snapshot yet: the first build reads the stored rows whole
	}
	// As after a full region refresh: fan the written rows out to the
	// live market streams, after the unlock.
	for _, r := range rRows {
		app.publishMarketStatRow(r)
	}
}

// patchStatPackTypes applies refreshStatPackTypes' rows to the
// resident snapshot under its lock, reporting false when no snapshot
// has landed yet.
func (app *Application) patchStatPackTypes(regionID int64, changed, gone []int64, rRows []db.MarketRegionStat, stations []db.MarketStationStat) bool {
	idx := &app.statPackIdx
	idx.mu.Lock()
	defer idx.mu.Unlock()
	snap := idx.snap
	if snap == nil {
		return false
	}
	touched := make(map[int64]bool, len(changed)+len(gone))
	for _, t := range changed {
		touched[t] = true
	}
	for _, t := range gone {
		touched[t] = true
		delete(snap.regionStats, statPackKey{regionID: regionID, typeID: t})
	}
	for _, r := range rRows {
		snap.regionStats[statPackKey{regionID: r.RegionID, typeID: r.TypeID}] = r
	}
	for t := range touched {
		var list []db.MarketRegionStat
		for _, r := range snap.byType[t] {
			if r.RegionID != regionID {
				list = append(list, r)
			}
		}
		if r, ok := snap.regionStats[statPackKey{regionID: regionID, typeID: t}]; ok {
			list = append(list, r)
		}
		if len(list) == 0 {
			delete(snap.byType, t)
		} else {
			snap.byType[t] = list
		}
	}
	for k, s := range snap.stationStats {
		if s.RegionID == regionID && touched[k.TypeID] {
			delete(snap.stationStats, k)
		}
	}
	for _, s := range stations {
		snap.stationStats[stationKey{LocationID: s.LocationID, TypeID: s.TypeID}] = s
	}
	return true
}

// statPack is one type's canonical market figures in one region.
type statPack struct {
	TypeID int64
	// Own is the swept stat row (nil when the region's sweep has no
	// row for the type). For a type whose market is ESI's global
	// book it is the global row, and FromGlobal says so.
	Own        *db.MarketRegionStat
	FromGlobal bool
	Estimate   *db.TrueValue
	Fallback   *db.MarketStatsFallback
}

// statPackSet is a loaded set, keyed by type.
type statPackSet map[int64]*statPack

// loadStatPacks reads the stat pack for typeIDs in regionID. A type
// the region carries no swept row for reads its global-market row
// instead (a global-market type's book IS its market everywhere);
// a type the region does carry keeps its own row. Duplicate ids are
// read once. Callers pass bounded sets (a basket, one LP store's
// offers, an appraisal's lines, a page of rows).
func (app *Application) loadStatPacks(ctx context.Context, regionID int64, typeIDs []int64) (statPackSet, error) {
	if regionID == 0 {
		regionID = defaultMarketRegion
	}
	out := make(statPackSet, len(typeIDs))
	ids := make([]int64, 0, len(typeIDs))
	for _, id := range typeIDs {
		if id > 0 && out[id] == nil {
			out[id] = &statPack{TypeID: id}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	servedFromMem := false
	if snap := app.statPackSnapshot(); snap != nil {
		idx := &app.statPackIdx
		idx.mu.RLock()
		if s := idx.snap; s != nil {
			servedFromMem = true
			for _, id := range ids {
				if r, ok := s.regionStats[statPackKey{regionID: regionID, typeID: id}]; ok {
					rowCopy := r
					out[id].Own = &rowCopy
				}
				if tv, ok := s.estimates[id]; ok {
					tvCopy := tv
					out[id].Estimate = &tvCopy
				}
				if fb, ok := s.fallbacks[statPackKey{regionID: regionID, typeID: id}]; ok {
					fbCopy := fb
					out[id].Fallback = &fbCopy
				}
			}
			if regionID != globalMarketRegion {
				for _, id := range ids {
					if out[id].Own == nil {
						if g, ok := s.regionStats[statPackKey{regionID: globalMarketRegion, typeID: id}]; ok {
							rowCopy := g
							out[id].Own = &rowCopy
							out[id].FromGlobal = true
						}
					}
				}
			}
		}
		idx.mu.RUnlock()
	}

	if !servedFromMem {
		ownRows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{
			RegionID: regionID, Column2: ids,
		})
		if err != nil {
			return nil, fmt.Errorf("stat pack: swept rows: %w", err)
		}
		for i := range ownRows {
			out[ownRows[i].TypeID].Own = &ownRows[i]
		}
		if regionID != globalMarketRegion {
			var missing []int64
			for _, id := range ids {
				if out[id].Own == nil {
					missing = append(missing, id)
				}
			}
			if len(missing) > 0 {
				gRows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{
					RegionID: globalMarketRegion, Column2: missing,
				})
				if err != nil {
					return nil, fmt.Errorf("stat pack: global rows: %w", err)
				}
				for i := range gRows {
					p := out[gRows[i].TypeID]
					p.Own, p.FromGlobal = &gRows[i], true
				}
			}
		}
		tvRows, err := app.queries.ListTrueValuesForTypes(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("stat pack: estimates: %w", err)
		}
		for i := range tvRows {
			if p := out[tvRows[i].TypeID]; p != nil {
				p.Estimate = &tvRows[i]
			}
		}
		fbRows, err := app.queries.ListMarketStatsFallbackForTypes(ctx, db.ListMarketStatsFallbackForTypesParams{
			RegionID: regionID, Column2: ids,
		})
		if err != nil {
			return nil, fmt.Errorf("stat pack: fallback: %w", err)
		}
		for i := range fbRows {
			out[fbRows[i].TypeID].Fallback = &fbRows[i]
		}
	}
	return out, nil
}
