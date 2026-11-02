package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Per-location asset values (schema 119). The worker folds each
// character's stored asset snapshot into one row per place the
// character holds things in, priced the house way, so the Assets
// page directory renders stored rows instead of repricing a
// hangar on every view. The fold runs in the worker cycle next to
// the wallet history sample, and only when the snapshot has
// advanced past the last fold.
// ---------------------------------------------------------------------------

// assetLocValue is one place's fold, before it is stored.
type assetLocValue struct {
	LocationID   int64
	LocationType string
	EstValue     float64
	PricedStacks int64
	TotalStacks  int64
	TotalItems   int64
	Ships        int64
	Volume       float64
}

// foldAssetLocationValues groups one character's asset rows by the
// place each is really in (the location of its outermost
// container) and values them. Every row filed under a place counts
// toward its stacks, items and value, fitted modules and cargo
// included; ships and volume count top-level rows only, a fitted
// ship being one hangar entry whose own volume is its size.
//
// The lookups come in as functions so the math is testable without
// a database: isShip decides the ship rows, price values one unit
// of a type (ok=false: no house price, the stack counts as
// unpriced and adds nothing), volume is one unit's stored volume.
func foldAssetLocationValues(items []esi.Asset, isShip func(typeID int64) bool, price func(typeID int64) (float64, bool), volume func(typeID int64) float64) []assetLocValue {
	byID := make(map[int64]esi.Asset, len(items))
	for _, it := range items {
		byID[it.ItemID] = it
	}
	// placeOf walks the containment chain to the outermost item,
	// whose location is the place. The walk is bounded the same
	// way the page's nesting is (assetNestMax), so a damaged or
	// looping snapshot still folds somewhere harmless.
	placeOf := func(it esi.Asset) (int64, string, bool) {
		cur := it
		top := true
		for depth := 0; depth < assetNestMax; depth++ {
			parent, inside := byID[cur.LocationID]
			if !inside || parent.ItemID == cur.ItemID {
				break
			}
			top = false
			cur = parent
		}
		return cur.LocationID, cur.LocationType, top
	}

	byLoc := make(map[int64]*assetLocValue)
	var order []int64
	for _, it := range items {
		locID, locType, _ := placeOf(it)
		row, ok := byLoc[locID]
		if !ok {
			row = &assetLocValue{LocationID: locID, LocationType: locType}
			byLoc[locID] = row
			order = append(order, locID)
		}
		row.TotalStacks++
		row.TotalItems += it.Quantity
		if unit, ok := price(it.TypeID); ok {
			row.PricedStacks++
			row.EstValue += unit * float64(it.Quantity)
		}
	}
	// Ships and volume are properties of the top level: what sits
	// directly in the place.
	for _, it := range items {
		locID, _, top := placeOf(it)
		if !top {
			continue
		}
		row := byLoc[locID]
		if isShip(it.TypeID) {
			row.Ships++
		}
		row.Volume += volume(it.TypeID) * float64(it.Quantity)
	}
	out := make([]assetLocValue, 0, len(order))
	for _, locID := range order {
		out = append(out, *byLoc[locID])
	}
	return out
}

// refreshAssetLocationValues folds one character's stored asset
// snapshot into asset_location_values when the snapshot has
// advanced past the last fold. Best-effort like the wallet
// sampler beside it: a failure logs and leaves the last fold in
// place rather than breaking the sync cycle.
func (app *Application) refreshAssetLocationValues(ctx context.Context, ch db.Character) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapAssets})
	if err != nil {
		return // no asset snapshot yet, nothing to fold
	}
	if latest, err := app.queries.GetAssetLocationValueLatest(ctx, db.GetAssetLocationValueLatestParams{
		UserID: ch.UserID, CharacterID: ch.CharacterID,
	}); err == nil && !snap.FetchedAt.After(latest) {
		return // the stored fold already covers this snapshot
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("worker: asset values watermark for character %d: %v", ch.CharacterID, err)
		return
	}

	var items []esi.Asset
	if err := json.Unmarshal([]byte(snap.Payload), &items); err != nil {
		logging.Errorf("worker: asset values decode for character %d: %v", ch.CharacterID, err)
		return
	}

	typicals := app.TypicalSells(ctx)
	trueValues := app.storedTrueValues(ctx)
	price := func(typeID int64) (float64, bool) {
		return pickValuationPrice(typicals[typeID], estSellOf(trueValues, typeID))
	}

	typeIDs := make([]int64, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, it := range items {
		if !seen[it.TypeID] {
			seen[it.TypeID] = true
			typeIDs = append(typeIDs, it.TypeID)
		}
	}
	shipSet := make(map[int64]bool)
	if ids, err := app.queries.ListSDEShipTypeIDs(ctx, typeIDs); err != nil {
		logging.Errorf("worker: asset values ship types for character %d: %v", ch.CharacterID, err)
	} else {
		for _, id := range ids {
			shipSet[id] = true
		}
	}
	volumes := make(map[int64]float64)
	if rows, err := app.queries.ListSDETypePhysicsByIDs(ctx, typeIDs); err != nil {
		logging.Errorf("worker: asset values volumes for character %d: %v", ch.CharacterID, err)
	} else {
		for _, row := range rows {
			volumes[row.TypeID] = row.Volume
		}
	}

	folded := foldAssetLocationValues(items,
		func(typeID int64) bool { return shipSet[typeID] },
		price,
		func(typeID int64) float64 { return volumes[typeID] })

	now := time.Now().UTC()
	keep := make([]int64, 0, len(folded))
	for _, row := range folded {
		if err := app.queries.UpsertAssetLocationValue(ctx, db.UpsertAssetLocationValueParams{
			UserID: ch.UserID, CharacterID: ch.CharacterID,
			LocationID: row.LocationID, LocationType: row.LocationType,
			EstValue: row.EstValue, PricedStacks: row.PricedStacks,
			TotalStacks: row.TotalStacks, TotalItems: row.TotalItems,
			Ships: row.Ships, Volume: row.Volume, ComputedAt: now,
		}); err != nil {
			logging.Errorf("worker: asset values store for character %d: %v", ch.CharacterID, err)
			return
		}
		keep = append(keep, row.LocationID)
	}
	if len(keep) == 0 {
		err = app.queries.DeleteAssetLocationValuesForCharacter(ctx, db.DeleteAssetLocationValuesForCharacterParams{
			UserID: ch.UserID, CharacterID: ch.CharacterID,
		})
	} else {
		err = app.queries.DeleteAssetLocationValuesExcept(ctx, db.DeleteAssetLocationValuesExceptParams{
			UserID: ch.UserID, CharacterID: ch.CharacterID, KeepLocationIds: keep,
		})
	}
	if err != nil {
		logging.Errorf("worker: asset values sweep for character %d: %v", ch.CharacterID, err)
	}
}
