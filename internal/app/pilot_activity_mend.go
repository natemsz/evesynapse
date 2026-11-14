package app

// The pilot activity mend: one walk per profile over the stored kills
// the pilot took part in, folding the ones the live stream left out.
// The stream used to fold only a kill's victim and final blow, and
// while the history backfill was switched off it folded nothing, so a
// profile could stop at its backfill and read days stale. The walk
// reads the kills store only (no outside calls), from the profile's
// backfill (two years back for a profile never backfilled, the
// attacker rows' retention) to now; the (kill, pilot) markers keep a
// kill the profile already holds from counting twice. Each pass walks
// a bounded share; the cursor survives restarts (schema 157).

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// pilotMendPilots and pilotMendKills bound one pass: a few
	// profiles, a page of kills each.
	pilotMendPilots = 10
	pilotMendKills  = 1000
	// pilotMendBudget stops a pass that runs long; the cursor
	// picks it up next pass.
	pilotMendBudget = 20 * time.Second
	// pilotMendOverlap starts a walk a day before the backfill, so
	// a kill the backfill's last page missed is folded too.
	pilotMendOverlap = 24 * time.Hour
)

// mendPilotActivity runs one pass of the mend.
func (app *Application) mendPilotActivity(ctx context.Context) {
	pilots, err := app.queries.ListPilotsToMend(ctx, pilotMendPilots)
	if err != nil {
		logging.Errorf("pilot activity mend: list: %v", err)
		return
	}
	start := time.Now()
	folded, done := 0, 0
	for _, p := range pilots {
		if time.Since(start) > pilotMendBudget || ctx.Err() != nil {
			break
		}
		after, afterID := time.Now().UTC().Add(-2*365*24*time.Hour), int64(0)
		if p.BackfilledAt.Valid {
			after = p.BackfilledAt.Time.UTC().Add(-pilotMendOverlap)
		}
		if p.MendCursorTime.Valid {
			after, afterID = p.MendCursorTime.Time.UTC(), p.MendCursorID
		}
		rows, err := app.queries.ListPilotMendKills(ctx, db.ListPilotMendKillsParams{
			CharacterID: p.CharacterID, AfterTime: after, AfterID: afterID, MaxRows: pilotMendKills,
		})
		if err != nil {
			logging.Errorf("pilot activity mend: pilot %d kills: %v", p.CharacterID, err)
			continue
		}
		for _, r := range rows {
			ok, err := app.applyPilotPart(ctx, p.CharacterID, pilotKillPart{
				KillmailID: r.KillmailID, Time: r.KillmailTime.UTC(), SystemID: r.SystemID,
				Victim: r.IsVictim, VictimShip: r.VictimShip,
				Attacker: r.IsAttacker, AttackerShip: r.AttackerShip, FinalBlow: r.FinalBlow,
				Gang: int(r.AttackerCount),
			}, true)
			if err != nil {
				logging.Errorf("pilot activity mend: pilot %d kill %d: %v", p.CharacterID, r.KillmailID, err)
				return
			}
			if ok {
				folded++
			}
		}
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			if err := app.queries.SetPilotMendCursor(ctx, db.SetPilotMendCursorParams{
				CharacterID: p.CharacterID, CursorTime: last.KillmailTime, CursorID: last.KillmailID,
			}); err != nil {
				logging.Errorf("pilot activity mend: pilot %d cursor: %v", p.CharacterID, err)
				continue
			}
		}
		if len(rows) < pilotMendKills {
			if err := app.queries.MarkPilotMended(ctx, db.MarkPilotMendedParams{CharacterID: p.CharacterID, MendedAt: time.Now().UTC()}); err != nil {
				logging.Errorf("pilot activity mend: pilot %d done: %v", p.CharacterID, err)
				continue
			}
			done++
		}
	}
	if folded > 0 || done > 0 {
		logging.Infof("pilot activity mend: folded %d missed kill(s), %d profile(s) mended", folded, done)
	}
}
