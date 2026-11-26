package doctrines

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// A doctrine fit's search text (its module and charge names, lower
// case, one per line) and its own tags (one per line) are worked out
// once, when the fit is added, and stored on the fit (schema 186): the
// loadouts browser reads them instead of decoding every fit's items
// and resolving their names on each view. Fits stored before that step,
// and seeded ones, are worked out by a worker pass in small batches.

// fitSearchTypeIDs is every type a fit's search text names: its modules
// and the charges it carries.
func fitSearchTypeIDs(doc fitview.FitDoc) []int64 {
	ids := make([]int64, 0, len(doc.Items)+len(doc.Charges))
	for _, item := range doc.Items {
		ids = append(ids, item.TypeID)
	}
	for chargeID := range doc.Charges {
		ids = append(ids, chargeID)
	}
	return ids
}

// fitSearchFields is a fit's search text and own tags, given the names
// of the types it holds.
func fitSearchFields(doc fitview.FitDoc, names map[int64]string) (search, tags string) {
	var mods []string
	for _, item := range doc.Items {
		if name := names[item.TypeID]; name != "" {
			mods = append(mods, name)
		}
	}
	for _, chargeID := range doc.LoadedCharges() {
		if name := names[chargeID]; name != "" {
			mods = append(mods, name)
		}
	}
	return strings.ToLower(strings.Join(mods, "\n")), strings.Join(doc.Tags, "\n")
}

// splitLines is a stored one-per-line field as a list.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// storeFitSearch works out and stores the search fields of the fits
// given (id to decoded items), resolving every name in one batched
// lookup. It reports how many it stored.
func (app *mod) storeFitSearch(ctx context.Context, docs map[int64]fitview.FitDoc) int {
	var typeIDs []int64
	for _, doc := range docs {
		typeIDs = append(typeIDs, fitSearchTypeIDs(doc)...)
	}
	names := app.deps.CachedTypeNames(ctx, typeIDs)
	stored := 0
	for id, doc := range docs {
		search, tags := fitSearchFields(doc, names)
		if err := app.queries.SetDoctrineFitSearch(ctx, db.SetDoctrineFitSearchParams{
			ID: id, SearchText: sql.NullString{String: search, Valid: true}, FitTags: sql.NullString{String: tags, Valid: true},
		}); err != nil {
			logging.Errorf("doctrines: store search text of fit %d: %v", id, err)
			continue
		}
		stored++
	}
	return stored
}

// fitSearchBatch is how many fits the worker pass works out at a time.
const fitSearchBatch = 200

// foldFitSearch is the worker pass: fits stored without search fields
// get them, a batch per cycle, oldest first.
func foldFitSearch(ctx context.Context, deps module.Deps, _ module.Cycle) (int, bool) {
	app := newMod(deps)
	rows, err := app.queries.ListDoctrineFitsWithoutSearch(ctx, fitSearchBatch)
	if err != nil {
		logging.Errorf("doctrines: fits without search text: %v", err)
		return 0, false
	}
	if len(rows) == 0 {
		return 0, false
	}
	docs := make(map[int64]fitview.FitDoc, len(rows))
	for _, row := range rows {
		var doc fitview.FitDoc
		_ = json.Unmarshal([]byte(row.ItemsJson), &doc)
		docs[row.ID] = doc
	}
	return app.storeFitSearch(ctx, docs), false
}
