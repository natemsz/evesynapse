package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// True values (schema 051): a bottom-up price for manufacturable
// types, computed by the worker (truevalue_worker.go) and read
// here. CCP's adjusted price is a fossil for ships that trade
// by contract instead of on the open market -- it derives from
// market history that barely exists for them, so an Avatar
// "adjusts" to ~65B against a ~600B real value -- and it used to
// be the fallback behind the guide average everywhere a price
// was needed. Prices now resolve in the house order (owner
// doctrine, 2026-10-17):
//
//     Forge swept typical sell  ->  true-value estimated sell
//     ->  unpriced
//
// The guide's average is banned as a price source outright and
// the adjusted price survives only as the EIV input to CCP's
// job-fee formula. Items with a healthy market never reach the
// estimated-sell tier (the worker stores no estimated sell for
// them): the swept book prices them directly.
//
// The pricing profile is a standard, named here once: build in
// a T2-rigged Sotiyo in null security (the facility the planner
// would quote at those SDE attributes), components researched
// to ME 10, ship hulls at ME 2 (the top of the range hull BPCs
// actually trade at), installation fees at the cost index of a
// major staging system, facility tax at the NPC rate (a player
// structure's real rate is the owner's private setting; the
// public build hubs cluster at this one). profile_version
// stamps every row: bump it when any of this changes and the
// whole table goes stale at once.
// ---------------------------------------------------------------------------

const (
	trueValueProfileVersion = 1

	trueValueComponentME = 10 // components: fully researched
	trueValueShipME      = 2  // hulls: BPCs trade at ME 0-2

	// The facility: a Sotiyo (35827) wearing the T2 material
	// rig the planner uses as its tier representative
	// (37147) -- rig bonuses are uniform across a tier's
	// manufacturing families in the SDE (buildplanner).
	trueValueStructureTypeID = 35827
	trueValueRigTypeID       = 37147

	// The fee reference system: 1DQ1-A (30004759), the game's
	// best-known staging system, read from the stored cost
	// indices with a cluster-median fallback.
	trueValueReferenceSystemID = 30004759

	// A type has a real market when the guide carries an
	// average for it and The Forge trades at least this many
	// units a day; below that, contracts are the market.
	trueValueHealthyDailyVolume = 10.0

	// How long a stored row stays fresh when its inputs have
	// not moved.
	trueValueStaleAfter = 48 * time.Hour

	// Renders reload the table at most this often.
	trueValueCacheWindow = 5 * time.Minute
)

// storedTrueValues returns the worker's true values by type,
// reloaded from the table at most once per cache window. Nil
// until the worker's first pass has stored any.
func (app *Application) storedTrueValues(ctx context.Context) map[int64]db.TrueValue {
	app.trueValuesMu.Lock()
	defer app.trueValuesMu.Unlock()
	if app.trueValuesCache != nil && time.Since(app.trueValuesLoaded) < trueValueCacheWindow {
		return app.trueValuesCache
	}
	rows, err := app.queries.ListTrueValues(ctx)
	if err != nil {
		logging.Errorf("true values: load: %v", err)
		return app.trueValuesCache // nil on first failure: "none"
	}
	if len(rows) == 0 {
		app.trueValuesCache, app.trueValuesLoaded = nil, time.Now()
		return nil
	}
	out := make(map[int64]db.TrueValue, len(rows))
	for _, row := range rows {
		out[row.TypeID] = row
	}
	app.trueValuesCache, app.trueValuesLoaded = out, time.Now()
	return out
}

// TrueValues is the fit foundation's view of the table
// (fitview.Env): estimated sell by type, only where one is
// stored -- healthy-market types are absent by design.
func (app *Application) TrueValues(ctx context.Context) map[int64]float64 {
	rows := app.storedTrueValues(ctx)
	if len(rows) == 0 {
		return nil
	}
	out := make(map[int64]float64, len(rows))
	for id, tv := range rows {
		if tv.EstSell.Valid && tv.EstSell.Float64 > 0 {
			out[id] = tv.EstSell.Float64
		}
	}
	return out
}

// pickValuationPrice resolves one unit's price in the house
// order: The Forge's swept typical sell, then the true-value
// estimated sell (estSell, 0 when none is stored). ok=false
// when neither knows the type — CCP's guide average is banned
// as a price source (owner doctrine, 2026-10-17) and the
// adjusted price survives only as the EIV input to job fees,
// never as a price.
func pickValuationPrice(typicalSell, estSell float64) (float64, bool) {
	if typicalSell > 0 {
		return typicalSell, true
	}
	if estSell > 0 {
		return estSell, true
	}
	return 0, false
}

// estSellOf reads the estimated sell for one type out of a
// stored-true-values map (nil-safe): 0 when there is none.
func estSellOf(rows map[int64]db.TrueValue, typeID int64) float64 {
	if tv, ok := rows[typeID]; ok && tv.EstSell.Valid {
		return tv.EstSell.Float64
	}
	return 0
}

// trueValueBasis is the estimated sell's provenance in plain
// end-user words, tier by tier: recorded sales are sales,
// vanished listings are only ever "likely" sales (a listing
// can also have been withdrawn), live listings are asks, and
// the anchor is named when no tier had two samples to speak
// with.
func trueValueBasis(tv db.TrueValue) string {
	if tv.SellSamples >= 2 {
		switch tv.SellSource {
		case tvSampleFulfilled:
			return fmt.Sprintf("from %d recorded sales", tv.SellSamples)
		case tvSampleVanished:
			return fmt.Sprintf("from %d likely sales", tv.SellSamples)
		default:
			return fmt.Sprintf("from %d public contracts", tv.SellSamples)
		}
	}
	return "anchored to build cost"
}

// trueValueProfile is the resolved facility + fee state one
// pricing pass runs under.
type trueValueProfile struct {
	matFactor  float64 // structure role bonus x T2 rig, null band
	costFactor float64 // structure cost bonus (rigs do not touch it)
	costIndex  float64 // reference system's manufacturing index
}

// resolveTrueValueProfile reads the profile's game data from
// the local SDE and the stored cost indices, the same way the
// planner resolves a facility (buildplanner/payload.go): the
// structure's role bonuses are straight multipliers (dogma
// 2600/2601), the rig publishes a percent decrease (2594) and
// per-band effectiveness multipliers (null = 2357). Anything
// missing degrades that term to neutral, never to a guess.
func (app *Application) resolveTrueValueProfile(ctx context.Context) trueValueProfile {
	profile := trueValueProfile{matFactor: 1, costFactor: 1}
	attrs := make(map[int64]map[int64]float64)
	if rows, err := app.queries.ListSDETypeAttributesByIDs(ctx, []int64{trueValueStructureTypeID, trueValueRigTypeID}); err == nil {
		for _, row := range rows {
			if attrs[row.TypeID] == nil {
				attrs[row.TypeID] = make(map[int64]float64)
			}
			attrs[row.TypeID][row.AttributeID] = row.Value
		}
	} else {
		logging.Errorf("true values: facility attributes: %v", err)
	}
	get := func(typeID, attr int64, def float64) float64 {
		if v, ok := attrs[typeID][attr]; ok && v != 0 {
			return v
		}
		return def
	}
	profile.matFactor = get(trueValueStructureTypeID, 2600, 1) *
		(1 + get(trueValueRigTypeID, 2594, 0)/100*get(trueValueRigTypeID, 2357, 1))
	profile.costFactor = get(trueValueStructureTypeID, 2601, 1)

	if row, err := app.queries.GetIndustryCostIndex(ctx, db.GetIndustryCostIndexParams{
		SolarSystemID: trueValueReferenceSystemID, Activity: "manufacturing",
	}); err == nil && row.CostIndex > 0 {
		profile.costIndex = row.CostIndex
		return profile
	}
	// Fallback: the cluster median of every stored
	// manufacturing index -- a typical system's fee level
	// when the reference system has none on file.
	if rows, err := app.queries.ListIndustryCostIndices(ctx); err == nil {
		var values []float64
		for _, row := range rows {
			if row.Activity == "manufacturing" && row.CostIndex > 0 {
				values = append(values, row.CostIndex)
			}
		}
		if len(values) > 0 {
			sort.Float64s(values)
			profile.costIndex = values[len(values)/2]
		}
	}
	return profile
}
