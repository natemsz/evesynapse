package app

// Sov fights: the kills fought in each sovereignty contest. A contest
// the campaigns mirror lists is a fight (schema 156, kept after ESI
// drops the contest); a kill belongs to it when it lands in the
// contested constellation from sovFightLead before the timer until the
// contest ends. Kills are filed twice over: at ingest, against an
// in-memory index of the open fights, so the sov tracker streams them
// as they land; and by a catch-up sweep on every killboard pass, for
// kills stored before a contest was seen and any the ingest missed.
// The sweep also recounts each open fight's stored figures, so the
// pages read stored rows only.

import (
	"context"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// sovFightLead is how long before a contest's timer its kills start
// counting: the fleets form up and fight for the grid first.
const sovFightLead = time.Hour

// sovFightTail is how long after a fight ends the sweep still files
// its kills and recounts it: a killmail can arrive late.
const sovFightTail = 2 * time.Hour

type sovFightRef struct {
	CampaignID int64
	DefenderID int64
	From       time.Time
}

// sovFightIndex is the open fights by constellation.
type sovFightIndex struct {
	byCons map[int64][]sovFightRef
}

// sovFightState holds the index; nil until first loaded.
type sovFightState struct {
	idx atomic.Pointer[sovFightIndex]
}

func (app *Application) loadSovFightIndex(ctx context.Context) *sovFightIndex {
	rows, err := app.queries.ListOpenSovFights(ctx)
	if err != nil {
		logging.Warnf("sov fights: load open fights: %v", err)
		return nil
	}
	ix := &sovFightIndex{byCons: map[int64][]sovFightRef{}}
	for _, r := range rows {
		ix.byCons[r.ConstellationID] = append(ix.byCons[r.ConstellationID], sovFightRef{
			CampaignID: r.CampaignID, DefenderID: r.DefenderID, From: r.StartTime.Add(-sovFightLead),
		})
	}
	app.sovFights.idx.Store(ix)
	return ix
}

func (app *Application) sovFightIndexFor(ctx context.Context) *sovFightIndex {
	if ix := app.sovFights.idx.Load(); ix != nil {
		return ix
	}
	return app.loadSovFightIndex(ctx)
}

// syncSovFights follows a campaigns mirror that changed: new
// contests open fights, gone ones end, the sweep files their kills,
// and the ingest's index is rebuilt.
func (app *Application) syncSovFights(ctx context.Context) {
	now := time.Now().UTC()
	if err := app.queries.SyncSovFights(ctx, now); err != nil {
		logging.Errorf("sov fights: sync: %v", err)
		return
	}
	if n, err := app.queries.EndSovFights(ctx, now); err != nil {
		logging.Errorf("sov fights: end: %v", err)
	} else if n > 0 {
		logging.Infof("sov fights: %d contest(s) ended", n)
	}
	app.loadSovFightIndex(ctx)
	app.sweepSovFights(ctx)
}

// sweepSovFights files the stored kills of the open and just-ended
// fights and recounts their figures. Each fight reads its own
// constellation's systems through the kill index, bounded by its
// window.
func (app *Application) sweepSovFights(ctx context.Context) {
	since := time.Now().UTC().Add(-sovFightTail)
	if _, err := app.queries.CorrelateSovFightKills(ctx, db.CorrelateSovFightKillsParams{LeadSecs: sovFightLead.Seconds(), Since: since}); err != nil {
		logging.Errorf("sov fights: file kills: %v", err)
		return
	}
	if err := app.queries.RefreshSovFightStats(ctx, since); err != nil {
		logging.Errorf("sov fights: recount: %v", err)
	}
}

// sovFightSide is how a fight's defender took part in a kill: 1 it
// lost the ship, 2 it was on the attacker list, 0 neither.
func sovFightSide(defender, victimAlliance int64, attackerAlliances []int64) int16 {
	if defender <= 0 {
		return 0
	}
	if victimAlliance == defender {
		return 1
	}
	for _, a := range attackerAlliances {
		if a == defender {
			return 2
		}
	}
	return 0
}

// sovFightKillRef names one fight a streamed kill belongs to.
type sovFightKillRef struct {
	Campaign int64 `json:"campaign"`
	Side     int16 `json:"side"`
}

// fileSovFightKill files one new public kill into the open fights of
// its constellation and streams it to the sov tracker. A kill outside
// every fight costs two map reads.
func (app *Application) fileSovFightKill(ctx context.Context, k db.Kill, attackerAlliances []int64) {
	if k.Npc || !k.Public {
		return
	}
	ix := app.sovFightIndexFor(ctx)
	if ix == nil || len(ix.byCons) == 0 {
		return
	}
	place, ok := app.killSDE(ctx).places[k.SystemID]
	if !ok {
		return
	}
	var refs []sovFightKillRef
	for _, f := range ix.byCons[place.Constellation] {
		if k.KillmailTime.Before(f.From) {
			continue
		}
		side := sovFightSide(f.DefenderID, k.VictimAllianceID, attackerAlliances)
		if err := app.queries.InsertSovFightKill(ctx, db.InsertSovFightKillParams{
			CampaignID: f.CampaignID, KillmailID: k.KillmailID, KillmailTime: k.KillmailTime, SystemID: k.SystemID, Side: side,
		}); err != nil {
			logging.Warnf("sov fights: file kill %d: %v", k.KillmailID, err)
			continue
		}
		refs = append(refs, sovFightKillRef{Campaign: f.CampaignID, Side: side})
	}
	if len(refs) > 0 && app.killLive.count() > 0 {
		app.killLive.publishFightKill(app.newKillNames(ctx).row(k), refs)
	}
}
