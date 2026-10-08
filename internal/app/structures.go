package app

import (
	"context"
	"time"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Player structure names. GET /universe/structures/{id}/ answers
// 401 without a token and needs esi-universe.read_structures.v1
// (in eveScopes since the Phase 3 build), so names can only ever
// come from a background lookup — pages must never wait on one.
// The queue lives in the structure_names table: worker-computed
// views and the market book view note the structure ids they meet
// (noteStructureIDs), the worker resolves the due ones in the
// background (resolveStructureNames), and every "Structure #<id>"
// fallback in the render layer reads the cache first. Resolved
// names re-check after 30 days (structures can be renamed);
// negative answers negative-cache for 24 hours so private
// structures aren't re-asked every cycle.
//
// Resolution tiers (v0.3.08):
//  1. Multi-character attempts — every linked character whose
//     login granted the scope may be asked, corp-mates of the
//     owning corporation first, then most recently active. One
//     character's 403 never poisons the cache: a negative answer
//     is recorded only after every candidate has answered no.
//     The first success warms the shared cache for everyone.
//  2. Corporation structures — the corp structure list already
//     carries names, so a due id the list covers resolves with
//     no per-structure call at all (provenance 'corp').
//  3. Community dataset — plumbed only: cached names record
//     their provenance ('source', schema 023) and ESI truth
//     outranks the community tier, but no dataset ships and no
//     name is ever invented. Unresolved ids keep the honest
//     "Structure #<id>" floor.
//
// This file is what pages use: the queue note and the cached-name
// lookups. Resolution itself is the worker's, in
// structures_worker.go.
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
			logging.Errorf("structures: note structure %d: %v", id, err)
		}
	}
}

// resolvedStructureTitle is the render tier: the structure's
// cached name when the worker has resolved it, "" otherwise.
func (app *Application) resolvedStructureTitle(ctx context.Context, structureID int64) string {
	name, _ := app.esi.CachedStructureName(ctx, structureID)
	return name
}

// structureLinkable reports whether the structure page has
// something real to show for this id: a resolved name, or stored
// context (owner/system/type) from a corporation snapshot. The
// link policy only links structure titles then — an unresolved
// "Structure #<id>" stays text rather than pointing at a page
// that would only say "not yet". Cache-only.
func (app *Application) structureLinkable(ctx context.Context, structureID int64) bool {
	if name := app.resolvedStructureTitle(ctx, structureID); name != "" {
		return true
	}
	_, err := app.queries.GetStructureContext(ctx, structureID)
	return err == nil
}
