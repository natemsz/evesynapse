package app

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Customize hint: a small card that drops down from the
// Customize button the first time a signed-in account views its
// home, telling it the page can be arranged and that new modules
// have joined the list. It follows the notification prompt's
// shape (notify_prompt.go): it shows itself once per session for
// an account that has not answered, the answer is remembered on
// the account (users.customize_prompt_seen_at, schema 185) so it
// follows the person and not the browser, and opening Customize
// counts as answering, since the reader has found the thing the
// card points at. It queues behind the notification prompt, one
// by one: it waits while that card is about to show on this view,
// and drops once the notification is done (answered, or already
// shown this session). If the notification was already answered
// the hint does not wait for one that will never come, it drops
// right away. The demo account reads the same shape without the
// remembered answer: it drops once per login (the session mark),
// and nothing is saved for it, so every visitor sees it once.
// ---------------------------------------------------------------------------

// sessionCustomizePromptShown marks a session that has already
// been shown the hint unasked, so it drops once per visit and
// not on every home view of it.
const sessionCustomizePromptShown = "customize_prompt_shown"

// customizePromptView is the card, as home.html shows it under
// the Customize button.
type customizePromptView struct {
	// AutoOpen: the card renders open. The card only renders at
	// all for an account that has not answered in a session that
	// has not been shown it, so this is always true in practice;
	// it keeps the card's contract the notification prompt's.
	AutoOpen bool
}

// customizePromptFor builds the card for an account, or nil when
// it must not drop: already answered, already shown this session,
// or the notification prompt is about to show on this view. The
// queue is one by one: the notification goes first, and this card
// waits until it is done. This runs before the shell builds the
// notification prompt (buildHome runs before fillShell), so the
// session flag here still means "shown on an earlier view", and
// the check reads whether the notification will show on this one.
// A notification that was already answered never holds this one
// back: the hint drops right away instead of waiting for a card
// that will never come.
func (app *Application) customizePromptFor(ctx context.Context, userID int64) *customizePromptView {
	seen, err := app.queries.GetUserCustomizePromptSeen(ctx, userID)
	if err != nil {
		logging.Errorf("home: customize hint stamp for user %d: %v", userID, err)
		return nil
	}
	if seen.Valid {
		return nil
	}
	if app.sessions.GetBool(ctx, sessionCustomizePromptShown) {
		return nil
	}
	if !app.notifyPrefsFor(ctx, userID).PromptAnswered && !app.sessions.GetBool(ctx, sessionNotifyPromptShown) {
		return nil
	}
	app.sessions.Put(ctx, sessionCustomizePromptShown, true)
	return &customizePromptView{AutoOpen: true}
}

// markCustomizePromptSeen records the account's answer. The
// write only lands on the first answer, so calling it again
// (every Customize view, every hint post) costs one guarded
// update and changes nothing.
func (app *Application) markCustomizePromptSeen(ctx context.Context, userID int64) {
	if userID == 0 || app.isDemoUser(ctx, userID) {
		return
	}
	if _, err := app.queries.SetUserCustomizePromptSeen(ctx, db.SetUserCustomizePromptSeenParams{
		CustomizePromptSeenAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, ID: userID,
	}); err != nil {
		logging.Errorf("home: answer customize hint for user %d: %v", userID, err)
	}
}

// handleCustomizePrompt stores the card's answer (POST
// /home/customize-prompt): following it in lands on Customize
// with the Add a module list open, "Not now" lands back on the
// home. Either way the hint is answered and never drops unasked
// again.
func (app *Application) handleCustomizePrompt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 || r.ParseForm() != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	app.markCustomizePromptSeen(ctx, userID)
	if r.FormValue("action") == "try" {
		http.Redirect(w, r, "/?customize=1&add=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
