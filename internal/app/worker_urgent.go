package app

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Urgent want drain: a short-cadence loop alongside the minute
// cycle that treats fresh wants as urgent. Proactive coverage
// (market coverage tiers, pilot orbit) should mean a user almost
// never sees a pending state at all; when they do — a type
// outside the coverage set, a pilot id typed directly — the
// click notes a want, and this loop picks it up within seconds
// instead of the click waiting on the next minute cycle.
//
// Etiquette is unchanged: every fetch goes through the same
// gates (20h history refetch gate, priority-ordered pilot drain)
// and the shared fetch lock, and an ESI error limit both stops
// the nudge and silences the loop for urgentErrorBackoff. Per
// nudge the caps are small — a burst of wants drains
// first-in-line-first, a few per tick.
// ---------------------------------------------------------------------------

const (
	urgentTickInterval        = 5 * time.Second
	urgentHistoryPerNudge     = 3
	urgentPilotsPerNudge      = 3
	urgentOrgsPerNudge        = 8 // corp + alliance names are the cheapest ESI calls; a page that queues a handful (alliance member list) should clear in one tick, not four
	urgentTypeDetailsPerNudge = 2
	urgentErrorBackoff        = 2 * time.Minute
	urgentPilotFetchAllowance = 20
)

// runUrgentDrain polls the want queues on the short cadence
// until the worker context ends.
func (app *Application) runUrgentDrain(ctx context.Context) {
	ticker := time.NewTicker(urgentTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runGuarded("urgent drain", func() { app.urgentDrain(ctx) })
		}
	}
}

// urgentDrain runs one nudge unless a recent error limit is
// still holding the loop quiet.
func (app *Application) urgentDrain(ctx context.Context) {
	app.urgentMu.Lock()
	if time.Now().Before(app.urgentHoldUntil) {
		app.urgentMu.Unlock()
		return
	}
	app.urgentMu.Unlock()

	if app.drainUrgentWants(ctx) {
		app.urgentMu.Lock()
		app.urgentHoldUntil = time.Now().Add(urgentErrorBackoff)
		app.urgentMu.Unlock()
		logging.Warnf("worker: urgent drain: ESI error limit; holding nudges for %s", urgentErrorBackoff)
	}
}

// drainUrgentWants fetches what the want queues are holding,
// current-page wants first: pilot name resolutions the topbar
// search is waiting on (each queues the pilot record it names),
// planet names behind any "Planet #<id>" on screen, pilot
// records (the drain query already orders viewed wants ahead of
// the proactively noted orbit, so a name someone is looking at
// jumps the queue), then market history wants
// (gate-respecting), then a couple of type descriptions, and
// finally the guide-price want a kill view left behind (schema
// 027). Every pass spends from the same small allowances as
// before — urgency reorders the work, it never widens it.
// Returns ESI's stop signal. Runs under the shared fetch lock
// so it never races the cycle's passes over the same queue rows.
func (app *Application) drainUrgentWants(ctx context.Context) (limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()

	now := time.Now().UTC()

	allowance := &fetchBudget{left: urgentPilotFetchAllowance}
	if _, ltd := app.drainPilotNameWants(ctx, allowance); ltd {
		return true
	}
	// Planet names: a "Planet #<id>" on the page the user is
	// looking at is a current-page want like any other (schema
	// 025). Idempotent upserts make an overlap with the cycle's
	// planet pass harmless.
	if _, ltd := app.resolvePlanetNames(ctx, allowance); ltd {
		return true
	}
	ids, err := app.queries.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		StaleCutoff: now.Add(-pilotStaleAfter),
		DrainLimit:  urgentPilotsPerNudge,
	})
	if err != nil {
		logging.Errorf("worker: urgent drain: list pilot drains: %v", err)
	} else {
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			if _, ltd := app.drainPilotRecord(ctx, id, allowance, now); ltd {
				return true
			}
		}
	}

	// Organization records: a corporation or alliance page the
	// user is looking at fills in within seconds the same way.
	if _, ltd := app.drainCorporationPass(ctx, allowance, urgentOrgsPerNudge, now); ltd {
		return true
	}
	if _, ltd := app.drainAlliancePass(ctx, allowance, urgentOrgsPerNudge, now); ltd {
		return true
	}

	wants, err := app.queries.ListMarketHistoryWants(ctx, now.Add(-historyWantMaxAge))
	if err != nil {
		logging.Errorf("worker: urgent drain: list history wants: %v", err)
	} else {
		fetched := 0
		for _, wn := range wants {
			if fetched >= urgentHistoryPerNudge || ctx.Err() != nil {
				break
			}
			key := marketKey{RegionID: wn.RegionID, TypeID: wn.TypeID}
			if !app.marketFetchDue(ctx, marketFetchKind("history", key), historyRefetchGate) {
				continue
			}
			fetched++
			if _, ltd := app.fetchAndStoreHistory(ctx, key); ltd {
				return true
			}
		}
	}

	typeIDs, err := app.queries.ListTypeDetailWants(ctx, urgentTypeDetailsPerNudge)
	if err != nil {
		logging.Errorf("worker: urgent drain: list type details: %v", err)
	} else {
		for _, id := range typeIDs {
			if ctx.Err() != nil {
				break
			}
			if _, ltd := app.fetchOneTypeDetail(ctx, id, now); ltd {
				return true
			}
		}
	}

	// Guide prices: a kill view that found no prices to value
	// with left the durable want behind (schema 027); answer it
	// with a guide refresh so kill values stop depending on a
	// Market visit.
	if app.drainGuidePriceWant(ctx) {
		return true
	}
	return false
}

// drainGuidePriceWant answers the durable guide-price want a
// kill view noted (schema 027): refresh the stored guide when
// ESI's cache window allows, then clear the note. The refresh
// self-gates on the meta row's cached_until, so a want noted
// while the guide is still fresh costs no call — and the note
// clears as soon as prices are held by any route. Reports
// whether ESI's error limit stopped the refresh.
func (app *Application) drainGuidePriceWant(ctx context.Context) (limited bool) {
	if _, err := app.queries.GetGuidePriceWant(ctx); err != nil {
		return false // no want on file
	}
	stored, ltd := app.refreshGuidePrices(ctx)
	if ltd {
		return true
	}
	if stored || app.storedGuidePrices(ctx) != nil {
		if err := app.queries.ClearGuidePriceWant(ctx); err != nil {
			logging.Errorf("worker: urgent drain: clear guide price want: %v", err)
		}
	}
	return false
}
