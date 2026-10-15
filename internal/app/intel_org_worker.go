package app

// The worker's side of the public corporation and alliance pages
// (intel_orgpages.go): draining the corporation_records and
// alliance_records queues from ESI's public endpoints. Nothing here
// renders anything.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// refreshCorporationRecords drains the corporation queue inside
// the cycle budget: 'pending' rows first, then ready rows gone
// stale. Called from refreshCycle.
func (app *Application) refreshCorporationRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.orgFetchMu.Lock()
	defer app.orgFetchMu.Unlock()
	return app.drainCorporationPass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// refreshAllianceRecords is refreshCorporationRecords for the
// alliance queue.
func (app *Application) refreshAllianceRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.orgFetchMu.Lock()
	defer app.orgFetchMu.Unlock()
	return app.drainAlliancePass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// allianceSeedInterval is how often the worker re-lists EVE's
// alliances. New alliances form a few times a week, and the list
// call is one fetch, so a daily pass keeps the universe complete
// at no real cost.
const allianceSeedInterval = 24 * time.Hour

// seedAllianceUniverse queues a public record for every alliance
// in EVE, not just the ones somebody has visited: one list call
// (GET /alliances/) and one bulk queue write. Seeded wants carry
// the lowest closeness, so anything a person is actually looking
// at still drains first, and the upsert never demotes an existing
// want. The gate is in memory; a restart simply re-seeds once,
// which the idempotent upsert makes harmless.
func (app *Application) seedAllianceUniverse(ctx context.Context, allowance *fetchBudget) (int, bool) {
	app.allianceSeedMu.Lock()
	due := time.Since(app.lastAllianceSeed) >= allianceSeedInterval
	app.allianceSeedMu.Unlock()
	if !due {
		return 0, false
	}
	if !allowance.take() {
		return 0, false
	}
	var ids []int64
	if err := app.esi.Get(ctx, "", "/alliances/", &ids); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return 0, true
		}
		logging.Errorf("worker: alliance seeding: list alliances: %v", err)
		return 0, false
	}
	now := time.Now().UTC()
	if err := app.queries.UpsertAllianceWants(ctx, db.UpsertAllianceWantsParams{
		AllianceIds: ids, Priority: wantHop, NotedAt: now,
	}); err != nil {
		logging.Errorf("worker: alliance seeding: queue alliances: %v", err)
		return 0, false
	}
	app.allianceSeedMu.Lock()
	app.lastAllianceSeed = now
	app.allianceSeedMu.Unlock()
	return len(ids), false
}

// drainCorporationPass fills up to limit due corporation records.
// Callers hold the organization lock (the cycle wrappers above,
// the urgent drain).
func (app *Application) drainCorporationPass(ctx context.Context, allowance *fetchBudget, limit int, now time.Time) (drained int, limited bool) {
	ids, err := app.queries.ListCorporationDrains(ctx, db.ListCorporationDrainsParams{
		TypingPriority: wantTyping,
		TypingAssumed:  typingAssumedHit,
		TypingWeight:   typingAssumedWeight,
		TypingSince:    now.Add(-typingLearnWindow),
		StaleCutoff:    now.Add(-orgStaleAfter),
		TypingLimit:    int64(app.typingSlice()),
		DrainLimit:     int64(limit),
	})
	if err != nil {
		logging.Errorf("worker: corporation records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainCorporationRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// drainAlliancePass is drainCorporationPass for alliances.
func (app *Application) drainAlliancePass(ctx context.Context, allowance *fetchBudget, limit int, now time.Time) (drained int, limited bool) {
	ids, err := app.queries.ListAllianceDrains(ctx, db.ListAllianceDrainsParams{
		TypingPriority: wantTyping,
		TypingAssumed:  typingAssumedHit,
		TypingWeight:   typingAssumedWeight,
		TypingSince:    now.Add(-typingLearnWindow),
		StaleCutoff:    now.Add(-orgStaleAfter),
		TypingLimit:    int64(app.typingSlice()),
		DrainLimit:     int64(limit),
	})
	if err != nil {
		logging.Errorf("worker: alliance records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainAllianceRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// drainCorporationRecord assembles and stores one corporation's
// public record from ESI's public endpoints (no token involved
// anywhere). A 404 or 422 settles the record as 'missing': both
// are ESI's definitive answer that the ID is not a corporation.
func (app *Application) drainCorporationRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {
	// Swarm first: a dedicated node's budget before this one's.
	if app.swarmDefersFetch(ctx, swarmKindCorporation, strconv.FormatInt(id, 10), now) {
		return false, false
	}

	if !allowance.take() {
		return false, false
	}
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: corporation records: ESI error limit hit resolving corporation %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && (code == http.StatusNotFound || code == http.StatusUnprocessableEntity) {
			if serr := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
				CorporationID: id, Payload: "", State: orgStateMissing, FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: corporation records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: corporation records: fetch corporation %d: %v", id, err)
		return false, false
	}
	app.esi.StoreCorpName(id, corp.Name)

	payload := corporationRecordPayload{Corp: corp}
	// One hop out: the alliance this corporation is in.
	if corp.AllianceID > 0 {
		if err := app.wantAlliance(ctx, corp.AllianceID, wantHop); err != nil {
			logging.Errorf("worker: corporation records: note alliance %d of corporation %d: %v", corp.AllianceID, id, err)
		}
	}

	// The alliance's name + ticker ride along so the corporation
	// page (and every corp line elsewhere) never chases them.
	if corp.AllianceID > 0 && allowance.take() {
		var alliance esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", corp.AllianceID), &alliance); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			logging.Errorf("worker: corporation records: fetch alliance %d for corporation %d: %v", corp.AllianceID, id, err)
		} else {
			payload.Alliance = alliance
			app.esi.StoreAllianceName(corp.AllianceID, alliance.Name)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logging.Errorf("worker: corporation records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: id, Payload: string(encoded), State: orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("worker: corporation records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}

// drainAllianceRecord assembles and stores one alliance's public
// record: the profile plus the member-corporation list, with every
// member name baked in through bulk name resolution, and a queued
// corporation record for each member. A 404 or 422 settles the
// record as 'missing': both are ESI's definitive answer that the
// ID is not an alliance.
func (app *Application) drainAllianceRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {
	// Swarm first: a dedicated node's budget before this one's.
	if app.swarmDefersFetch(ctx, swarmKindAlliance, strconv.FormatInt(id, 10), now) {
		return false, false
	}

	if !allowance.take() {
		return false, false
	}
	var alliance esi.Alliance
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: alliance records: ESI error limit hit resolving alliance %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && (code == http.StatusNotFound || code == http.StatusUnprocessableEntity) {
			if serr := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
				AllianceID: id, Payload: "", State: orgStateMissing, FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: alliance records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: alliance records: fetch alliance %d: %v", id, err)
		return false, false
	}
	app.esi.StoreAllianceName(id, alliance.Name)

	payload := allianceRecordPayload{Alliance: alliance}

	if allowance.take() {
		var corpIDs []int64
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/corporations/", id), &corpIDs); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			// The member list failing is not fatal to the record:
			// store the profile with an empty list.
			logging.Warnf("worker: alliance records: fetch member corporations for %d: %v (storing profile only)", id, err)
		} else {
			// Every member name in one pass: the bulk names
			// endpoint resolves a whole roster (up to 1000 IDs)
			// per call, where the old pass baked a capped few
			// names through one fetch apiece and left the rest
			// to trickle in through the corporation queue.
			names, limited := app.resolveCorpNames(ctx, allowance, corpIDs)
			if limited {
				// ESI said stop: the record waits for a cycle that
				// can name its members, rather than being stored
				// with a roster of blanks.
				return false, true
			}
			for _, corpID := range corpIDs {
				payload.Corporations = append(payload.Corporations, allianceMemberCorp{ID: corpID, Name: names[corpID]})
			}
			// The roster doubles as the corporation universe's
			// index: queue a public record for every member so
			// corporation pages are warm before anyone visits.
			if err := app.queries.UpsertCorporationWants(ctx, db.UpsertCorporationWantsParams{
				CorporationIds: corpIDs, Priority: wantHop, NotedAt: now,
			}); err != nil {
				logging.Errorf("worker: alliance records: queue member corporations for %d: %v", id, err)
			}
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logging.Errorf("worker: alliance records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: id, Payload: string(encoded), State: orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("worker: alliance records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}
