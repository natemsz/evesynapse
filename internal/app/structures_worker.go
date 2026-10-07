package app

// Structure-name resolution: the worker's side of structures.go. It
// resolves the due ids in the structure_names queue (asking each
// character that may know, in order), records names by where they
// came from, and keeps the facts corporation structure lists carry.

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
func (app *Application) storeStructureName(ctx context.Context, structureID int64, name, state, source string, at time.Time) bool {
	if existing, err := app.queries.GetStructureName(ctx, structureID); err == nil &&
		existing.State == esi.StructureResolved &&
		structureSourceRank(existing.Source) > structureSourceRank(source) {
		return false
	}
	if err := app.queries.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: structureID, Name: name, State: state, ResolvedAt: timeSet(at), Source: source,
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
		ResolvedCutoff:  now.Add(-structureRenameWindow),
		MissingCutoff:   now.Add(-structureMissingWindow),
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
idsLoop:
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Tier 2: the corporation structure list already names
		// this one — no per-structure ESI call needed.
		if name, ok := corpIdx.names[id]; ok {
			if app.storeStructureName(ctx, id, name, esi.StructureResolved, esi.StructureSourceCorp, now) {
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
			if app.storeStructureName(ctx, id, info.Name, esi.StructureResolved, esi.StructureSourceESI, now) {
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
			app.storeStructureName(ctx, id, "", esi.StructureMissing, esi.StructureSourceESI, now)
		}
	}
	return resolved, false
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
	now := time.Now().UTC()
	for _, s := range structures {
		if s.StructureID <= 0 {
			continue
		}
		if err := app.queries.SetStructureContext(ctx, db.SetStructureContextParams{
			StructureID:        s.StructureID,
			OwnerCorporationID: s.CorporationID,
			SystemID:           s.SystemID,
			TypeID:             s.TypeID,
			UpdatedAt:          now,
		}); err != nil {
			logging.Errorf("structures: persist context for %d: %v", s.StructureID, err)
		}
		if s.Name != "" {
			if app.storeStructureName(ctx, s.StructureID, s.Name, esi.StructureResolved, esi.StructureSourceCorp, now) {
				app.esi.StoreStructureName(s.StructureID, s.Name)
			}
		}
	}
}
