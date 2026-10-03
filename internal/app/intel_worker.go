package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Intel worker pass (module sweep, cluster 4): keeps the global
// public-data store warm. Everything here is public ESI — no
// character token — so unlike the other passes this one runs
// even with no characters linked (the Home status line works
// before a first login).
//
// The pass refreshes the six global snapshots on their ESI cache
// windows, warms war details behind the war ID list (bounded per
// cycle, the killmail-detail pattern), and warms the public name
// caches (corporation/alliance names from war details,
// constellation names from incursions) through the cycle's shared
// warmBudget, so intel can never turn one cycle into an ESI
// marathon either. A 420/429 anywhere stops the pass; the caller
// marks the cycle limited, like every other pass.
// ---------------------------------------------------------------------------

const (
	// maxWarDetailsPerCycle bounds war-detail fetches per worker
	// cycle; a cold start converges over a few cycles instead of
	// bursting hundreds of detail calls at once.
	maxWarDetailsPerCycle = 50
	// warDetailMaxAge is how old an *active* war's detail
	// payload may get before the worker refreshes it (kill and
	// ISK counters move while the war runs). A finished war's
	// payload is frozen; those are never refetched.
	warDetailMaxAge = time.Hour
)

// refreshIntel runs one intel pass, reporting how many payloads
// it stored, how many names it resolved, and whether ESI's error
// limit stopped it.
func (app *Application) refreshIntel(ctx context.Context, budget *warmBudget) (stored, names int, limited bool) {
	// Global snapshots, freshest-contract first: status is tiny
	// and feeds Home; factions are nearly static but ride the
	// same Expires discipline as everything else.
	for _, kind := range globalKindOrder {
		if ctx.Err() != nil {
			return stored, names, false
		}
		if snap, err := app.queries.GetGlobalSnapshot(ctx, kind); err == nil && esi.GlobalSnapshotFresh(snap) {
			continue // still inside ESI's cache window
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("worker: intel: read global snapshot %s: %v", kind, err)
		}

		if _, err := app.esi.FetchAndStoreGlobalSnapshot(ctx, kind); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: intel: ESI error limit hit refreshing %s; backing off until next cycle", kind)
				return stored, names, true
			}
			log.Printf("worker: intel: refresh %s: %v", kind, err)
			continue
		}
		stored++
	}

	// War details behind the ID list.
	var list esi.WarList
	if app.loadGlobalSnapshot(ctx, esi.GlobalWars, &list) {
		fetched, ltd := app.warmWarDetails(ctx, list)
		stored += fetched
		if ltd {
			return stored, names, true
		}
	}

	// Public names for the Intel pages, through the shared
	// per-cycle budget (the same pool the character name pass
	// spends from).
	if !budget.stopped() {
		names = app.warmIntelNames(ctx, budget)
	}
	return stored, names, false
}

// warmWarDetails fetches detail payloads for the war ID list:
// missing ones, plus active wars whose stored payload has aged
// past warDetailMaxAge, at most maxWarDetailsPerCycle fetches.
// It reports how many were stored and whether ESI's error limit
// stopped the pass. Finished wars are immutable — once a stored
// payload carries a finish time it is never refetched.
func (app *Application) warmWarDetails(ctx context.Context, list esi.WarList) (fetched int, limited bool) {
	for _, warID := range list {
		if fetched >= maxWarDetailsPerCycle {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if warID <= 0 {
			continue
		}

		need := false
		if row, err := app.queries.GetWarDetail(ctx, warID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("worker: intel: read war detail %d: %v", warID, err)
				continue
			}
			need = true
		} else {
			var war esi.War
			if err := json.Unmarshal([]byte(row.Payload), &war); err != nil {
				need = true // undecodable payload: replace it
			} else if war.Finished == "" {
				if fetchedAt, perr := time.Parse(time.RFC3339, row.FetchedAt); perr != nil || time.Since(fetchedAt) > warDetailMaxAge {
					need = true // still running; counters move
				}
			}
		}
		if !need {
			continue
		}

		body, _, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/wars/%d/", warID))
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if ctx.Err() == nil {
				log.Printf("worker: intel: war detail %d: %v", warID, err)
			}
			continue
		}
		if err := app.queries.UpsertWarDetail(ctx, db.UpsertWarDetailParams{
			WarID:     warID,
			Payload:   string(body),
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Printf("worker: intel: store war detail %d: %v", warID, err)
			continue
		}
		fetched++
	}
	return fetched, false
}

// warmIntelNames resolves the public names the Intel pages still
// lack — war-party corporations and alliances (from the stored
// war details behind the rendered head of the list) and incursion
// constellations — through the shared warm budget and pool.
func (app *Application) warmIntelNames(ctx context.Context, budget *warmBudget) int {
	corpIDs := make(map[int64]bool)
	allianceIDs := make(map[int64]bool)
	constellationIDs := make(map[int64]bool)

	var list esi.WarList
	if app.loadGlobalSnapshot(ctx, esi.GlobalWars, &list) {
		ids := list
		if len(ids) > maxWarsShown {
			ids = ids[:maxWarsShown]
		}
		for _, warID := range ids {
			row, err := app.queries.GetWarDetail(ctx, warID)
			if err != nil {
				continue // not warmed yet; its names wait a cycle
			}
			var war esi.War
			if err := json.Unmarshal([]byte(row.Payload), &war); err != nil {
				continue
			}
			harvestParty := func(p esi.WarParty) {
				if p.CorporationID > 0 {
					corpIDs[p.CorporationID] = true
				}
				if p.AllianceID > 0 {
					allianceIDs[p.AllianceID] = true
				}
			}
			harvestParty(war.Aggressor)
			harvestParty(war.Defender)
			for _, ally := range war.Allies {
				harvestParty(esi.WarParty{CorporationID: ally.CorporationID, AllianceID: ally.AllianceID})
			}
		}
	}

	var incursions esi.Incursions
	if app.loadGlobalSnapshot(ctx, esi.GlobalIncursions, &incursions) {
		for _, inc := range incursions {
			if inc.ConstellationID > 0 {
				constellationIDs[inc.ConstellationID] = true
			}
		}
	}

	resolved := 0

	var missingCorps []int64
	for _, id := range sortedInt64Keys(corpIDs) {
		if _, ok := app.esi.CachedCorpName(id); !ok {
			missingCorps = append(missingCorps, id)
		}
	}
	resolved += app.runWarmPool(ctx, missingCorps, budget, func(ctx context.Context, id int64) bool {
		return app.warmCorpName(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	var missingAlliances []int64
	for _, id := range sortedInt64Keys(allianceIDs) {
		if _, ok := app.esi.CachedAllianceName(id); !ok {
			missingAlliances = append(missingAlliances, id)
		}
	}
	resolved += app.runWarmPool(ctx, missingAlliances, budget, func(ctx context.Context, id int64) bool {
		return app.warmAllianceName(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	var missingConstellations []int64
	for _, id := range sortedInt64Keys(constellationIDs) {
		if _, ok := app.esi.CachedConstellationName(id); !ok {
			missingConstellations = append(missingConstellations, id)
		}
	}
	resolved += app.runWarmPool(ctx, missingConstellations, budget, func(ctx context.Context, id int64) bool {
		return app.warmConstellationName(ctx, budget, id)
	})

	return resolved
}

func (app *Application) warmCorpName(ctx context.Context, budget *warmBudget, id int64) bool {
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: intel: warm corporation %d: %v", id, err)
		}
		return false
	}
	if corp.Name == "" {
		return false
	}
	app.esi.StoreCorpName(id, corp.Name)
	return true
}

func (app *Application) warmAllianceName(ctx context.Context, budget *warmBudget, id int64) bool {
	var alliance esi.Alliance
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: intel: warm alliance %d: %v", id, err)
		}
		return false
	}
	if alliance.Name == "" {
		return false
	}
	app.esi.StoreAllianceName(id, alliance.Name)
	return true
}

func (app *Application) warmConstellationName(ctx context.Context, budget *warmBudget, id int64) bool {
	var constellation esi.Constellation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/constellations/%d/", id), &constellation); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: intel: warm constellation %d: %v", id, err)
		}
		return false
	}
	if constellation.Name == "" {
		return false
	}
	app.esi.StoreConstellationName(id, constellation.Name)
	return true
}
