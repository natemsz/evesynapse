package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The first-run prompt: a small card that drops down from the bell
// the first time a signed-in account views a page, asking whether
// it wants notifications and where they should reach it.
//
// It is a guest, not a surface of its own: it shows itself once (per
// account, remembered in the account's settings, so the answer
// follows the person and not the browser), and afterwards it waits
// behind the bell's "Notification choices" entry. The choices it
// offers are the same ones the settings page offers — the in-app
// and browser channels of notifyPrefs, and the Discord switch on
// the account's Discord link — never a store of their own.
// ---------------------------------------------------------------------------

// sessionNotifyPromptShown marks a session that has already been
// shown the prompt unasked, so it drops once per visit and not on
// every page of it. The remembered answer itself is in the
// account's settings (notifyPrefs.PromptAnswered).
const sessionNotifyPromptShown = "notify_prompt_shown"

// notifyPromptView is the card, as base.html shows it beside the
// bell. A row is offered only when this install can honor it: a
// channel nobody here can deliver is not a choice to offer.
type notifyPromptView struct {
	// AutoOpen: the card renders open — the account has not answered
	// and this session has not been shown it yet.
	AutoOpen bool
	Answered bool
	// InApp is the top-bar bell and the notifications page.
	InApp bool
	// Desktop is browser push (notify_push.go): offered only when
	// this server has a VAPID key pair. Subscribing a browser still
	// happens on the settings page, one browser at a time; this
	// switch says whether subscribed browsers are sent to at all.
	DesktopOffered bool
	Desktop        bool
	// Discord is direct messages: offered when the module is built
	// in and the account has linked Discord, or this install offers
	// the linking. The switch exists only with a link; without one
	// the row points at the settings page instead.
	DiscordOffered bool
	DiscordLinked  bool
	Discord        bool
	DiscordName    string
}

// notifyPromptFor builds the card for an account.
func (app *Application) notifyPromptFor(ctx context.Context, userID int64) *notifyPromptView {
	prefs := app.notifyPrefsFor(ctx, userID)
	view := &notifyPromptView{
		Answered:       prefs.PromptAnswered,
		InApp:          !prefs.channelOff(notifyChannelInApp),
		DesktopOffered: app.push != nil,
		Desktop:        !prefs.channelOff(notifyChannelDesktop),
	}
	if app.ModuleEnabled("discord") {
		if link, err := app.queries.GetDiscordLink(ctx, userID); err == nil {
			view.DiscordOffered, view.DiscordLinked = true, true
			view.Discord, view.DiscordName = link.DmNotifications, link.Username
		} else {
			if !errors.Is(err, sql.ErrNoRows) {
				logging.Errorf("notify: prompt discord link of user %d: %v", userID, err)
			}
			view.DiscordOffered = app.cfg.discordClientID != "" && app.cfg.discordClientSecret != ""
		}
	}
	if !view.Answered && !app.sessions.GetBool(ctx, sessionNotifyPromptShown) {
		app.sessions.Put(ctx, sessionNotifyPromptShown, true)
		view.AutoOpen = true
	}
	return view
}

// handleNotifyPrompt stores the card's answer: the channels the
// account picked (or, for "Not now", nothing but the answer
// itself). Either way the prompt is answered, and it is not shown
// unasked again.
func (app *Application) handleNotifyPrompt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := "/"
	if ref := r.Referer(); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host == r.Host {
			back = notifyTarget(u.EscapedPath())
		}
	}
	if userID == 0 || r.ParseForm() != nil {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	prefs := app.notifyPrefsFor(ctx, userID)
	prefs.PromptAnswered = true
	if r.FormValue("action") != "dismiss" {
		desktopOn := !prefs.channelOff(notifyChannelDesktop)
		if r.FormValue("desktop_shown") == "1" {
			desktopOn = r.FormValue("desktop") == "1"
		}
		prefs.OffChannels = offChannels(r.FormValue("inapp") == "1", desktopOn)
		// The Discord switch is the one on the account's link — the
		// same value the settings page's Discord box writes. Without
		// a link there is nothing to write.
		if r.FormValue("discord_shown") == "1" && app.ModuleEnabled("discord") {
			if _, err := app.queries.GetDiscordLink(ctx, userID); err == nil {
				if err := app.queries.SetDiscordDMNotifications(ctx, db.SetDiscordDMNotificationsParams{
					UserID: userID, DmNotifications: r.FormValue("discord") == "1",
				}); err != nil {
					logging.Errorf("notify: prompt discord switch for user %d: %v", userID, err)
					app.flash(ctx, "The Discord choice could not be saved; check the server log.")
					http.Redirect(w, r, back, http.StatusSeeOther)
					return
				}
			}
		}
	}
	if err := app.storeNotifyPrefs(ctx, userID, prefs); err != nil {
		logging.Errorf("notify: prompt answer for user %d: %v", userID, err)
		app.flash(ctx, "Your answer could not be saved; check the server log.")
	} else if r.FormValue("action") != "dismiss" {
		app.flash(ctx, "Notification choices saved. You can change them any time in Settings.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
