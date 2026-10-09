package app

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Connecting a Discord account (internal/discord). A signed-in user is
// sent to Discord, approves, and comes back with a code; the code is
// traded for the account's id and name, which are stored against the
// EveSynapse account. That is all that is kept: the Discord token is
// used once and dropped, so nothing here can act as the user on
// Discord.
//
// The link is what the Discord features hang off: notifications as
// direct messages (discord_notify.go) and roles in the server
// (discord_roles.go).
// ---------------------------------------------------------------------------

// sessionDiscordState holds the sign-in's single-use state value.
const sessionDiscordState = "discord_oauth_state"

// discordFromConfig builds the Discord client, or nil when the install
// has no Discord settings at all.
func discordFromConfig(cfg Config) *discord.Client {
	dc := discord.Config{
		ClientID:     cfg.discordClientID,
		ClientSecret: cfg.discordClientSecret,
		BotToken:     cfg.discordBotToken,
		GuildID:      cfg.discordGuildID,
	}
	if origin := cfg.publicOrigin(); origin != "" {
		dc.RedirectURL = origin + "/discord/callback"
	}
	if !dc.CanLink() && !dc.HasBot() {
		return nil
	}
	if cfg.discordGuildID != "" && !discord.ValidID(cfg.discordGuildID) {
		logging.Errorf("evesynapse: DISCORD_GUILD_ID=%q is not a Discord server id; the bot is off", cfg.discordGuildID)
		dc.GuildID = ""
	}
	return discord.New(dc, nil, "")
}

// discordCanLink reports whether "Connect Discord" is offered.
func (app *Application) discordCanLink() bool {
	return app.discord != nil && app.discord.Config().CanLink()
}

// discordLinkFor returns the account's link, if it has one.
func (app *Application) discordLinkFor(r *http.Request, userID int64) (db.DiscordLink, bool) {
	link, err := app.queries.GetDiscordLink(r.Context(), userID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("discord: link of user %d: %v", userID, err)
		}
		return db.DiscordLink{}, false
	}
	return link, true
}

// discordSettingsPath is where the Discord section lives, and where
// its actions come back to.
const discordSettingsPath = "/notifications/settings"

func (app *Application) handleDiscordConnect(w http.ResponseWriter, r *http.Request) {
	if !app.discordCanLink() {
		app.flash(r.Context(), "Discord is not set up on this server.")
		http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
		return
	}
	state, err := newOAuthState()
	if err != nil {
		logging.Errorf("discord: generate state: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	app.sessions.Put(r.Context(), sessionDiscordState, state)
	http.Redirect(w, r, app.discord.AuthURL(state), http.StatusFound)
}

func (app *Application) handleDiscordCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	back := func(message string) {
		app.flash(ctx, message)
		http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
	}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))

	// State is single-use: read it, clear it, then constant-time compare.
	want := app.sessions.GetString(ctx, sessionDiscordState)
	app.sessions.Remove(ctx, sessionDiscordState)
	got := q.Get("state")
	if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		logging.Warnf("discord callback: state mismatch")
		back("Connecting Discord did not complete. Please try again.")
		return
	}
	if !app.discordCanLink() || userID == 0 {
		back("Discord is not set up on this server.")
		return
	}
	// The user pressed Cancel on Discord's page, or Discord refused.
	// Only the error code is logged.
	if derr := q.Get("error"); derr != "" {
		logging.Warnf("discord callback: Discord returned error=%s", derr)
		back("Discord was not connected.")
		return
	}
	code := q.Get("code")
	if code == "" {
		back("Discord was not connected.")
		return
	}
	account, err := app.discord.Identify(ctx, code)
	if err != nil {
		logging.Errorf("discord callback: identify for user %d: %v", userID, err)
		back("Discord could not confirm the account. Please try again.")
		return
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		qtx := app.queries.WithTx(tx)
		if err = qtx.DeleteOtherDiscordLinks(ctx, db.DeleteOtherDiscordLinksParams{DiscordID: account.ID, UserID: userID}); err == nil {
			err = qtx.UpsertDiscordLink(ctx, db.UpsertDiscordLinkParams{
				UserID: userID, DiscordID: account.ID, Username: clip(account.DisplayName(), 100), LinkedAt: time.Now().UTC(),
			})
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		logging.Errorf("discord callback: store link for user %d: %v", userID, err)
		back("The Discord account could not be saved; check the server log.")
		return
	}
	back("Discord connected as " + account.DisplayName() + ".")
}

func (app *Application) handleDiscordDisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID != 0 {
		// The roles EveSynapse gave are taken back first, while the
		// account they were given to is still known.
		app.discordDropRoles(ctx, userID)
		if err := app.queries.DeleteDiscordLink(ctx, userID); err != nil {
			logging.Errorf("discord: remove link of user %d: %v", userID, err)
			app.flash(ctx, "The Discord account could not be disconnected; check the server log.")
			http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
			return
		}
		app.flash(ctx, "Discord disconnected.")
	}
	http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
}
