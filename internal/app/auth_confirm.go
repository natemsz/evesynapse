package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Which account a sign-in lands on is decided here, and the two
// cases a player could get wrong without noticing are asked about
// instead of decided: a character that is already on a different
// account of the same player (moving it would split their pilots
// across accounts), and a character that is on no account while
// nobody is signed in (a fresh account is only right when the player
// has none yet). The verified sign-in waits in the session, tokens
// sealed, until the player answers on /auth/confirm.

const (
	// sessionPendingLink holds the verified sign-in waiting for
	// the player's answer (a pendingLink as JSON).
	sessionPendingLink = "pending_link"
	// sessionLinkIntent marks a link started by a signed-in
	// player to add a character (not a re-link of one).
	sessionLinkIntent = "link_intent"

	// pendingLinkLifetime is how long a waiting sign-in stays
	// usable. EVE access tokens last 20 minutes; the refresh
	// token outlives this, so a link made late still syncs.
	pendingLinkLifetime = 15 * time.Minute
)

// signInPlan is what a verified sign-in does next.
type signInPlan struct {
	// Step is "link" (link to UserID now), "move" (the character
	// is on OnAccount, another account of the same player, while
	// UserID is signed in) or "new" (nobody is signed in and the
	// character is on no account).
	Step string
	// UserID is the account the sign-in links to ("link"), or the
	// signed-in account ("move"); 0 for "new".
	UserID int64
	// OnAccount is the account the character is on now, 0 when
	// none.
	OnAccount int64
	// Outcome says what a "link" does: "added" (a character new to
	// the signed-in account), "renewed" (already on it) or "signin"
	// (nobody was signed in; the character's account is entered).
	Outcome string
}

// planSignIn decides where a verified sign-in goes. A character
// whose owner hash changed belongs to a new player: it never counts
// as "already on an account" for that player, so it joins the
// signed-in account as before, or asks the signed-out player.
func (app *Application) planSignIn(ctx context.Context, sessionUserID, characterID int64, ownerHash string) (signInPlan, error) {
	if sessionUserID != 0 {
		switch _, err := app.queries.GetUser(ctx, sessionUserID); {
		case errors.Is(err, pgx.ErrNoRows):
			sessionUserID = 0 // the session's account was removed
		case err != nil:
			return signInPlan{}, err
		}
	}
	existing, err := app.queries.GetCharacter(ctx, characterID)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return signInPlan{}, err
	}
	onAccount := int64(0)
	if found {
		onAccount = existing.UserID
	}
	sameOwner := found && !ownerHashChanged(existing.OwnerHash, ownerHash)
	switch {
	case sessionUserID != 0 && found && existing.UserID == sessionUserID:
		return signInPlan{Step: "link", UserID: sessionUserID, OnAccount: onAccount, Outcome: "renewed"}, nil
	case sessionUserID != 0 && sameOwner:
		return signInPlan{Step: "move", UserID: sessionUserID, OnAccount: onAccount}, nil
	case sessionUserID != 0:
		return signInPlan{Step: "link", UserID: sessionUserID, OnAccount: onAccount, Outcome: "added"}, nil
	case sameOwner:
		return signInPlan{Step: "link", UserID: existing.UserID, OnAccount: onAccount, Outcome: "signin"}, nil
	}
	return signInPlan{Step: "new", OnAccount: onAccount}, nil
}

// pendingLink is a verified sign-in waiting in the session. The
// tokens are sealed the way the characters table stores them.
type pendingLink struct {
	CharacterID int64     `json:"c"`
	Name        string    `json:"n"`
	Scopes      string    `json:"s"`
	OwnerHash   string    `json:"o"`
	Access      string    `json:"a"`
	Refresh     string    `json:"r"`
	Expiry      time.Time `json:"e"`
	At          time.Time `json:"t"`
}

func (app *Application) stashPendingLink(ctx context.Context, in linkCharacterInput) error {
	access, refresh, err := app.tokens.sealTokens(in.CharacterID, in.AccessToken, in.RefreshToken)
	if err != nil {
		return err
	}
	p := pendingLink{
		CharacterID: in.CharacterID, Name: in.Name, Scopes: in.Scopes, OwnerHash: in.OwnerHash,
		Access: access, Refresh: refresh, At: time.Now().UTC(),
	}
	if in.TokenExpiry.Valid {
		p.Expiry = in.TokenExpiry.Time
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return err
	}
	app.sessions.Put(ctx, sessionPendingLink, string(blob))
	return nil
}

// loadPendingLink returns the waiting sign-in, opened back into a
// link input. A stale or unreadable one is dropped.
func (app *Application) loadPendingLink(ctx context.Context) (linkCharacterInput, bool) {
	raw := app.sessions.GetString(ctx, sessionPendingLink)
	if raw == "" {
		return linkCharacterInput{}, false
	}
	var p pendingLink
	if json.Unmarshal([]byte(raw), &p) != nil || p.CharacterID == 0 || time.Since(p.At) > pendingLinkLifetime {
		app.sessions.Remove(ctx, sessionPendingLink)
		return linkCharacterInput{}, false
	}
	access, refresh, err := app.tokens.openTokens(db.Character{CharacterID: p.CharacterID, AccessToken: p.Access, RefreshToken: p.Refresh})
	if err != nil {
		logging.Warnf("confirm: open waiting sign-in for character %d: %v", p.CharacterID, err)
		app.sessions.Remove(ctx, sessionPendingLink)
		return linkCharacterInput{}, false
	}
	return linkCharacterInput{
		CharacterID: p.CharacterID, Name: p.Name, Scopes: p.Scopes, OwnerHash: p.OwnerHash,
		AccessToken: access, RefreshToken: refresh,
		TokenExpiry: sql.NullTime{Time: p.Expiry, Valid: !p.Expiry.IsZero()},
	}, true
}

// pendingLinkWaiting reports whether a sign-in other than
// characterID waits for an answer.
func (app *Application) pendingLinkWaiting(ctx context.Context, characterID int64) bool {
	in, ok := app.loadPendingLink(ctx)
	return ok && in.CharacterID != characterID
}

// finishLink stores a verified sign-in on userID, signs the session
// in to that account, and sends the player on with notice shown
// (none when empty). returnTo, when set, is where the player lands
// once any second factor is through.
func (app *Application) finishLink(w http.ResponseWriter, r *http.Request, userID int64, in linkCharacterInput, notice, returnTo string) {
	ctx := r.Context()

	// What the player chose at sign-up is remembered for the
	// account's next character link (the choice waited in the
	// session until there was an account).
	if blob := app.sessions.GetString(ctx, sessionLinkChoice); blob != "" {
		app.sessions.Remove(ctx, sessionLinkChoice)
		var c linkChoice
		if json.Unmarshal([]byte(blob), &c) == nil {
			app.rememberLink(ctx, userID, c)
		}
	}
	// The theme the player picked at sign-up, saved to the account
	// it just created (or returned to), so the first page inside
	// already wears it.
	if blob := app.sessions.GetString(ctx, sessionThemeChoice); blob != "" {
		app.sessions.Remove(ctx, sessionThemeChoice)
		var prefs themePrefs
		if json.Unmarshal([]byte(blob), &prefs) == nil {
			if err := app.storeThemePrefs(ctx, userID, prefs); err != nil {
				logging.Errorf("sso: store sign-up theme for user %d: %v", userID, err)
			}
		}
	}

	in.UserID = userID
	result, err := app.linkVerifiedCharacter(ctx, in)
	if err != nil {
		logging.Errorf("sso: store character %d: %v", in.CharacterID, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	switch {
	case result.OwnerChanged && result.Moved:
		logging.Warnf("sso: character %d changed EVE accounts since it was linked; moved off its previous account to user %d", in.CharacterID, userID)
	case result.OwnerChanged:
		logging.Warnf("sso: character %d owner hash changed since the link was verified; flagged for re-verification", in.CharacterID)
	case result.Moved:
		logging.Infof("sso: character %d moved to user %d at the player's request", in.CharacterID, userID)
	}
	logging.Infof("sso: signed in character %d (%s) on user %d", in.CharacterID, in.Name, userID)

	// Warm this character first on the next worker cycle so its
	// pages are ready moments after login, not minutes later.
	app.markCharacterPriority(in.CharacterID)

	// Rotate the session token on privilege change, then sign in.
	if err := app.sessions.RenewToken(ctx); err != nil {
		logging.Errorf("sso: renew session token: %v", err)
	}
	app.sessions.Remove(ctx, sessionLinkIntent)
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionUserID, int(userID))
	putSessionCharID(app.sessions, ctx, in.CharacterID)
	app.sessions.Put(ctx, sessionCharacterName, in.Name)
	app.recorder().tick(statsEventSignIn)
	app.armSecondFactor(ctx, userID)
	if notice != "" {
		app.flash(ctx, notice)
	}
	if returnTo != "" {
		app.sessions.Put(ctx, sessionReturnTo, returnTo)
	}
	http.Redirect(w, r, app.signInTarget(ctx), http.StatusSeeOther)
}

// linkNotice is the message a "link" plan leaves, and where the
// player lands after it. A sign-in into an existing account says
// nothing and keeps the page the player came from.
func (app *Application) linkNotice(ctx context.Context, plan signInPlan, in linkCharacterInput) (notice, returnTo string) {
	switch plan.Outcome {
	case "added":
		return in.Name + " was added to your account.", "/characters/"
	case "renewed":
		if app.sessions.GetString(ctx, sessionLinkIntent) == "add" {
			return in.Name + " is already on your account, so its EVE sign-in was renewed. To add a different character, pick it on EVE's login page; for a character on another EVE account, switch EVE accounts there first.", "/characters/"
		}
		return in.Name + "'s EVE sign-in was renewed.", ""
	}
	if app.pendingLinkWaiting(ctx, in.CharacterID) {
		return "", "/auth/confirm"
	}
	return "", ""
}

// confirmView is the /auth/confirm page.
type confirmView struct {
	// Kind is "move", "add" (signed in, the character is on no
	// other account of this player) or "new" (signed out).
	Kind        string
	Name        string
	CharacterID int64
	PortraitURL string
	OtherCount  int              // characters on the other account ("move")
	Account     *linkAccountView // the signed-in account ("move", "add")
	CanCreate   bool             // "new": this server would create an account for it
	Error       string
}

// linkAccountView is the signed-in account, shown by its
// characters, so a player can tell which account they are on.
type linkAccountView struct {
	Chars []switcherEntry
	More  int
	Total int
}

const linkAccountShown = 6

func (app *Application) linkAccount(ctx context.Context) *linkAccountView {
	chars := app.sessionCharacters(ctx)
	if len(chars) == 0 {
		return nil
	}
	entries := app.switcherEntriesFor(ctx, chars)
	v := &linkAccountView{Total: len(entries)}
	if len(entries) > linkAccountShown {
		v.More = len(entries) - linkAccountShown
		entries = entries[:linkAccountShown]
	}
	v.Chars = entries
	return v
}

// confirmKind maps a plan to the question the page asks, "" when the
// sign-in needs no question.
func confirmKind(plan signInPlan) string {
	switch plan.Step {
	case "move":
		return "move"
	case "new":
		return "new"
	}
	if plan.Outcome == "added" || plan.Outcome == "renewed" {
		return "add"
	}
	return ""
}

// handleLinkConfirm asks the player where a waiting sign-in goes.
func (app *Application) handleLinkConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, ok := app.loadPendingLink(ctx)
	if !ok {
		http.Redirect(w, r, app.confirmExit(ctx), http.StatusSeeOther)
		return
	}
	plan, err := app.planSignIn(ctx, app.userID(ctx), in.CharacterID, in.OwnerHash)
	if err != nil {
		logging.Errorf("confirm: plan for character %d: %v", in.CharacterID, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	kind := confirmKind(plan)
	if kind == "" {
		// Signed out and the character now has an account of its
		// own player: nothing to ask, enter it.
		app.sessions.Remove(ctx, sessionPendingLink)
		app.finishLink(w, r, plan.UserID, in, "", "")
		return
	}
	data := app.publicPage(ctx)
	view := &confirmView{Kind: kind, Name: in.Name, CharacterID: in.CharacterID, PortraitURL: portraitURL(in.CharacterID, 64)}
	switch kind {
	case "move":
		if chars, err := app.queries.ListCharactersByUser(ctx, plan.OnAccount); err == nil {
			view.OtherCount = len(chars)
		}
		view.Account = app.linkAccount(ctx)
	case "add":
		view.Account = app.linkAccount(ctx)
	case "new":
		err := app.mayCreateAccount(ctx, in.CharacterID, in.OwnerHash)
		view.CanCreate = !errors.Is(err, errSignUpNotAllowed)
	}
	data.Confirm = view
	app.render(ctx, w, http.StatusOK, "link_confirm.html", data)
}

// handleLinkConfirmPost carries out the player's answer. The answer
// must fit the question the page asks now: a session that changed
// account in between is asked again.
func (app *Application) handleLinkConfirmPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	action := r.PostFormValue("action")
	if action == "cancel" {
		app.sessions.Remove(ctx, sessionPendingLink)
		http.Redirect(w, r, app.confirmExit(ctx), http.StatusSeeOther)
		return
	}
	in, ok := app.loadPendingLink(ctx)
	if !ok {
		app.flash(ctx, "That sign-in expired before it was used. Link the character again.")
		http.Redirect(w, r, app.confirmExit(ctx), http.StatusSeeOther)
		return
	}
	plan, err := app.planSignIn(ctx, app.userID(ctx), in.CharacterID, in.OwnerHash)
	if err != nil {
		logging.Errorf("confirm: plan for character %d: %v", in.CharacterID, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	kind := confirmKind(plan)
	again := func() { http.Redirect(w, r, "/auth/confirm", http.StatusSeeOther) }
	switch {
	case kind == "add" && action == "add":
		app.sessions.Remove(ctx, sessionPendingLink)
		notice, _ := app.linkNotice(ctx, plan, in)
		app.finishLink(w, r, plan.UserID, in, notice, "/characters/")
	case kind == "move" && action == "move":
		app.sessions.Remove(ctx, sessionPendingLink)
		app.finishLink(w, r, plan.UserID, in, in.Name+" was moved to this account.", "/characters/")
	case kind == "move" && action == "switch":
		app.sessions.Remove(ctx, sessionPendingLink)
		app.finishLink(w, r, plan.OnAccount, in, "You are now signed in to the account "+in.Name+" is on.", "/characters/")
	case kind == "new" && action == "create":
		userID, err := app.resolveSignInUser(ctx, 0, in.CharacterID, in.OwnerHash)
		if err != nil {
			switch {
			case errors.Is(err, errSignUpNotAllowed):
				logging.Infof("confirm: character %d (%s) is not on this instance's sign-up lists; no account created", in.CharacterID, in.Name)
				http.Redirect(w, r, "/?error=notallowed", http.StatusSeeOther)
			case errors.Is(err, errSignUpUnverified):
				logging.Warnf("confirm: sign-up check for character %d: %v", in.CharacterID, err)
				http.Redirect(w, r, "/?error=allowcheck", http.StatusSeeOther)
			default:
				logging.Errorf("confirm: create account for character %d: %v", in.CharacterID, err)
				http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
			}
			return
		}
		app.sessions.Remove(ctx, sessionPendingLink)
		app.finishLink(w, r, userID, in, "Welcome to EveSynapse. "+in.Name+" is linked. Link your other characters now so they all sit on this one account.", "/characters/")
	default:
		again()
	}
}

// confirmExit is where leaving the confirm page goes.
func (app *Application) confirmExit(ctx context.Context) string {
	if app.userID(ctx) != 0 {
		return "/characters/"
	}
	return "/"
}

// routeVerifiedSignIn sends a verified (non-identity) sign-in where
// it belongs: the signed-in account for a character new to it or
// already on it, the character's own account for a signed-out
// sign-in. A character on another account of the same player, and a
// signed-out character on no account, wait on /auth/confirm for the
// player to say where they go.
func (app *Application) routeVerifiedSignIn(w http.ResponseWriter, r *http.Request, in linkCharacterInput) {
	ctx := r.Context()
	plan, err := app.planSignIn(ctx, app.signInSessionUser(ctx), in.CharacterID, in.OwnerHash)
	if err != nil {
		logging.Errorf("sso callback: plan account for character %d: %v", in.CharacterID, err)
		http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
		return
	}
	if plan.Step != "link" {
		if err := app.stashPendingLink(ctx, in); err != nil {
			logging.Errorf("sso callback: hold sign-in for character %d: %v", in.CharacterID, err)
			http.Redirect(w, r, "/?error=save", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/auth/confirm", http.StatusSeeOther)
		return
	}
	notice, returnTo := app.linkNotice(ctx, plan, in)
	app.finishLink(w, r, plan.UserID, in, notice, returnTo)
}

// signInSessionUser is the account a new sign-in may add to: the
// session's, unless the session still owes its second factor. A
// sign-in that has not passed the factor holds no account, so it is
// dropped and the new sign-in starts from signed out (and is asked
// for the factor of whatever account it lands on).
func (app *Application) signInSessionUser(ctx context.Context) int64 {
	if !app.sessions.GetBool(ctx, session2FAPending) {
		return app.userID(ctx)
	}
	app.sessions.Remove(ctx, sessionUserID)
	app.sessions.Remove(ctx, sessionAuthenticated)
	app.sessions.Remove(ctx, sessionLinkIntent)
	app.sessions.Put(ctx, session2FAPending, false)
	app.sessions.Put(ctx, session2FAMustEnroll, false)
	return 0
}
