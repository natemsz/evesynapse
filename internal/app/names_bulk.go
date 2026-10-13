package app

import (
	"context"
	"errors"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// Bulk name resolution through ESI's POST /universe/names/: up to
// 1000 IDs per call, against the one call per name the rest of the
// worker spends. Resolved names land in the shared name caches by
// category, so pages, search and every other consumer see them too.

const universeNamesChunk = 1000

type universeName struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// resolveUniverseNames resolves ids in bulk and returns the names
// it settled, keyed by ID. The endpoint answers 404 for the whole
// batch when even one ID does not exist, so a failed batch splits
// and retries its halves; a lone ID that still fails names nothing
// in EVE and is skipped. Every call spends from the allowance, and
// when the allowance runs out the rest stay unresolved — callers
// carry on with what they have.
func (app *Application) resolveUniverseNames(ctx context.Context, allowance *fetchBudget, ids []int64) map[int64]string {
	out := make(map[int64]string)
	for start := 0; start < len(ids); start += universeNamesChunk {
		end := start + universeNamesChunk
		if end > len(ids) {
			end = len(ids)
		}
		app.resolveNameBatch(ctx, allowance, ids[start:end], out)
		if ctx.Err() != nil {
			return out
		}
	}
	return out
}

func (app *Application) resolveNameBatch(ctx context.Context, allowance *fetchBudget, ids []int64, out map[int64]string) {
	if len(ids) == 0 || !allowance.take() {
		return
	}
	var resolved []universeName
	if err := app.esi.PostJSON(ctx, "/universe/names/", ids, &resolved); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) || len(ids) == 1 {
			return
		}
		mid := len(ids) / 2
		app.resolveNameBatch(ctx, allowance, ids[:mid], out)
		app.resolveNameBatch(ctx, allowance, ids[mid:], out)
		return
	}
	for _, r := range resolved {
		if r.Name == "" {
			continue
		}
		out[r.ID] = r.Name
		switch r.Category {
		case "corporation":
			app.esi.StoreCorpName(r.ID, r.Name)
		case "character":
			app.esi.StoreCharacterName(r.ID, r.Name)
		case "alliance":
			app.esi.StoreAllianceName(r.ID, r.Name)
		}
	}
}

// resolveCorpNames returns the names of the given corporations:
// the shared cache first, one bulk call for the rest.
func (app *Application) resolveCorpNames(ctx context.Context, allowance *fetchBudget, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	seen := make(map[int64]bool, len(ids))
	var missing []int64
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if name, ok := app.esi.CachedCorpName(id); ok && name != "" {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	for id, name := range app.resolveUniverseNames(ctx, allowance, missing) {
		out[id] = name
	}
	if len(missing) > 0 {
		logging.Debugf("names: bulk-resolved %d of %d corporation names", len(out), len(ids))
	}
	return out
}
