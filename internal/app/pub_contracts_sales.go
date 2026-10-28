package app

// ---------------------------------------------------------------------------
// Confirmed sales from the public route. ESI's public contract
// items route (/contracts/public/items/{id}/) answers a contract
// that has left the market in one of two ways: 204 No Content for
// one "expired or recently accepted by player", and 404 for one
// deleted by its issuer (or gone too long for ESI to say). A
// contract asked about before its expiry date cannot have
// expired, so a 204 then is an acceptance: a sale at the
// listing's price, as certain as the public route allows. Every
// pass that used to read "left early" as a likely sale asks this
// once instead (probeContractExit), and only a 204 is ever read
// as sold; a 404 is never read as "not sold", only as no answer.
//
// ESI also answers 403 "Contract not public" for most departed
// contracts (9 of 12 sampled on 2026-10-28, none answering 204).
// That answer is final, since a contract never becomes public
// again, but it says nothing about a sale: it settles the probe as
// closed, which ends the asking and adds no evidence either way.
// Read as transient it was asked again every cycle until the probe
// window closed, about 80 refused calls a minute. Each final
// answer's status is stored with the probe (schema 099), so a later
// finding about what 403 means can be read back from the record.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// contractExit is what the public items route says about a
// contract that left the public list.
type contractExit int

const (
	exitUnknown contractExit = iota // transient answer: ask again later
	exitLive                        // 200: still listed after all
	exitSold                        // 204 before expiry: accepted
	exitGone                        // 404, or 204 at or after expiry: no sale to read
	exitClosed                      // 403 "not public": final, and says nothing about a sale
)

// contractExitFromStatus classifies one items-route answer for a
// contract that expires at expiresAt, asked at now. err is the
// fetch's error (nil on 200).
func contractExitFromStatus(err error, expiresAt, now time.Time) contractExit {
	if err == nil {
		return exitLive
	}
	code, ok := esi.StatusCode(err)
	switch {
	case !ok:
		return exitUnknown
	case code == http.StatusNoContent:
		if !expiresAt.IsZero() && !now.Before(expiresAt) {
			return exitGone // expiry explains the 204
		}
		return exitSold
	case code == http.StatusNotFound:
		return exitGone
	case code == http.StatusForbidden:
		return exitClosed
	}
	return exitUnknown
}

// probeContractExit asks the items route about one contract, and
// reports the status ESI answered with (200 for a listing) for the
// record. The error is ESI's error limit only: callers stop their
// pass on it.
func (app *Application) probeContractExit(ctx context.Context, contractID int64, expiresAt, now time.Time) (exit contractExit, status int64, err error) {
	_, _, ferr := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/contracts/public/items/%d/", contractID))
	if errors.Is(ferr, esi.ErrErrorLimit) {
		return exitUnknown, 0, ferr
	}
	status = http.StatusOK
	if code, ok := esi.StatusCode(ferr); ok {
		status = int64(code)
	}
	return contractExitFromStatus(ferr, expiresAt, now), status, nil
}

const (
	// abyssalSaleProbesPerCycle bounds sale probes in one pass.
	// A deleted contract answers 404, which costs error budget,
	// so the probe is paced like the dogma probe.
	abyssalSaleProbesPerCycle = 40
	// abyssalSaleProbeWindow: ESI answers 204 for an accepted
	// contract only for a while after it leaves; older
	// departures are not asked about.
	abyssalSaleProbeWindow = 12 * time.Hour
)

// probeAbyssalSales asks each abyssal exchange listing that left
// the market before its expiry what happened to it, once. A sale
// stamps sold_at on its sightings (the corpus then reads the
// price as a sale, not an ask); any other definite answer stamps
// only the probe. Transient answers leave it for the next pass.
func (app *Application) probeAbyssalSales(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	now := time.Now().UTC()
	due, err := app.queries.ListAbyssalSaleProbes(ctx, db.ListAbyssalSaleProbesParams{
		Since: now.Add(-abyssalSaleProbeWindow), MaxRows: abyssalSaleProbesPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: abyssal sales: due list: %v", err)
		return 0, false
	}
	sold := 0
	for _, d := range due {
		if app.esi.ErrorBudgetLow() || !allowance.Take() {
			break
		}
		// The departure stamp already ruled expiry out
		// ("vanished" means it left before date_expired).
		exit, status, err := app.probeContractExit(ctx, d.ContractID, time.Time{}, now)
		if err != nil {
			return stored, true
		}
		switch exit {
		case exitSold:
			if err := app.queries.MarkAbyssalSightingsSold(ctx, db.MarkAbyssalSightingsSoldParams{
				SoldAt: d.DepartedAt, ContractID: d.ContractID,
			}); err != nil {
				logging.Errorf("worker: abyssal sales: mark %d sold: %v", d.ContractID, err)
				continue
			}
			sold++
		case exitLive, exitGone, exitClosed:
			if err := app.queries.MarkAbyssalSaleProbed(ctx, db.MarkAbyssalSaleProbedParams{
				ProbedAt: now, Status: status, ContractID: d.ContractID,
			}); err != nil {
				logging.Errorf("worker: abyssal sales: mark %d probed: %v", d.ContractID, err)
				continue
			}
		default:
			continue
		}
		stored++
	}
	if stored > 0 {
		logging.Infof("worker: abyssal sales: %d departures asked, %d confirmed sold", stored, sold)
	}
	return stored, false
}
