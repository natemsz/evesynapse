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

import (
	"context"
	"fmt"

	db "evesynapse/internal/db/sqlc"
)

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
	return out, nil
}
