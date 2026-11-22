package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Verified pilots: people who prove a character through EVE sign-in so
// they can join a community or fleet, with no account here. The sign-in
// asks for no permissions and keeps no token. A verified pilot is never
// a row in characters and has no standing, so it meets no rule and can
// manage nothing; the session only says which character it is.
// ---------------------------------------------------------------------------

const (
	// sessionPilotEntry marks a sign-in that started at a Fleets entry
	// point, so its end never falls through to the full link.
	sessionPilotEntry = "pilot_entry"
	sessionPilotID    = "verified_pilot_id"
	sessionPilotOwner = "verified_pilot_owner"

	pilotHomePath        = "/fleets/communities/"
	pilotAffiliationWait = 5 * time.Second
)

// handlePilotSignIn starts a sign-in with no permissions for someone
// on a Fleets page (GET /auth/pilot?next=).
func (app *Application) handlePilotSignIn(w http.ResponseWriter, r *http.Request) {
	if !moduleCompiledIn("communities") {
		http.NotFound(w, r)
		return
	}
	if !app.cfg.SSOConfigured() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "EVE SSO is not configured yet: set EVE_CLIENT_ID / EVE_CLIENT_SECRET (see .env.example).")
		return
	}
	ctx := r.Context()
	target := pilotHomePath
	if next := r.URL.Query().Get("next"); safeReturnPath(next) && strings.HasPrefix(next, "/fleets/") && !strings.Contains(next, "..") {
		target = next
	}
	if app.userID(ctx) != 0 {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	app.sessions.Put(ctx, sessionReturnTo, target)
	app.sessions.Put(ctx, sessionIdentityLogin, "1")
	app.sessions.Put(ctx, sessionPilotEntry, "1")
	cfg := eveOAuthConfig(app.cfg)
	cfg.Scopes = nil
	app.authorizeRedirect(w, r, cfg)
}

// finishPilotSignIn ends a sign-in from a Fleets entry point. A
// character on a working account signs in to that account. Any other
// becomes a verified pilot, including one whose account needs a new
// link: a Fleets entry point never sends anyone to the full link.
func (app *Application) finishPilotSignIn(w http.ResponseWriter, r *http.Request, characterID int64, name, ownerHash string) {
	ctx := r.Context()
	_, target := app.identityLogin(ctx, characterID, ownerHash)
	switch {
	case target == "":
		app.finishIdentityLogin(w, r, characterID, name, ownerHash)
		return
	case strings.HasPrefix(target, "/?error="):
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	case ownerHash == "":
		// Without the owner hash a sold character could not be told
		// from its previous owner.
		logging.Warnf("sso pilot sign-in: character %d has no owner hash", characterID)
		http.Redirect(w, r, "/?error=verify", http.StatusSeeOther)
		return
	}
	if err := app.recordVerifiedPilot(ctx, characterID, name, ownerHash); err != nil {
		logging.Errorf("sso pilot sign-in: store character %d: %v", characterID, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	if err := app.sessions.RenewToken(ctx); err != nil {
		logging.Errorf("sso pilot sign-in: renew session token: %v", err)
	}
	app.sessions.Put(ctx, sessionPilotID, characterID)
	app.sessions.Put(ctx, sessionPilotOwner, ownerHash)
	app.flash(ctx, "Signed in as "+name+". You can join communities without an account.")
	next := app.takeReturnTo(ctx)
	if next == "/" {
		next = pilotHomePath
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// recordVerifiedPilot stores who signed in, after ending any community
// identity the character held under a previous owner.
func (app *Application) recordVerifiedPilot(ctx context.Context, characterID int64, name, ownerHash string) error {
	if err := app.releaseSoldCharacter(ctx, characterID, ownerHash); err != nil {
		return err
	}
	corp, alliance, at := app.pilotAffiliation5s(ctx, characterID)
	return app.queries.UpsertVerifiedPilot(ctx, db.UpsertVerifiedPilotParams{
		CharacterID: characterID, Name: name, OwnerHash: ownerHash,
		CorporationID: corp, AllianceID: alliance, AffiliationAt: at, SeenAt: time.Now().UTC(),
	})
}

// pilotAffiliation5s reads a character's public corporation and
// alliance at sign-in. A failure leaves the stored values alone.
func (app *Application) pilotAffiliation5s(ctx context.Context, characterID int64) (corp, alliance int64, at sql.NullTime) {
	ctx, cancel := context.WithTimeout(ctx, pilotAffiliationWait)
	defer cancel()
	var rows []struct {
		CharacterID   int64 `json:"character_id"`
		CorporationID int64 `json:"corporation_id"`
		AllianceID    int64 `json:"alliance_id"`
	}
	if err := app.esi.PostJSON(ctx, "/characters/affiliation/", []int64{characterID}, &rows); err != nil {
		logging.Warnf("sso pilot sign-in: affiliation of %d: %v", characterID, err)
		return 0, 0, sql.NullTime{}
	}
	for _, row := range rows {
		if row.CharacterID == characterID {
			return row.CorporationID, row.AllianceID, sql.NullTime{Time: time.Now().UTC(), Valid: true}
		}
	}
	return 0, 0, sql.NullTime{}
}

// releaseSoldCharacter ends a character's community identity when a
// verified sign-in proves it has changed EVE accounts.
func (app *Application) releaseSoldCharacter(ctx context.Context, characterID int64, ownerHash string) error {
	row, err := app.queries.GetVerifiedPilot(ctx, characterID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownerHashChanged(row.OwnerHash, ownerHash) {
		return nil
	}
	logging.Infof("communities: character %d changed EVE accounts; its community places end", characterID)
	return app.queries.ReleaseCommunityCharacter(ctx, characterID)
}

// VerifiedPilot is the session's verified pilot. An account's session
// never is one, and a session whose character has since been sold or
// removed stops being one.
func (app *Application) VerifiedPilot(ctx context.Context) (module.VerifiedPilot, bool) {
	if app.sessions.GetBool(ctx, sessionAuthenticated) {
		return module.VerifiedPilot{}, false
	}
	id, _ := app.sessions.Get(ctx, sessionPilotID).(int64)
	if id == 0 {
		return module.VerifiedPilot{}, false
	}
	row, err := app.queries.GetVerifiedPilot(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("verified pilot %d: %v", id, err)
		return module.VerifiedPilot{}, false
	}
	if err != nil || row.OwnerHash != app.sessions.GetString(ctx, sessionPilotOwner) {
		app.sessions.Remove(ctx, sessionPilotID)
		app.sessions.Remove(ctx, sessionPilotOwner)
		return module.VerifiedPilot{}, false
	}
	return module.VerifiedPilot{CharacterID: id, Name: row.Name}, true
}

// handlePilotForget removes a verified pilot's record and community
// places at their request (POST /auth/pilot/forget). A ban a community
// placed on the character stays with that community.
func (app *Application) handlePilotForget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pilot, ok := app.VerifiedPilot(ctx)
	if !ok {
		http.Redirect(w, r, pilotHomePath, http.StatusSeeOther)
		return
	}
	if err := app.queries.ReleaseCommunityCharacter(ctx, pilot.CharacterID); err != nil {
		logging.Errorf("verified pilot %d: remove record: %v", pilot.CharacterID, err)
		app.flash(ctx, "Your pilot record could not be removed. Try again in a moment.")
		http.Redirect(w, r, pilotHomePath, http.StatusSeeOther)
		return
	}
	app.sessions.Remove(ctx, sessionPilotID)
	app.sessions.Remove(ctx, sessionPilotOwner)
	if err := app.sessions.RenewToken(ctx); err != nil {
		logging.Errorf("verified pilot: renew session token: %v", err)
	}
	app.flash(ctx, "Your pilot record and community memberships are removed.")
	http.Redirect(w, r, pilotHomePath, http.StatusSeeOther)
}

// GroupOwners lists the owners whose groups an account may keep: the
// corporations and alliances it directs, then the communities it leads
// or is an admin of. Discord and intel keep using ManageableOwners, so
// communities stay out of those surfaces.
func (app *Application) GroupOwners(ctx context.Context, userID int64) []standing.Owner {
	owners := app.manageableOwners(ctx, userID)
	st, err := app.standingFor(ctx, userID)
	if err != nil {
		logging.Errorf("groups: standing of user %d: %v", userID, err)
		return owners
	}
	for _, id := range slices.Sorted(maps.Keys(st.Communities)) {
		if st.ManagesCommunity(id) {
			owners = append(owners, standing.Owner{Kind: ownerCommunity, ID: id})
		}
	}
	return owners
}
