package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// Fleet invites sent from EveSynapse (schema 177). A pilot signed up
// for a running fleet who is not in it yet can say they are ready for an
// invite. Those running the fleet invite one pilot, everyone ready or
// everyone coming; each invite waits in op_invites until the fleet lane
// sends it through ESI (POST /fleets/{id}/members/) as the character the
// fleet is read through, its boss. That character has to have granted
// EveSynapse fleet invites (the re-link-only fleet_invites module): it is
// asked for on the fleet's own page, never at sign-up. Each invite keeps
// how it went, so the panel shows who was sent one and who EVE refused.

const (
	fleetInvitesModule = "fleet_invites"
	fleetInviteScope   = "esi-fleets.write_fleet.v1"
	// fleetInvitesPerPass bounds one pass of the lane: ESI's fleet group
	// allows 1800 requests in 15 minutes, and a fleet invites at most
	// its coming list (300 sign-ups) a click.
	fleetInvitesPerPass = 100
)

// nudgeInvites wakes the fleet lane to send queued invites now. It never
// blocks: a wake already waiting covers this one.
func (app *Application) nudgeInvites() {
	select {
	case app.inviteNudge <- struct{}{}:
	default:
	}
}

// fleetInviteSender is the character a fleet's invites go out as: the
// one its fleet is read through. token is empty when it cannot send;
// why then says so, for a commander, or is empty while there is
// nothing to say yet (no read).
func (app *Application) fleetInviteSender(ctx context.Context, bossID int64) (token, name, why string) {
	if bossID == 0 {
		return "", "", ""
	}
	name = app.displayCharacter(ctx, bossID)
	ch, err := app.queries.GetCharacter(ctx, bossID)
	if err != nil || !app.characterMaySync(ctx, ch) {
		return "", name, name + " is not signed in to EveSynapse, so invites cannot go out from here."
	}
	if !scopeSet(ch.Scopes)[fleetInviteScope] {
		return "", name, name + " has not allowed EveSynapse to send fleet invites."
	}
	token, err = app.validAccessToken(ctx, ch)
	if err != nil {
		return "", name, name + "'s sign-in has stopped working. Sign in again to send invites."
	}
	return token, name, ""
}

// fleetInviteFailure words why EVE refused an invite for those running
// the fleet, and says whether trying again could help: a rate limit or
// a server error waits for the next pass.
func fleetInviteFailure(err error, sender string) (msg string, final bool) {
	var se *esi.StatusError
	if !errors.As(err, &se) {
		return "", false
	}
	switch {
	case se.Code == http.StatusForbidden:
		return sender + " cannot invite: sign in again to allow fleet invites, and stay the fleet's boss.", true
	case se.Code == http.StatusNotFound:
		return "EVE no longer finds the fleet.", true
	case se.Code == 420 || se.Code == http.StatusTooManyRequests || se.Code >= 500:
		return "", false
	}
	return "EVE refused the invite. The pilot may be in another fleet, or ask a fee for invites.", true
}

// sendFleetInvites sends the invites waiting for fleets in their live
// window, oldest first, a bounded number a pass. An invite whose fleet
// has not been read yet waits for the read.
func (app *Application) sendFleetInvites(ctx context.Context, now time.Time) {
	rows, err := app.queries.ListPendingFleetInvites(ctx, db.ListPendingFleetInvitesParams{Now: now, MaxRows: fleetInvitesPerPass})
	if err != nil {
		logging.Errorf("fleets: pending invites: %v", err)
		return
	}
	type sender struct{ token, name, why string }
	senders := map[int64]*sender{}
	mark := func(row db.ListPendingFleetInvitesRow, msg string) {
		if err := app.queries.MarkFleetInviteFailed(ctx, db.MarkFleetInviteFailedParams{At: sql.NullTime{Time: now, Valid: true}, Error: msg, OpID: row.OpID, CharacterID: row.CharacterID}); err != nil {
			logging.Errorf("fleets: invite of %d to %d: %v", row.CharacterID, row.OpID, err)
		}
	}
	for _, row := range rows {
		s := senders[row.OpID]
		if s == nil {
			s = &sender{}
			s.token, s.name, s.why = app.fleetInviteSender(ctx, row.CaptureBossID)
			senders[row.OpID] = s
		}
		switch {
		case row.FleetID == 0 || (s.token == "" && s.why == ""):
			continue
		case s.token == "":
			mark(row, s.why)
			continue
		}
		path := fmt.Sprintf("/fleets/%d/members/", row.FleetID)
		err := app.esi.PostAuthedNoContent(ctx, s.token, path, map[string]any{"character_id": row.CharacterID, "role": "squad_member"})
		if err == nil {
			if err := app.queries.MarkFleetInviteSent(ctx, db.MarkFleetInviteSentParams{At: sql.NullTime{Time: now, Valid: true}, OpID: row.OpID, CharacterID: row.CharacterID}); err != nil {
				logging.Errorf("fleets: invite of %d to %d: %v", row.CharacterID, row.OpID, err)
			}
			continue
		}
		if errors.Is(err, esi.ErrErrorLimit) {
			return
		}
		if msg, final := fleetInviteFailure(err, s.name); final {
			mark(row, msg)
		} else {
			logging.Warnf("fleets: invite of %d to fleet %d: %v", row.CharacterID, row.OpID, err)
		}
	}
}

// handleFleetInvite queues invites from the invite panel: one pilot, or
// everyone ready, or everyone coming who is not in the fleet. Only those
// running the fleet may, while it runs, once its boss allows invites.
func (app *Application) handleFleetInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, a, live, ok := app.liveFleetFromURL(w, r, false)
	if !ok {
		return
	}
	back := fleetCompositionPath(op.ID)
	defer func() { http.Redirect(w, r, back, http.StatusSeeOther) }()
	if !a.Manage || !fleetLiveOpen(op, time.Now()) {
		app.flash(ctx, "Only those running this fleet can invite, while it runs.")
		return
	}
	if token, _, why := app.fleetInviteSender(ctx, op.CaptureBossID); token == "" {
		if why == "" {
			why = "The fleet has not been read from EVE yet, so there is no fleet to invite to."
		}
		app.flash(ctx, why)
		return
	}
	view := &fleetLiveView{}
	app.fillFleetInvite(ctx, view, op, live.comp.Members, v)
	who := r.FormValue("who")
	var ids []int64
	for _, p := range view.InvitePilots {
		switch who {
		case "coming":
			ids = append(ids, p.ID)
		case "ready":
			if p.Ready {
				ids = append(ids, p.ID)
			}
		default:
			if strconv.FormatInt(p.ID, 10) == who {
				ids = append(ids, p.ID)
			}
		}
	}
	if len(ids) == 0 {
		app.flash(ctx, "Nobody to invite: the pilots may have joined already.")
		return
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if err := app.queries.QueueFleetInvite(ctx, db.QueueFleetInviteParams{OpID: op.ID, CharacterID: id, QueuedBy: v.UserID, QueuedAt: now}); err != nil {
			logging.Errorf("fleets: queue invite of %d to %d: %v", id, op.ID, err)
			app.flash(ctx, "The invites could not be queued. Try again in a moment.")
			return
		}
	}
	app.nudgeInvites()
	if len(ids) == 1 {
		app.flash(ctx, "Invite on its way. It shows below once EVE has it.")
	} else {
		app.flash(ctx, fmt.Sprintf("%d invites on their way. Each shows below once EVE has it.", len(ids)))
	}
}

// handleFleetReady records that a signed-up pilot is ready for an
// invite, or no longer is. It counts while the fleet runs.
func (app *Application) handleFleetReady(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil || r.ParseForm() != nil {
		app.renderFleetHidden(w, r, v)
		return
	}
	a := v.access(op, app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID])
	if !a.View {
		app.renderFleetHidden(w, r, v)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	allowed := false
	for _, c := range a.Signers {
		allowed = allowed || c.CharacterID == characterID
	}
	defer func() { http.Redirect(w, r, fmt.Sprintf("%s?as=%d", opURL(op.ID), characterID), http.StatusSeeOther) }()
	if !allowed || !fleetLiveOpen(op, time.Now()) {
		app.flash(ctx, "You can say you are ready only while the fleet runs, for a character signed up to it.")
		return
	}
	ready := sql.NullTime{Time: time.Now().UTC(), Valid: r.FormValue("ready") == "1"}
	n, err := app.queries.SetOpSignupReady(ctx, db.SetOpSignupReadyParams{ReadyAt: ready, OpID: op.ID, CharacterID: characterID})
	switch {
	case err != nil:
		logging.Errorf("fleets: ready of %d on %d: %v", characterID, op.ID, err)
		app.flash(ctx, "That could not be saved. Try again in a moment.")
	case n == 0:
		app.flash(ctx, "Sign up first, then say you are ready.")
	}
}
