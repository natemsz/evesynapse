package app

import (
	"context"
	"errors"
	"log"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Player structure names. GET /universe/structures/{id}/ answers
// 401 without a token and needs esi-universe.read_structures.v1
// (in eveScopes since the Phase 3 build), so names can only ever
// come from a background lookup — pages must never wait on one.
// The queue lives in the structure_names table: worker-computed
// views and the market book view note the structure ids they meet
// (noteStructureIDs), the worker resolves the due ones with any
// linked character holding the scope (resolveStructureNames), and
// every "Structure #<id>" fallback in the render layer reads the
// cache first. Resolved names re-check after 30 days (structures
// can be renamed); 403/404 answers negative-cache for 24 hours so
// private structures aren't re-asked every cycle.
// ---------------------------------------------------------------------------

const (
	// structureIDThreshold separates player structure ids (11+,
	// e.g. 1044752365771) from stations (~6×10⁷) and systems
	// (~3×10⁷) in location-id fields that don't carry a kind.
	structureIDThreshold = int64(1_000_000_000_000)
	// maxStructureResolutionsPerCycle bounds structure lookups
	// per worker cycle; a cold backlog converges over a few
	// cycles instead of bursting.
	maxStructureResolutionsPerCycle = 10
	// structureRenameWindow is how long a resolved name is
	// trusted before the worker re-asks.
	structureRenameWindow = 30 * 24 * time.Hour
	// structureMissingWindow is how long a 403/404 answer is
	// trusted before the worker re-asks.
	structureMissingWindow = 24 * time.Hour
	// structureScope is the read scope the authenticated
	// structure lookup needs.
	structureScope = "esi-universe.read_structures.v1"
)

// isStructureID reports whether a bare location id is in the
// player-structure range (as opposed to a station or system id).
func isStructureID(id int64) bool { return id > structureIDThreshold }

// noteStructureIDs records structure ids a view encountered so
// the worker resolves them in the background. Idempotent
// (INSERT OR IGNORE) and best-effort: it's a queue hint, never
// worth failing the view that produced it.
func (app *Application) noteStructureIDs(ctx context.Context, ids ...int64) {
	for _, id := range ids {
		if !isStructureID(id) {
			continue
		}
		if err := app.queries.UpsertStructureSeen(ctx, id); err != nil {
			log.Printf("structures: note structure %d: %v", id, err)
		}
	}
}

// structureResolverCharacter picks the character whose token
// resolves structure names: any syncing character whose login
// granted the structure scope. A login predating the scope simply
// doesn't qualify — the queue waits for a re-link, and pages keep
// the "#<id>" fallback meanwhile.
func (app *Application) structureResolverCharacter(characters []db.Character) (db.Character, bool) {
	for _, ch := range characters {
		if characterSyncs(ch) && characterHasScope(ch, structureScope) {
			return ch, true
		}
	}
	return db.Character{}, false
}

// resolveStructureNames drains the due slice of the structure
// queue: pending ids first, then stale renames and stale misses.
// Each lookup spends from the cycle allowance; a 420/429 stops
// the pass (limited) like every other worker pass.
func (app *Application) resolveStructureNames(ctx context.Context, characters []db.Character, allowance *fetchBudget) (resolved int, limited bool) {
	now := time.Now().UTC()
	ids, err := app.queries.ListStructureResolutions(ctx, db.ListStructureResolutionsParams{
		ResolvedCutoff:  now.Add(-structureRenameWindow).Format(time.RFC3339),
		MissingCutoff:   now.Add(-structureMissingWindow).Format(time.RFC3339),
		ResolutionLimit: maxStructureResolutionsPerCycle,
	})
	if err != nil {
		log.Printf("worker: structures: list resolutions: %v", err)
		return 0, false
	}
	if len(ids) == 0 {
		return 0, false
	}
	resolver, ok := app.structureResolverCharacter(characters)
	if !ok {
		return 0, false
	}
	stamp := now.Format(time.RFC3339)
	for _, id := range ids {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		info, err := app.esi.FetchStructure(ctx, resolver, id)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: structures: ESI error limit hit resolving structure %d; backing off until next cycle", id)
				return resolved, true
			}
			if code, has := esi.StatusCode(err); has && (code == 403 || code == 404) {
				// Private or destroyed: remember the answer so
				// this id isn't re-asked for a day.
				if serr := app.queries.SetStructureName(ctx, db.SetStructureNameParams{
					StructureID: id, Name: "", State: esi.StructureMissing, ResolvedAt: stamp,
				}); serr != nil {
					log.Printf("worker: structures: record miss for %d: %v", id, serr)
				}
				continue
			}
			log.Printf("worker: structures: resolve structure %d: %v", id, err)
			continue
		}
		if info.Name == "" {
			continue
		}
		if err := app.queries.SetStructureName(ctx, db.SetStructureNameParams{
			StructureID: id, Name: info.Name, State: esi.StructureResolved, ResolvedAt: stamp,
		}); err != nil {
			log.Printf("worker: structures: store name for %d: %v", id, err)
			continue
		}
		app.esi.StoreStructureName(id, info.Name)
		resolved++
	}
	return resolved, false
}

// resolvedStructureTitle is the render tier: the structure's
// cached name when the worker has resolved it, "" otherwise.
func (app *Application) resolvedStructureTitle(ctx context.Context, structureID int64) string {
	name, _ := app.esi.CachedStructureName(ctx, structureID)
	return name
}
