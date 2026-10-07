package app

// The worker's side of the public corporation and alliance pages
// (orgpages.go): draining the corporation_records and
// alliance_records queues from ESI's public endpoints. Nothing here
// renders anything.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// refreshCorporationRecords drains the corporation queue inside
// the cycle budget: 'pending' rows first, then ready rows gone
// stale. Called from refreshCycle.
func (app *Application) refreshCorporationRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()
	return app.drainCorporationPass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// refreshAllianceRecords is refreshCorporationRecords for the
// alliance queue.
func (app *Application) refreshAllianceRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()
	return app.drainAlliancePass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// drainCorporationPass fills up to limit due corporation records.
// Callers hold the shared fetch lock (the cycle wrappers above,
// the urgent drain).
func (app *Application) drainCorporationPass(ctx context.Context, allowance *fetchBudget, limit int, now time.Time) (drained int, limited bool) {
	ids, err := app.queries.ListCorporationDrains(ctx, db.ListCorporationDrainsParams{
		StaleCutoff: now.Add(-orgStaleAfter),
		DrainLimit:  int64(limit),
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
		StaleCutoff: now.Add(-orgStaleAfter),
		DrainLimit:  int64(limit),
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
// anywhere). A 404 settles the record as 'missing'.
func (app *Application) drainCorporationRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {

	if !allowance.take() {
		return false, false
	}
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: corporation records: ESI error limit hit resolving corporation %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
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
// record: the profile plus the member-corporation list, with as
// many member names baked in as the pass budget allows (the rest
// resolve through the corporation queue, like pilot employment
// histories). A 404 settles the record as 'missing'.
func (app *Application) drainAllianceRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {

	if !allowance.take() {
		return false, false
	}
	var alliance esi.Alliance
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: alliance records: ESI error limit hit resolving alliance %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
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
			nameFetches := 0
			for _, corpID := range corpIDs {
				member := allianceMemberCorp{ID: corpID}
				if name, ok := app.esi.CachedCorpName(corpID); ok && name != "" {
					member.Name = name
				} else if nameFetches < maxAllianceMemberNamesPerDrain && allowance.take() {
					nameFetches++
					var corp esi.Corporation
					if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", corpID), &corp); err != nil {
						if errors.Is(err, esi.ErrErrorLimit) {
							return false, true
						}
						logging.Errorf("worker: alliance records: fetch member corporation %d for alliance %d: %v", corpID, id, err)
					} else {
						member.Name = corp.Name
						app.esi.StoreCorpName(corpID, corp.Name)
					}
				}
				payload.Corporations = append(payload.Corporations, member)
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
