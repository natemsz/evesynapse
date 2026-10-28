package app

// ---------------------------------------------------------------------------
// Confirmed sales for contract-only ships, at corpus scale --
// the abyssal corpus's method (pub_contracts_sales.go) pointed
// at ship contracts. The true-value crawl watches eight
// regions an item-fetch at a time; the public-contract corpus
// already sweeps every region, and the pass records the ship
// listings it accepts as observations (corpusListingSamples).
// When such a listing leaves the corpus -- the corpus holds
// live contracts only, so absence is a departure -- this pass
// asks ESI's public items route about it once, under a
// per-cycle fetch allowance like probeAbyssalSales spends:
//
//   204 before expiry: accepted -- a confirmed sale at the
//     ask, stamped sold on the observation, where it joins
//     the evidence as the strongest tier (a "recorded sale").
//   404: never a sale -- the listing vanished; marked
//     vanished when that happened well before its expiry
//     (the likely tier), forgotten when expiry explains it.
//   200: still listed after all; touched and left alone.
//
// Confirmed sales are the evidence the learned ship pricing
// (truevalue_learn.go) weights highest; this is how they
// accumulate at the corpus's scale instead of the crawl's.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// tvShipSaleProbesPerCycle bounds exit probes in one
	// pass, the abyssal probe's bound: a deleted contract
	// answers 404, which costs ESI error budget.
	tvShipSaleProbesPerCycle = 40
	// tvShipSaleProbeWindow: departures whose expiry is
	// further past than this are not asked about -- ESI
	// answers 204 for an accepted contract only for a while
	// after it leaves, and the observation purge forgets
	// the rest.
	tvShipSaleProbeWindow = 12 * time.Hour
)

// probeShipContractSales asks each departed ship listing the
// pass is watching what happened to it, once, spending from
// its own per-cycle allowance and stopping at ESI's error
// limit, exactly as probeAbyssalSales does for abyssals.
func (app *Application) probeShipContractSales(ctx context.Context, shipIDs []int64, now time.Time) {
	if len(shipIDs) == 0 {
		return
	}
	allowance := &fetchBudget{left: tvShipSaleProbesPerCycle}
	due, err := app.queries.ListTVShipSaleProbes(ctx, db.ListTVShipSaleProbesParams{
		ShipTypeIds: shipIDs,
		ProbeAfter:  now.Add(-tvShipSaleProbeWindow),
		MaxRows:     tvShipSaleProbesPerCycle,
	})
	if err != nil {
		logging.Errorf("true values: ship sale probes: due list: %v", err)
		return
	}
	st := &app.trueValuePass
	if st.samples == nil {
		st.samples = make(map[int64][]tvContractSample)
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
				logging.Errorf("true values: ship sale probes: mark %d sold: %v", d.ContractID, err)
				continue
			}
			// The listing copy of this sample leaves the ask
			// tier, as when the crawl's reconcile promotes
			// one: the contract now speaks as a recorded sale.
			st.samples[d.TypeID] = dropContractSample(st.samples[d.TypeID], db.TrueValueContractObservation{
				SamplePrice: d.SamplePrice, FirstSeenAt: d.FirstSeenAt,
			})
			sold++
		case exitLive:
			if err := app.queries.TouchTVContractObservation(ctx, db.TouchTVContractObservationParams{
				ContractID: d.ContractID, LastSeenAt: now,
			}); err != nil {
				logging.Errorf("true values: ship sale probes: touch %d: %v", d.ContractID, err)
				continue
			}
		case exitGone:
			if tvVanishedBeforeExpiry(now, d.ExpiresAt) {
				if err := app.queries.MarkTVContractObservationVanished(ctx, db.MarkTVContractObservationVanishedParams{
					ContractID: d.ContractID, VanishedAt: sql.NullTime{Time: now, Valid: true},
				}); err != nil {
					logging.Errorf("true values: ship sale probes: mark %d vanished: %v", d.ContractID, err)
					continue
				}
				st.samples[d.TypeID] = dropContractSample(st.samples[d.TypeID], db.TrueValueContractObservation{
					SamplePrice: d.SamplePrice, FirstSeenAt: d.FirstSeenAt,
				})
			} else if err := app.queries.DeleteTVContractObservation(ctx, d.ContractID); err != nil {
				logging.Errorf("true values: ship sale probes: forget %d: %v", d.ContractID, err)
				continue
			}
		case exitClosed:
			// Final, but neither a sale nor a vanish: the row
			// leaves the queue without joining any evidence tier,
			// and its listing copy leaves the ask tier, since the
			// contract is no longer listed.
			if err := app.queries.MarkTVContractObservationExit(ctx, db.MarkTVContractObservationExitParams{
				Status: status, ProbedAt: now, ContractID: d.ContractID,
			}); err != nil {
				logging.Errorf("true values: ship sale probes: settle %d: %v", d.ContractID, err)
				continue
			}
			st.samples[d.TypeID] = dropContractSample(st.samples[d.TypeID], db.TrueValueContractObservation{
				SamplePrice: d.SamplePrice, FirstSeenAt: d.FirstSeenAt,
			})
		default:
			continue // a transient answer says nothing either way
		}
		probed++
	}
	if probed > 0 {
		logging.Infof("true values: ship sale probes: %d departures asked, %d confirmed sold", probed, sold)
	}
}
