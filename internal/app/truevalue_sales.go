package app

// ---------------------------------------------------------------------------
// Exits of watched listings, at corpus scale -- the abyssal
// corpus's method (pub_contracts_sales.go) pointed at the thin
// types the true-value pass prices from contracts. The public
// contract sweep already stores every region's live contracts,
// and the pass records the listings it accepts as observations
// (corpusListingSamples). When such a listing leaves the corpus
// -- the corpus holds live contracts only, so absence is a
// departure -- this pass asks ESI's public items route about it
// once, under a per-cycle fetch allowance like probeAbyssalSales
// spends:
//
//   204 before expiry: accepted -- a confirmed sale at the
//     ask, stamped sold on the observation, where it joins
//     the evidence as the strongest tier (a "recorded sale").
//   404: never a sale -- the listing vanished; marked
//     vanished when that happened well before its expiry
//     (the likely tier), forgotten when expiry explains it.
//   200: still listed after all; touched and left alone.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// tvExitProbesPerCycle bounds exit probes in one pass,
	// the abyssal probe's bound: a deleted contract answers
	// 404, which costs ESI error budget.
	tvExitProbesPerCycle = 40
	// tvExitProbeWindow: departures whose expiry is
	// further past than this are not asked about -- ESI
	// answers 204 for an accepted contract only for a while
	// after it leaves, and the observation purge forgets
	// the rest.
	tvExitProbeWindow = 12 * time.Hour
)

// probeContractExits asks each departed listing the pass is
// watching what happened to it, once, spending from its own
// per-cycle allowance and stopping at ESI's error limit,
// exactly as probeAbyssalSales does for abyssals.
func (app *Application) probeContractExits(ctx context.Context, now time.Time) {
	allowance := &fetchBudget{left: tvExitProbesPerCycle}
	due, err := app.queries.ListTVExitProbes(ctx, db.ListTVExitProbesParams{
		ProbeAfter: now.Add(-tvExitProbeWindow),
		MaxRows:    tvExitProbesPerCycle,
	})
	if err != nil {
		logging.Errorf("true values: exit probes: due list: %v", err)
		return
	}
	probed, sold := 0, 0
	for _, d := range due {
		if app.esi.ErrorBudgetLow() || !allowance.Take() {
			break
		}
		exit, status, err := app.probeContractExit(ctx, d.ContractID, d.ExpiresAt, now)
		if err != nil {
			break // ESI's error limit: the rest wait for the next cycle
		}
		switch exit {
		case exitSold:
			if err := app.queries.MarkTVContractObservationSold(ctx, db.MarkTVContractObservationSoldParams{
				ContractID: d.ContractID, VanishedAt: sql.NullTime{Time: now, Valid: true},
			}); err != nil {
				logging.Errorf("true values: exit probes: mark %d sold: %v", d.ContractID, err)
				continue
			}
			sold++
			app.trueValuePass.evidenceMoved = true
		case exitLive:
			if err := app.queries.TouchTVContractObservation(ctx, db.TouchTVContractObservationParams{
				ContractID: d.ContractID, LastSeenAt: now,
			}); err != nil {
				logging.Errorf("true values: exit probes: touch %d: %v", d.ContractID, err)
				continue
			}
		case exitGone:
			if tvVanishedBeforeExpiry(now, d.ExpiresAt) {
				if err := app.queries.MarkTVContractObservationVanished(ctx, db.MarkTVContractObservationVanishedParams{
					ContractID: d.ContractID, VanishedAt: sql.NullTime{Time: now, Valid: true},
				}); err != nil {
					logging.Errorf("true values: exit probes: mark %d vanished: %v", d.ContractID, err)
					continue
				}
				app.trueValuePass.evidenceMoved = true
			} else if err := app.queries.DeleteTVContractObservation(ctx, d.ContractID); err != nil {
				logging.Errorf("true values: exit probes: forget %d: %v", d.ContractID, err)
				continue
			}
		case exitClosed:
			// Final, but neither a sale nor a vanish: the row
			// leaves the queue without joining any evidence tier.
			if err := app.queries.MarkTVContractObservationExit(ctx, db.MarkTVContractObservationExitParams{
				Status: status, ProbedAt: now, ContractID: d.ContractID,
			}); err != nil {
				logging.Errorf("true values: exit probes: settle %d: %v", d.ContractID, err)
				continue
			}
		default:
			continue // a transient answer says nothing either way
		}
		probed++
	}
	if probed > 0 {
		logging.Infof("true values: exit probes: %d departures asked, %d confirmed sold", probed, sold)
	}
}
