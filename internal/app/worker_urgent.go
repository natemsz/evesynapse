package app

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"
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
	urgentHistoryPerNudge     = 20
	urgentPilotsPerNudge      = 20
	urgentOrgsPerNudge        = 50                     // corp + alliance records are the cheapest ESI calls; a page that queues a batch (an alliance's member list) clears in one tick
	urgentOrgFetchAllowance   = 2 * urgentOrgsPerNudge // their own budget: pilot work earlier in the tick can't starve them
	urgentTypeDetailsPerNudge = 15
	urgentErrorBackoff        = 2 * time.Minute
	urgentPilotFetchAllowance = 150
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
// organization records first (their own lock and budget), then
// current-page wants: pilot name resolutions the topbar
// search is waiting on (each queues the pilot record it names),
// planet names behind any "Planet #<id>" on screen, pilot
// records (the drain query already orders viewed wants ahead of
// the proactively noted orbit, so a name someone is looking at
// jumps the queue), then market history wants
// (gate-respecting), then a couple of type descriptions, and
// finally the guide-price want a kill view left behind (schema
// 027). Every pass spends from the same small allowances as
// before — urgency reorders the work, it never widens it.
// Returns ESI's stop signal: the error limit only; a rate limit
// stops just the pass that met it. Everything after the
// organizations runs under the shared fetch lock, taken only if
// free, so it never races the cycle's passes over the same rows.
func (app *Application) drainUrgentWants(ctx context.Context) (limited bool) {
	now := time.Now().UTC()
	// hard: a pass stopped on a limit stops the whole nudge only for
	// ESI's application-wide error limit; a rate limit is one bucket's,
	// and the next pass uses another.
	hard := func(ltd bool) bool { return ltd && app.esi.HardLimitActive(time.Now()) }

	// Organization records first, under their own lock and budget: a
	// corporation or alliance name on the page being looked at fills in
	// within a tick, whatever the cycle is busy with.
	app.orgFetchMu.Lock()
	orgAllowance := &fetchBudget{left: urgentOrgFetchAllowance}
	_, corpLtd := app.drainCorporationPass(ctx, orgAllowance, urgentOrgsPerNudge, now)
	_, allianceLtd := app.drainAlliancePass(ctx, orgAllowance, urgentOrgsPerNudge, now)
	app.orgFetchMu.Unlock()
	if hard(corpLtd) || hard(allianceLtd) {
		return true
	}

	// Intel reports waiting on names and affiliations next
	// (intel_analysis.go): a pasted report is understood within a
	// tick. Two small bulk calls at most; under its own lock.
	if func() bool {
		app.intelAnalysisMu.Lock()
		defer app.intelAnalysisMu.Unlock()
		return app.drainIntelAnalysis(ctx)
	}() {
		return true
	}

	// Market history next, under its own lock: the item page being
	// looked at gets its chart within a tick even while a long cycle
	// pass (a market sweep) holds the fetch lock.
	if func() bool {
		app.histFetchMu.Lock()
		defer app.histFetchMu.Unlock()
		wants, err := app.queries.ListMarketHistoryWants(ctx, db.ListMarketHistoryWantsParams{
			LastRequestedAt: now.Add(-historyWantMaxAge),
			TypingPriority:  wantTyping,
			TypingAssumed:   typingAssumedHit,
			TypingWeight:    typingAssumedWeight,
			TypingSince:     now.Add(-typingLearnWindow),
			TypingLimit:     int64(app.typingSlice()),
		})
		if err != nil {
			logging.Errorf("worker: urgent drain: list history wants: %v", err)
		} else {
			// The candidate gate, as in the cycle pass: a type
			// ESI does not know can only ever be answered 404.
			fetchable := app.historyFetchableTypes(ctx)
			fetched := 0
			for _, wn := range wants {
				if fetched >= urgentHistoryPerNudge || ctx.Err() != nil {
					break
				}
				if fetchable != nil && !fetchable[wn.TypeID] {
					continue
				}
				key := marketKey{RegionID: wn.RegionID, TypeID: wn.TypeID}
				if !app.marketFetchDue(ctx, marketFetchKind("history", key), historyRefetchGate) {
					continue
				}
				fetched++
				if app.urgentHistory(ctx, key) {
					continue
				}
				// No peer had it yet: a dedicated node that warms
				// history gets its grace before this budget is spent.
				if app.deferFetch(ctx, module.FetchKindHistory, fmt.Sprintf("%d/%d", key.RegionID, key.TypeID), now) {
					continue
				}
				if _, ltd := app.fetchAndStoreHistory(ctx, key); ltd {
					if hard(ltd) {
						return true
					}
					break
				}
			}
		}

		return false
	}() {
		return true
	}

	// The rest shares the cycle's lock. A cycle pass holding it (a
	// long market sweep) means this tick skips the rest rather than
	// queueing behind it; the next tick is five seconds away.
	if !app.fetchMu.TryLock() {
		return false
	}
	defer app.fetchMu.Unlock()

	allowance := &fetchBudget{left: urgentPilotFetchAllowance}
	if _, ltd := app.drainPilotNameWants(ctx, allowance); hard(ltd) {
		return true
	}
	// Planet names: a "Planet #<id>" on the page the user is
	// looking at is a current-page want like any other (schema
	// 025). Idempotent upserts make an overlap with the cycle's
	// planet pass harmless.
	if _, ltd := app.resolvePlanetNames(ctx, allowance); hard(ltd) {
		return true
	}
	ids, err := app.queries.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		TypingPriority: wantTyping,
		TypingAssumed:  typingAssumedHit,
		TypingWeight:   typingAssumedWeight,
		TypingSince:    now.Add(-typingLearnWindow),
		StaleCutoff:    now.Add(-pilotStaleAfter),
		TypingLimit:    int64(app.typingSlice()),
		DrainLimit:     urgentPilotsPerNudge,
	})
	if err != nil {
		logging.Errorf("worker: urgent drain: list pilot drains: %v", err)
	} else {
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			if _, ltd := app.drainPilotRecord(ctx, id, allowance, now); ltd {
				if hard(ltd) {
					return true
				}
				break
			}
		}
	}

	typeIDs, err := app.queries.ListTypeDetailWants(ctx, db.ListTypeDetailWantsParams{
		TypingPriority: wantTyping,
		TypingAssumed:  typingAssumedHit,
		TypingWeight:   typingAssumedWeight,
		TypingSince:    now.Add(-typingLearnWindow),
		TypingLimit:    int64(app.typingSlice()),
		RowLimit:       urgentTypeDetailsPerNudge,
	})
	if err != nil {
		logging.Errorf("worker: urgent drain: list type details: %v", err)
	} else {
		for _, id := range typeIDs {
			if ctx.Err() != nil {
				break
			}
			if _, ltd := app.fetchOneTypeDetail(ctx, id, now); ltd {
				if hard(ltd) {
					return true
				}
				break
			}
		}
	}

	// Guide prices: a kill view that found no prices to value
	// with left the durable want behind (schema 027); answer it
	// with a guide refresh so kill values stop depending on a
	// Market visit.
	return hard(app.drainGuidePriceWant(ctx))
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
