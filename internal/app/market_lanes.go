package app

// The public sweep lanes: the hub order book sweep and the public
// contract sweep each run on a goroutine of their own beside the
// worker's one-minute cycle, so a long sweep never holds character
// sync and the rest of the cycle behind it. Both are paced by ESI's
// own cache windows (market_region_sweep.go, pub_contracts.go); the
// lane ticks only decide how soon a due sweep notices it is due.

import (
	"context"
	"time"
)

const (
	// marketLaneTick is how often the hub sweep lane looks for a
	// region whose window just rolled over; a sweep starts within
	// this of the rollover.
	marketLaneTick = 5 * time.Second
	// marketLanePagesPerPass bounds one lane pass's page reads: room
	// for every hub's whole book at once (about 900 pages today).
	marketLanePagesPerPass = 4000
	// contractLaneTick is how often the public contract lane runs
	// its pass: list sweeps on their regions' windows, then the
	// item, bid and probe warming from the pass's allowance.
	contractLaneTick = time.Minute
)

// runMarketLane runs the hub sweep until ctx ends; a pass that
// panics is recovered and the next tick runs again.
func (app *Application) runMarketLane(ctx context.Context) {
	tick := time.NewTicker(marketLaneTick)
	defer tick.Stop()
	for {
		if !app.esi.HardLimitActive(time.Now()) && !app.esi.ErrorBudgetLow() {
			runGuarded("market lane", func() {
				app.sweepRegionStats(ctx, &fetchBudget{left: marketLanePagesPerPass})
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// runContractLane runs the public contract pass until ctx ends,
// recovering a pass that panics like runMarketLane.
func (app *Application) runContractLane(ctx context.Context) {
	tick := time.NewTicker(contractLaneTick)
	defer tick.Stop()
	for {
		if !app.esi.HardLimitActive(time.Now()) && !app.esi.ErrorBudgetLow() {
			runGuarded("contract lane", func() {
				app.refreshPublicContracts(ctx, &fetchBudget{left: pubContractFetchesPerCycle})
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
