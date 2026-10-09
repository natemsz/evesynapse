package app

import (
	"context"
	"errors"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The names players give their ships and containers. ESI's asset list
// carries only what kind of thing each asset is; the name ("Zoom
// Zoom", not "Loki") comes from a second call, which the worker makes
// so that the Assets page never has to.
//
// Only assets that can meaningfully carry a name are asked about:
// ships, and anything that has something inside it. Everything else
// that ESI marks a singleton (fitted modules, blueprint originals)
// would be thousands of ids for no names.
// ---------------------------------------------------------------------------

const (
	// charAssetNamesKind is the pseudo-kind the outcome of the names
	// call is recorded under, per character.
	charAssetNamesKind = "asset_names"
	// charAssetNamesRefresh: everything nameable is asked about again
	// this often, which is how a rename is noticed. In between, only
	// assets not asked about yet are.
	charAssetNamesRefresh = 24 * time.Hour
	// maxCharAssetNameItemsPerCycle is ESI's limit for one call, and
	// one call is all a cycle makes per character.
	maxCharAssetNameItemsPerCycle = 1000
)

// nameableAssets picks the assets worth asking a name for: assembled
// ships, and singletons with something inside them.
func (app *Application) nameableAssets(ctx context.Context, items []esi.Asset) []int64 {
	typeSet := map[int64]bool{}
	hasContents := map[int64]bool{}
	owned := make(map[int64]bool, len(items))
	for _, it := range items {
		owned[it.ItemID] = true
	}
	for _, it := range items {
		typeSet[it.TypeID] = true
		if owned[it.LocationID] {
			hasContents[it.LocationID] = true
		}
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for id := range typeSet {
		typeIDs = append(typeIDs, id)
	}
	ships := map[int64]bool{}
	if rows, err := app.queries.ListSDEShipTypeIDs(ctx, typeIDs); err != nil {
		logging.Errorf("worker: asset names: ship types: %v", err)
	} else {
		for _, id := range rows {
			ships[id] = true
		}
	}
	var out []int64
	for _, it := range items {
		if it.IsSingleton && it.Quantity == 1 && (ships[it.TypeID] || hasContents[it.ItemID]) {
			out = append(out, it.ItemID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// warmCharacterAssetNames stores the player-given names of one
// character's ships and containers. An asset ESI reports no name for
// is stored with an empty name, so it is not asked about again until
// the daily refresh.
func (app *Application) warmCharacterAssetNames(ctx context.Context, ch db.Character) (stored int, limited bool) {
	items, ok := app.characterAssets(ctx, ch)
	if !ok {
		return 0, false // no asset list yet: nothing to name
	}
	nameable := app.nameableAssets(ctx, items)
	if len(nameable) == 0 {
		return 0, false
	}

	refresh := true
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: charAssetNamesKind}); err == nil {
		age := time.Since(state.AttemptedAt)
		if state.State == fetchStateError && age < roleMissingBackoff {
			return 0, false // it failed recently: wait, do not retry every minute
		}
		refresh = age >= charAssetNamesRefresh
	}

	ask := nameable
	if !refresh {
		known := map[int64]bool{}
		if rows, err := app.queries.ListItemNamesByIDs(ctx, nameable); err == nil {
			for _, row := range rows {
				known[row.ItemID] = true
			}
		}
		ask = nil
		for _, id := range nameable {
			if !known[id] {
				ask = append(ask, id)
			}
		}
	}
	if len(ask) == 0 {
		return 0, false
	}
	if len(ask) > maxCharAssetNameItemsPerCycle {
		ask = ask[:maxCharAssetNameItemsPerCycle]
	}

	names, err := app.esi.FetchCharacterAssetNames(ctx, ch, ask)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: ESI error limit hit warming asset names for character %d", ch.CharacterID)
			return 0, true
		}
		logging.Errorf("worker: asset names for character %d: %v", ch.CharacterID, err)
		app.recordCorpFetchState(ctx, ch.CharacterID, charAssetNamesKind, fetchStateError, err.Error())
		return 0, false
	}
	if refresh {
		app.recordCorpFetchState(ctx, ch.CharacterID, charAssetNamesKind, fetchStateOK, "")
	}

	answered := make(map[int64]string, len(names))
	for _, n := range names {
		name := n.Name
		if name == "None" {
			name = "" // CCP's placeholder for an unnamed item
		}
		answered[n.ItemID] = name
	}
	for _, id := range ask {
		name := answered[id] // absent from the answer is unnamed too
		if err := app.queries.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: id, Name: name}); err != nil {
			logging.Errorf("worker: store item name %d: %v", id, err)
			continue
		}
		if name != "" {
			stored++
		}
	}
	return stored, false
}
