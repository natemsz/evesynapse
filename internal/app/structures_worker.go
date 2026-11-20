package app

// Structure-name resolution: the worker's side of structures.go. It
// resolves the due ids in the structure_names queue (asking each
// character that may know, in order), records names by where they
// came from, and keeps the facts corporation structure lists carry.

import (
	"context"
	"errors"
	"evesynapse/internal/app/module"
	json "github.com/goccy/go-json"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// structureSourceRank orders name provenance: ESI truth (the
// authenticated lookup, then the corp structure list) always
// outranks the community tier, so a lower-trust name can never
// overwrite a better one.
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
	if state == esi.StructureResolved && name != "" {
		app.noteEntity(structureID, entityDirectoryEntry{ID: structureID, Name: name, Kind: "structure"}, false)
	}
	return true
}

// corpStructureIndex distils the corporation-structure snapshots
// the worker already keeps into a name/owner index. The corp
// list carries names directly (tier 2), and knowing the owning
// corporation lets tier 1 ask corp-mates first, a character in
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
// predating the scope simply doesn't qualify, the queue waits
// for a re-link, and pages keep the "#<id>" fallback meanwhile.
func (app *Application) structureResolverCandidates(ctx context.Context, characters []db.Character, ownerCorpID int64) []db.Character {
	var out []db.Character
	corpOf := map[int64]int64{}
	for _, ch := range characters {
		if !app.characterMaySync(ctx, ch) || !characterHasScope(ch, structureScope) {
			continue
		}
		if app.refusalSettled(ctx, ch.CharacterID, structureNamesKind) {
			continue // this login's structure lookups are refused and settled
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
	// Refusals older than the missing window are stale: the
	// character may have gained access since.
	if err := app.queries.PruneStructureProbes(ctx, now.Add(-structureMissingWindow)); err != nil {
		logging.Errorf("worker: structures: prune probes: %v", err)
	}
	corpIdx := app.corpStructureIndex(ctx)
	refusals, unreachable := 0, 0
	defer func() {
		if refusals > 0 || unreachable > 0 {
			logging.Infof("worker: structures: %d lookup(s) refused this cycle, %d structure(s) no character can see", refusals, unreachable)
		}
	}()
	settleMissing := func(id int64) {
		app.storeStructureName(ctx, id, "", esi.StructureMissing, esi.StructureSourceESI, now)
		app.clearStructureProbes(ctx, id)
		unreachable++
	}
idsLoop:
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Tier 2: the corporation structure list already names
		// this one, no per-structure ESI call needed.
		if name, ok := corpIdx.names[id]; ok {
			if app.storeStructureName(ctx, id, name, esi.StructureResolved, esi.StructureSourceCorp, now) {
				app.esi.StoreStructureName(id, name)
				app.clearStructureProbes(ctx, id)
				resolved++
			}
			continue
		}
		// Swarm first: a dedicated node's budget before this
		// one's (swarm/warm.go). The free tier above still runs.
		if app.deferFetch(ctx, module.FetchKindStructure, strconv.FormatInt(id, 10), now) {
			continue
		}
		// Tier 1: ask the eligible characters, best bet first,
		// until one can name it. A character that was refused
		// for this structure is not asked again within the
		// missing window, so each cycle moves on to the next
		// ones; every refusal costs ESI error budget.
		candidates := app.structureResolverCandidates(ctx, characters, corpIdx.owner[id])
		if len(candidates) == 0 {
			continue
		}
		asked := map[int64]bool{}
		if prior, err := app.queries.ListStructureProbes(ctx, db.ListStructureProbesParams{StructureID: id, Since: now.Add(-structureMissingWindow)}); err == nil {
			for _, cid := range prior {
				asked[cid] = true
			}
		}
		refusedHere := 0
		settled := false
		for _, ch := range candidates {
			if asked[ch.CharacterID] {
				continue
			}
			if refusedHere >= maxStructureRefusalsPerID {
				continue idsLoop // the rest of its candidates wait for a later cycle
			}
			if refusals >= maxStructureRefusalsPerCycle || app.esiErrorBudgetBelow(structureErrorBudgetReserve) {
				break idsLoop // structure names wait; core sync keeps the budget
			}
			if ctx.Err() != nil || !allowance.take() {
				break idsLoop
			}
			info, err := app.esi.FetchStructure(ctx, ch, id)
			if err != nil {
				if errors.Is(err, esi.ErrErrorLimit) {
					logging.Warnf("worker: structures: ESI error limit hit resolving structure %d; backing off until next cycle", id)
					return resolved, true
				}
				if scopeNamedRefusal(err) {
					// The login itself is refused (a 401, or a
					// 403 naming the scope): settle it for this
					// character and let another candidate try.
					app.recordFetchRefusal(ctx, ch.CharacterID, structureNamesKind, err)
					refusals++
					refusedHere++
					continue
				}
				if code, has := esi.StatusCode(err); has && (code == 403 || code == 404) {
					// This character can't name it; another
					// still might.
					app.recordStructureProbe(ctx, id, ch.CharacterID, now)
					asked[ch.CharacterID] = true
					refusals++
					refusedHere++
					continue
				}
				logging.Errorf("worker: structures: resolve structure %d via character %d: %v", id, ch.CharacterID, err)
				continue
			}
			if info.Name == "" {
				app.recordStructureProbe(ctx, id, ch.CharacterID, now)
				asked[ch.CharacterID] = true
				continue
			}
			if app.storeStructureName(ctx, id, info.Name, esi.StructureResolved, esi.StructureSourceESI, now) {
				app.esi.StoreStructureName(id, info.Name)
				app.storeStructureLookupContext(ctx, id, info, now)
				resolved++
			}
			app.clearStructureProbes(ctx, id)
			settled = true
			break
		}
		// Every candidate has now answered 403/404, this cycle or
		// an earlier one: remember the answer so this id isn't
		// re-asked for a day. Candidates not yet asked (a transient
		// error, or a cap reached) leave the row pending.
		if !settled && allAsked(candidates, asked) {
			settleMissing(id)
		}
	}
	return resolved, false
}

// allAsked reports whether every candidate has a refusal on record.
func allAsked(candidates []db.Character, asked map[int64]bool) bool {
	for _, ch := range candidates {
		if !asked[ch.CharacterID] {
			return false
		}
	}
	return true
}

// recordStructureProbe remembers that a character could not name a
// structure, so later cycles ask the next candidate instead.
func (app *Application) recordStructureProbe(ctx context.Context, structureID, characterID int64, now time.Time) {
	if err := app.queries.RecordStructureProbe(ctx, db.RecordStructureProbeParams{StructureID: structureID, CharacterID: characterID, AskedAt: now}); err != nil {
		logging.Errorf("worker: structures: record probe %d/%d: %v", structureID, characterID, err)
	}
}

// clearStructureProbes drops a settled structure's refusals.
func (app *Application) clearStructureProbes(ctx context.Context, structureID int64) {
	if err := app.queries.ClearStructureProbes(ctx, structureID); err != nil {
		logging.Errorf("worker: structures: clear probes %d: %v", structureID, err)
	}
}

// esiErrorBudgetBelow reports whether ESI's error budget, as its last
// answer reported it, is under reserve for the current window. Low
// priority passes stop there so core sync never runs out.
func (app *Application) esiErrorBudgetBelow(reserve int) bool {
	if app.esi == nil {
		return false
	}
	remain, reset := app.esi.ErrorBudgetStatus()
	return reset != 0 && time.Now().Unix() < reset && remain < reserve
}

// storeStructureLookupContext persists the owner/system/type
// facts a successful per-structure lookup carried in its payload,
// so the structure page renders context for structures no
// corporation snapshot covers (public structures especially).
// The lookup is live ESI truth, the same tier as the corporation
// snapshots that otherwise feed SetStructureContext; the fresher
// write wins. A payload carrying none of the facts (never in
// practice) leaves any existing context row untouched.
func (app *Application) storeStructureLookupContext(ctx context.Context, structureID int64, info esi.UniverseStructure, at time.Time) {
	if info.OwnerID <= 0 && info.SolarSystemID <= 0 && info.TypeID <= 0 {
		return
	}
	if err := app.queries.SetStructureContext(ctx, db.SetStructureContextParams{
		StructureID:        structureID,
		OwnerCorporationID: info.OwnerID,
		SystemID:           info.SolarSystemID,
		TypeID:             info.TypeID,
		UpdatedAt:          at,
	}); err != nil {
		logging.Errorf("worker: structures: store lookup context for %d: %v", structureID, err)
	}
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

func structureSourceRank(source string) int { return module.StructureSourceRank(source) }
