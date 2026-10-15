package app

import (
	"context"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// deferFetch asks every registered gate whether this fetch should wait
// for a peer. A gate that fails (a panic) is read as "fetch normally":
// a gate can delay a fetch, never cancel or break one.
func (app *Application) deferFetch(ctx context.Context, kind, key string, now time.Time) (deferred bool) {
	for _, m := range registeredModules {
		for _, g := range m.FetchGates {
			if func() (d bool) {
				defer func() {
					if r := recover(); r != nil {
						logging.Errorf("module %s: fetch gate for %s %s: %v; fetching normally", m.ID, kind, key, r)
						d = false
					}
				}()
				return g.Defer(ctx, app, kind, key, now)
			}() {
				return true
			}
		}
	}
	return false
}

// urgentHistory asks the gates to serve one due market history before
// the urgent drain spends ESI; fail-open like deferFetch.
func (app *Application) urgentHistory(ctx context.Context, key marketKey) (served bool) {
	for _, m := range registeredModules {
		for _, g := range m.FetchGates {
			if func() (s bool) {
				defer func() {
					if r := recover(); r != nil {
						logging.Errorf("module %s: urgent history gate: %v; fetching normally", m.ID, r)
						s = false
					}
				}()
				return g.UrgentHistory(ctx, app, key.RegionID, key.TypeID)
			}() {
				return true
			}
		}
	}
	return false
}

// runCycleJobs runs the module jobs of one cycle phase under the cycle's
// own accounting: each is a pass (it counts what it stored and notes a
// limit) and is timed under its own name.
func (app *Application) runCycleJobs(ctx context.Context, c *cycleState, phase module.CyclePhase, characters []db.Character, clock *phaseClock) {
	for _, m := range registeredModules {
		for _, job := range m.CycleJobs {
			if job.At != phase || job.Run == nil {
				continue
			}
			c.pass(job.Describe, func() (int, bool) {
				var stored int
				var limited bool
				func() {
					defer recoverWorkerPanic(job.Name)
					stored, limited = job.Run(ctx, app, module.Cycle{Budget: c.allowance, Characters: characters, Now: time.Now()})
				}()
				return stored, limited
			})
			clock.mark(job.Name, time.Now())
		}
	}
}
