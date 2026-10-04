package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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
// the honest fallback stays until it ages out.
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
			log.Printf("planets: note planet %d: %v", id, err)
		}
	}
}

// resolvePlanetNames drains the due slice of the planet queue:
// pending ids first, then stale renames and stale misses. The
// endpoint is public, so no character or scope is involved; each
// lookup spends from the cycle allowance and a 420/429 stops the
// pass (limited) like every other worker pass. A name the
// per-character warm pass already holds in-process is persisted
// without spending a fetch.
func (app *Application) resolvePlanetNames(ctx context.Context, allowance *fetchBudget) (resolved int, limited bool) {
	now := time.Now().UTC()
	ids, err := app.queries.ListPlanetResolutions(ctx, db.ListPlanetResolutionsParams{
		ResolvedCutoff:  now.Add(-planetRenameWindow).Format(time.RFC3339),
		MissingCutoff:   now.Add(-planetMissingWindow).Format(time.RFC3339),
		ResolutionLimit: maxPlanetResolutionsPerCycle,
	})
	if err != nil {
		log.Printf("worker: planets: list resolutions: %v", err)
		return 0, false
	}
	stamp := now.Format(time.RFC3339)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Already warm in-process (the colonies harvest beat
		// this pass to it): persist, no fetch spent.
		if name, ok := app.esi.CachedPlanetName(ctx, id); ok && name != "" {
			if serr := app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
				PlanetID: id, Name: name, State: esi.PlanetResolved, ResolvedAt: stamp,
			}); serr != nil {
				log.Printf("worker: planets: persist cached name for %d: %v", id, serr)
			} else {
				resolved++
			}
			continue
		}
		if !allowance.take() {
			break
		}
		planet, err := app.esi.FetchPlanet(ctx, id)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: planets: ESI error limit hit resolving planet %d; backing off until next cycle", id)
				return resolved, true
			}
			if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
				// Not a planet: remember the answer so this id
				// isn't re-asked every cycle.
				if serr := app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
					PlanetID: id, Name: "", State: esi.PlanetMissing, ResolvedAt: stamp,
				}); serr != nil {
					log.Printf("worker: planets: record miss for %d: %v", id, serr)
				}
				continue
			}
			log.Printf("worker: planets: resolve planet %d: %v", id, err)
			continue
		}
		if planet.Name == "" {
			continue
		}
		if serr := app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
			PlanetID: id, Name: planet.Name, State: esi.PlanetResolved, ResolvedAt: stamp,
		}); serr != nil {
			log.Printf("worker: planets: store name for %d: %v", id, serr)
			continue
		}
		app.esi.StorePlanetName(id, planet.Name)
		resolved++
	}
	return resolved, false
}
