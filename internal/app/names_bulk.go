package app

import (
	"context"
	"errors"
	"net/http"

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

// nameBatchOutcome is how one bulk call (and its splits) ended.
type nameBatchOutcome int

const (
	nameBatchDone    nameBatchOutcome = iota // answered, or out of allowance
	nameBatchBackOff                         // ESI or the network failed: try again next cycle
	nameBatchLimited                         // ESI's error limit: the caller must stop
)

// resolveUniverseNamesLimited resolves ids in bulk and returns the
// names it settled, keyed by ID, and whether ESI's error limit
// stopped it.
//
// The endpoint answers 404 for the whole batch when even one ID
// does not exist, so a batch that gets a 404 splits and retries
// its halves; a lone ID that still gets one names nothing in EVE
// and is skipped. Only a 404 splits. Any other failure (a 5xx, a
// timeout, the network) says nothing about the IDs, and asking
// again in halves would spend the cycle's allowance on the same
// failure: the resolve stops there and what is unresolved waits
// for the caller's next cycle. Every call spends from the
// allowance, and when that runs out the rest stay unresolved too.
func (app *Application) resolveUniverseNamesLimited(ctx context.Context, allowance *fetchBudget, ids []int64) (names map[int64]string, limited bool) {
	out := make(map[int64]string)
	for start := 0; start < len(ids); start += universeNamesChunk {
		end := start + universeNamesChunk
		if end > len(ids) {
			end = len(ids)
		}
		switch app.resolveNameBatch(ctx, allowance, ids[start:end], out) {
		case nameBatchLimited:
			return out, true
		case nameBatchBackOff:
			return out, false
		}
		if ctx.Err() != nil {
			return out, false
		}
	}
	return out, false
}

func (app *Application) resolveNameBatch(ctx context.Context, allowance *fetchBudget, ids []int64, out map[int64]string) nameBatchOutcome {
	if len(ids) == 0 || !allowance.take() {
		return nameBatchDone
	}
	var resolved []universeName
	if err := app.esi.PostJSON(ctx, "/universe/names/", ids, &resolved); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return nameBatchLimited
		}
		if code, has := esi.StatusCode(err); !has || code != http.StatusNotFound {
			logging.Warnf("names: bulk lookup of %d IDs failed (%v); leaving them for the next cycle", len(ids), err)
			return nameBatchBackOff
		}
		if len(ids) == 1 {
			return nameBatchDone // names nothing in EVE
		}
		mid := len(ids) / 2
		if outcome := app.resolveNameBatch(ctx, allowance, ids[:mid], out); outcome != nameBatchDone {
			return outcome
		}
		return app.resolveNameBatch(ctx, allowance, ids[mid:], out)
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
	return nameBatchDone
}

// resolveCorpNames returns the names of the given corporations:
// the shared cache first, one bulk call for the rest. limited
// reports ESI's error limit, which the caller must pass on.
func (app *Application) resolveCorpNames(ctx context.Context, allowance *fetchBudget, ids []int64) (names map[int64]string, limited bool) {
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
	resolved, limited := app.resolveUniverseNamesLimited(ctx, allowance, missing)
	for id, name := range resolved {
		out[id] = name
	}
	if len(missing) > 0 {
		logging.Debugf("names: bulk-resolved %d of %d uncached corporation names", len(resolved), len(missing))
	}
	return out, limited
}
