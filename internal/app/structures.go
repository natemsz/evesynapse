package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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
			logging.Errorf("structures: note structure %d: %v", id, err)
		}
	}
}

// structureSourceRank orders name provenance: ESI truth (the
// authenticated lookup, then the corp structure list) always
// outranks the community tier, so a lower-trust name can never
// overwrite a better one.
func structureSourceRank(source string) int {
	switch source {
	case esi.StructureSourceESI:
		return 3
	case esi.StructureSourceCorp:
		return 2
	case esi.StructureSourceCommunity:
		return 1
	}
	return 0
}

// storeStructureName records a resolution outcome, respecting
// provenance: an existing resolved name from a better-trusted
// source is kept. Reports whether the row was written.
func (app *Application) storeStructureName(ctx context.Context, structureID int64, name, state, source, stamp string) bool {
	if existing, err := app.queries.GetStructureName(ctx, structureID); err == nil &&
		existing.State == esi.StructureResolved &&
		structureSourceRank(existing.Source) > structureSourceRank(source) {
		return false
	}
	if err := app.queries.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: structureID, Name: name, State: state, ResolvedAt: stamp, Source: source,
	}); err != nil {
		logging.Errorf("worker: structures: store %s name for %d: %v", source, structureID, err)
		return false
	}
	return true
}

// corpStructureIndex distils the corporation-structure snapshots
// the worker already keeps into a name/owner index. The corp
// list carries names directly (tier 2), and knowing the owning
// corporation lets tier 1 ask corp-mates first — a character in
// the owning corp is the most likely to hold docking access.
type corpStructureIndex struct {
	names map[int64]string
	owner map[int64]int64
}

func (app *Application) corpStructureIndex(ctx context.Context) corpStructureIndex {
	idx := corpStructureIndex{names: map[int64]string{}, owner: map[int64]int64{}}
	snaps, err := app.queries.ListSnapshotsByKind(ctx, esi.SnapCorpStructures)
	if err != nil {
		logging.Errorf("worker: structures: list corp structure snapshots: %v", err)
		return idx
	}
	for _, snap := range snaps {
		var structures esi.CorpStructures
		if err := json.Unmarshal([]byte(snap.Payload), &structures); err != nil {
			continue
		}
		for _, s := range structures {
			if s.CorporationID > 0 {
				idx.owner[s.StructureID] = s.CorporationID
			}
			if s.Name != "" {
				idx.names[s.StructureID] = s.Name
			}
		}
	}
	return idx
}

// structureResolverCandidates orders the characters whose tokens
// may resolve structure names: syncing characters (parked
// token_dead / owner_changed links are never fetched) whose
// login granted the structure scope. Corp-mates of the owning
// corporation go first, then most recently active; a login
// predating the scope simply doesn't qualify — the queue waits
// for a re-link, and pages keep the "#<id>" fallback meanwhile.
func (app *Application) structureResolverCandidates(ctx context.Context, characters []db.Character, ownerCorpID int64) []db.Character {
	var out []db.Character
	corpOf := map[int64]int64{}
	for _, ch := range characters {
		if !characterSyncs(ch) || !characterHasScope(ch, structureScope) {
			continue
		}
		out = append(out, ch)
		if row, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID); err == nil {
			corpOf[ch.CharacterID] = row.CorporationID
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		iMatch := ownerCorpID > 0 && corpOf[out[i].CharacterID] == ownerCorpID
		jMatch := ownerCorpID > 0 && corpOf[out[j].CharacterID] == ownerCorpID
		if iMatch != jMatch {
			return iMatch
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].CharacterID < out[j].CharacterID
	})
	return out
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
		logging.Errorf("worker: structures: list resolutions: %v", err)
		return 0, false
	}
	if len(ids) == 0 {
		return 0, false
	}
	corpIdx := app.corpStructureIndex(ctx)
	stamp := now.Format(time.RFC3339)
idsLoop:
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Tier 2: the corporation structure list already names
		// this one — no per-structure ESI call needed.
		if name, ok := corpIdx.names[id]; ok {
			if app.storeStructureName(ctx, id, name, esi.StructureResolved, esi.StructureSourceCorp, stamp) {
				app.esi.StoreStructureName(id, name)
				resolved++
			}
			continue
		}
		// Tier 1: ask every eligible character, best bet first,
		// until one can name it.
		candidates := app.structureResolverCandidates(ctx, characters, corpIdx.owner[id])
		negatives := 0
		for _, ch := range candidates {
			if ctx.Err() != nil || !allowance.take() {
				break idsLoop
			}
			info, err := app.esi.FetchStructure(ctx, ch, id)
			if err != nil {
				if errors.Is(err, esi.ErrErrorLimit) {
					logging.Warnf("worker: structures: ESI error limit hit resolving structure %d; backing off until next cycle", id)
					return resolved, true
				}
				if code, has := esi.StatusCode(err); has && (code == 403 || code == 404) {
					// This character can't name it; another
					// still might. Only an exhausted set earns
					// the negative cache entry.
					negatives++
					continue
				}
				logging.Errorf("worker: structures: resolve structure %d via character %d: %v", id, ch.CharacterID, err)
				continue
			}
			if info.Name == "" {
				negatives++
				continue
			}
			if app.storeStructureName(ctx, id, info.Name, esi.StructureResolved, esi.StructureSourceESI, stamp) {
				app.esi.StoreStructureName(id, info.Name)
				resolved++
			}
			negatives = -1 // settled: not an all-negative outcome
			break
		}
		// Every candidate answered 403/404: remember the answer
		// so this id isn't re-asked for a day. A pass interrupted
		// by transient errors leaves the row pending instead.
		if len(candidates) > 0 && negatives == len(candidates) {
			app.storeStructureName(ctx, id, "", esi.StructureMissing, esi.StructureSourceESI, stamp)
		}
	}
	return resolved, false
}

// resolvedStructureTitle is the render tier: the structure's
// cached name when the worker has resolved it, "" otherwise.
func (app *Application) resolvedStructureTitle(ctx context.Context, structureID int64) string {
	name, _ := app.esi.CachedStructureName(ctx, structureID)
	return name
}

// persistStructureContexts distils one corporation-structures
// snapshot into the structure_context store (schema 028): the
// owning corporation, system, and type each entry reports, so the
// structure page renders context from a single row instead of
// re-reading snapshot payloads. A named entry also lands its name
// in structure_names at 'corp' provenance through the usual
// precedence guard -- ESI truth already cached is never demoted.
// Called wherever snapshots are processed (the corp fetch path and
// the worker's name-warming pass); best-effort per entry, like
// every other queue note.
func (app *Application) persistStructureContexts(ctx context.Context, structures esi.CorpStructures) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	for _, s := range structures {
		if s.StructureID <= 0 {
			continue
		}
		if err := app.queries.SetStructureContext(ctx, db.SetStructureContextParams{
			StructureID:        s.StructureID,
			OwnerCorporationID: s.CorporationID,
			SystemID:           s.SystemID,
			TypeID:             s.TypeID,
			UpdatedAt:          stamp,
		}); err != nil {
			logging.Errorf("structures: persist context for %d: %v", s.StructureID, err)
		}
		if s.Name != "" {
			if app.storeStructureName(ctx, s.StructureID, s.Name, esi.StructureResolved, esi.StructureSourceCorp, stamp) {
				app.esi.StoreStructureName(s.StructureID, s.Name)
			}
		}
	}
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
