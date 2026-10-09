package app

import (
	"context"
	"time"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Planet names. GET /universe/planets/{planet_id}/ is public —
// no token, no scope — and planet names never change, so the
// queue in the planet_names table only needs to converge once:
// PI surfaces note the planet ids they meet (notePlanetIDs),
// the worker resolves the due ones in the background
// (resolvePlanetNames), and every "Planet #<id>" fallback in
// the render layer reads the cache first. Resolved names
// re-check after 90 days (they never change; the re-check is
// hygiene, not a schedule); a 404 negative-caches for 7 days —
// colony planet ids always exist, so a 404 means a bad id, and
// the fallback stays until it ages out.
//
// This file is the queue note pages make. resolvePlanetNames is with
// the rest of the planetary worker code, in planets_worker.go.
// ---------------------------------------------------------------------------

const (
	// maxPlanetResolutionsPerCycle bounds planet lookups per
	// worker cycle; a cold backlog converges over a few cycles
	// instead of bursting.
	maxPlanetResolutionsPerCycle = 20
	// planetRenameWindow is how long a resolved name is trusted
	// before the worker re-asks.
	planetRenameWindow = 90 * 24 * time.Hour
	// planetMissingWindow is how long a 404 answer is trusted
	// before the worker re-asks.
	planetMissingWindow = 7 * 24 * time.Hour
)

// notePlanetIDs records planet ids a view encountered so the
// worker resolves them in the background. Idempotent
// (INSERT OR IGNORE) and best-effort: it's a queue hint, never
// worth failing the view that produced it.
func (app *Application) notePlanetIDs(ctx context.Context, ids ...int64) {
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if err := app.queries.UpsertPlanetSeen(ctx, id); err != nil {
			logging.Errorf("planets: note planet %d: %v", id, err)
		}
	}
}
