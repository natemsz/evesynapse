package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// The one read of a character's fleet, shared by the live fleet capture
// (ops_attendance.go) and the map's "my fleet" layer (map_mine.go).
//
// ESI caches a character's fleet for 60 seconds and a fleet's members
// and wings for 5, and shows members only to the fleet's boss.
// ---------------------------------------------------------------------------

// fleetLaneTick is how often running fleets are read. A boss handoff
// cannot show sooner than ESI's 60 second cache on the fleet lookup.
const fleetLaneTick = time.Minute

type fleetReadState int

const (
	fleetReadNone   fleetReadState = iota // not in a fleet
	fleetReadMember                       // in a fleet, and its members cannot be read
	fleetReadBoss                         // the fleet's boss: Members is filled
)

// fleetRead is what ESI says about the fleet one character is in.
type fleetRead struct {
	State   fleetReadState
	FleetID int64
	BossID  int64
	Members []esi.FleetMember
}

// readCharacterFleet asks ESI for the character's fleet and, when the
// character is its boss, the members. Not being in a fleet, or losing
// the boss seat between the two calls, is an answer and not an error.
// An error with a FleetID set means the members could not be read.
func (app *Application) readCharacterFleet(ctx context.Context, token string, characterID int64) (fleetRead, error) {
	var fleet esi.CharacterFleet
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/characters/%d/fleet/", characterID), &fleet); err != nil {
		if code, ok := esi.StatusCode(err); ok && code == http.StatusNotFound {
			return fleetRead{State: fleetReadNone}, nil
		}
		return fleetRead{}, err
	}
	read := fleetRead{State: fleetReadMember, FleetID: fleet.FleetID, BossID: fleet.FleetBossID}
	if fleet.FleetBossID != characterID {
		return read, nil
	}
	var members []esi.FleetMember
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/fleets/%d/members/", fleet.FleetID), &members); err != nil {
		if code, ok := esi.StatusCode(err); ok && (code == http.StatusNotFound || code == http.StatusForbidden) {
			return read, nil
		}
		return read, err
	}
	read.State, read.Members = fleetReadBoss, members
	return read, nil
}

// runFleetLane reads running fleets on a goroutine of its own, so a
// long character pass never delays a live fleet. A tick with no fleet
// running costs one indexed query and no ESI call.
func (app *Application) runFleetLane(ctx context.Context) {
	tick := time.NewTicker(fleetLaneTick)
	defer tick.Stop()
	for {
		if !app.esi.HardLimitActive(time.Now()) && !app.esi.ErrorBudgetLow() {
			runGuarded("fleet lane", func() { app.captureOpAttendance(ctx, time.Now()) })
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
