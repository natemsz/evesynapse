package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// What the Ship Replacement module asks core about a fleet. The tab is
// for those with a stake in the fleet's replacement: its handlers, the
// owner's members and the pilots who flew it. It is never public.
// ---------------------------------------------------------------------------

// seesFleetSRP: the viewer can open the fleet's Ship Replacement tab.
// A pilot's public corporation does not make them a member here, and
// neither does belonging to the owner of a fleet kept from them.
func (app *Application) seesFleetSRP(ctx context.Context, op db.Op, v fleetViewer) (member, attended, handles, ok bool) {
	if !moduleCompiledIn("srp") {
		return false, false, false, false
	}
	member, attended = app.fleetStake(ctx, op, v)
	if v.UserID != 0 {
		handles = app.ownerPermits(ctx, v.Standing, opOwner(op), permSRP)
	}
	return member, attended, handles, member || attended || handles
}

// FleetSRPFor is one fleet for its Ship Replacement tab.
func (app *Application) FleetSRPFor(ctx context.Context, opID int64) (module.FleetSRP, bool) {
	op, err := app.queries.GetOp(ctx, opID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: load fleet %d: %v", opID, err)
		}
		return module.FleetSRP{}, false
	}
	v := app.fleetViewerFor(ctx)
	member, _, handles, ok := app.seesFleetSRP(ctx, op, v)
	if !ok {
		return module.FleetSRP{}, false
	}
	owner := opOwner(op)
	out := module.FleetSRP{
		Op: op, Owner: owner, OwnerName: app.ownerName(ctx, owner), UserID: v.UserID,
		Member: member, Handles: handles,
	}
	if v.Pilot != nil {
		out.Pilots = []module.NamedChoice{{ID: v.Pilot.CharacterID, Name: v.Pilot.Name}}
	}
	for _, c := range v.Characters {
		out.Pilots = append(out.Pilots, module.NamedChoice{ID: c.CharacterID, Name: c.Name})
	}
	now := time.Now()
	live, err := app.fleetLiveReadFor(ctx, op, v, now)
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
	}
	// A pilot with no other view of the fleet has no Overview to go to.
	access := v.access(op, app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID])
	out.Tabs = fleetTabs(op.ID, access.View, seesFleetLive(op, access, live.sight, now), true, app.fleetReportTab(ctx, op, v, now), "srp")
	return out, true
}

// RenderFleetHidden answers for a fleet the session cannot open.
func (app *Application) RenderFleetHidden(w http.ResponseWriter, r *http.Request) {
	app.renderFleetHidden(w, r, app.fleetViewerFor(r.Context()))
}

// FleetsVisible says which of some fleets the session can see.
func (app *Application) FleetsVisible(ctx context.Context, ops []db.Op) map[int64]bool {
	v := app.fleetViewerFor(ctx)
	facts := app.fleetFactsFor(ctx, v, ops)
	out := make(map[int64]bool, len(ops))
	for _, op := range ops {
		f := facts[op.ID]
		f.ViaToken = false
		out[op.ID] = v.access(op, f).View
	}
	return out
}

// KillmailPayload is a stored killmail as ESI JSON. A pilot's own copy
// comes first. A killboard row whose detail has been pruned still
// answers from its summary, with no item list.
func (app *Application) KillmailPayload(ctx context.Context, killmailID int64) (string, string, bool) {
	if row, err := app.queries.GetKillmailDetail(ctx, killmailID); err == nil {
		return row.Payload, row.Hash, true
	}
	k, err := app.queries.GetKill(ctx, killmailID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("srp: killmail %d: %v", killmailID, err)
		}
		return "", "", false
	}
	if payload, err := app.queries.GetKillDetail(ctx, killmailID); err == nil {
		if raw, err := killDetailRaw(payload); err == nil {
			return string(raw), k.Hash, true
		}
	}
	raw, err := json.Marshal(esi.Killmail{
		KillmailID: k.KillmailID, KillmailTime: k.KillmailTime.UTC().Format(time.RFC3339), SolarSystemID: k.SystemID,
		Victim: esi.KillmailVictim{CharacterID: k.VictimCharacterID, CorporationID: k.VictimCorporationID, ShipTypeID: k.ShipTypeID},
	})
	if err != nil {
		return "", "", false
	}
	return string(raw), k.Hash, true
}
