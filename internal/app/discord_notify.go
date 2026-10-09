package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Notifications on Discord. Two routes, both carried by the bot:
//
// Direct messages. An account that has connected Discord and ticked
// the box gets the same notifications there as in the top bar and by
// browser push; it is one more way of delivering what notify.go
// already decided to say, with the same per-kind and per-character
// settings.
//
// A server's channel. A new op is posted once in the ops channel of
// each server of its corporation, and of its alliance's server when
// the corporation agreed to that (discord_servers.go). That is said to
// a channel, not to an account, so it has its own memory
// (ops.discord_announced_at) and does not depend on anyone's settings.
//
// The bot only ever sends. Nothing is read from Discord.
// ---------------------------------------------------------------------------

const (
	// discordUserBudget bounds the time one account's messages may
	// take, so a slow Discord cannot hold up the worker.
	discordUserBudget = 15 * time.Second
	// discordAnnounceAge: an op is posted to its channel only while it
	// is this new. An install that turns the bot on does not post its
	// whole calendar.
	discordAnnounceAge = time.Hour
)

// discordHasBot reports whether the bot can send at all.
func (app *Application) discordHasBot() bool {
	return app.discord != nil && app.discord.Config().HasBot()
}

// siteURL makes a site path into a full address for a message read
// outside the site. Without a known public address the path alone is
// returned.
func (app *Application) siteURL(path string) string {
	return app.cfg.publicOrigin() + notifyTarget(path)
}

// discordToUser sends one worker pass's new notifications to the
// account as direct messages, if it asked for that. Failures are
// logged and lose nothing: the notifications are already stored.
func (app *Application) discordToUser(ctx context.Context, userID int64, events []notifyEvent) {
	if !app.discordHasBot() || len(events) == 0 {
		return
	}
	link, err := app.queries.GetDiscordLink(ctx, userID)
	if err != nil || !link.DmNotifications {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, discordUserBudget)
	defer cancel()
	// The same rule as browser push: a few are sent one by one, a
	// burst as a single line with a count.
	for _, msg := range pushMessagesFor(events) {
		text := msg.Body + "\n" + app.siteURL(msg.URL)
		if err := app.discord.SendDM(ctx, link.DiscordID, discord.Message{Content: text}); err != nil {
			logging.Warnf("discord: message to user %d: %v", userID, err)
			app.discordNoteDM(ctx, link, err)
			return // this account cannot be reached right now: do not try the rest
		}
	}
	app.discordNoteDM(ctx, link, nil)
}

// Why a direct message did not arrive, as the settings page says it.
const (
	discordDMRefused = "Discord refused to deliver your last notification. In Discord, open the server you share with the bot, then Privacy Settings, and allow direct messages from server members. You also have to be in a server the bot is in."
	discordDMFailed  = "Your last notification could not be sent to Discord. It will be tried again with the next one."
)

// discordNoteDM records how the last direct message to an account
// went, so that the person it concerns can see it: a refusal used to
// be written only to the server's log. A message that goes through
// clears it.
func (app *Application) discordNoteDM(ctx context.Context, link db.DiscordLink, err error) {
	problem := ""
	switch {
	case err == nil:
	case discord.IsStatus(err, http.StatusForbidden):
		problem = discordDMRefused
	default:
		problem = discordDMFailed
	}
	if problem == link.DmProblem {
		return
	}
	// Recorded even if the caller's time has run out.
	if serr := app.queries.SetDiscordDMProblem(context.WithoutCancel(ctx), db.SetDiscordDMProblemParams{
		UserID: link.UserID, DmProblem: problem, DmProblemAt: timeSet(time.Now().UTC()),
	}); serr != nil {
		logging.Errorf("discord: record message problem of user %d: %v", link.UserID, serr)
	}
}

// discordAnnounceOps posts the ops planned since the last look, each
// once, in every channel set for it. An op with nowhere to go is
// marked as looked at, so it is not asked about again.
func (app *Application) discordAnnounceOps(ctx context.Context, now time.Time) (posted int) {
	if !app.discordHasBot() {
		return 0
	}
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil || len(guilds) == 0 {
		return 0
	}
	ops, err := app.queries.ListOpsToAnnounceOnDiscord(ctx, db.ListOpsToAnnounceOnDiscordParams{
		Now: now, CreatedAfter: now.Add(-discordAnnounceAge),
	})
	if err != nil {
		logging.Errorf("discord: ops to announce: %v", err)
		return 0
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			return posted
		}
		retry := false
		targets := app.discordOpsChannels(ctx, guilds, op.CorporationID)
		if len(targets) > 0 {
			lead := "New op"
			if name, settled := app.resolvedCorpName(ctx, op.CorporationID); settled && name != "" {
				lead += " for " + name
			}
			text := fmt.Sprintf("**%s: %s**\n%s EVE time", lead, op.Title, op.StartsAt.UTC().Format("Monday, Jan 2 15:04"))
			if fc := app.displayCharacter(ctx, op.FcCharacterID); op.FcCharacterID != 0 && fc != "" {
				text += " · FC " + fc
			}
			if op.Doctrine != "" {
				text += " · " + op.Doctrine
			}
			text += "\nSign up: " + app.siteURL(opURL(op.ID))
			for _, guild := range targets {
				err := app.discord.SendChannel(ctx, guild.OpsChannel, discord.Message{Content: text})
				switch {
				case err == nil:
					posted++
				case discord.IsStatus(err, http.StatusTooManyRequests):
					logging.Warnf("discord: announce op %d in %s: %v", op.ID, guild.Name, err)
					return posted // asked to slow down: the rest wait for the next pass
				case discord.IsStatus(err, http.StatusForbidden), discord.IsStatus(err, http.StatusNotFound):
					// The bot cannot post there at all: not worth asking every minute.
					logging.Warnf("discord: announce op %d in %s: %v", op.ID, guild.Name, err)
				default:
					logging.Warnf("discord: announce op %d in %s: %v", op.ID, guild.Name, err)
					retry = true // a passing failure
				}
			}
		}
		if retry && posted == 0 {
			continue // nothing got out: try this op again next pass
		}
		if err := app.queries.SetOpDiscordAnnounced(ctx, db.SetOpDiscordAnnouncedParams{ID: op.ID, AnnouncedAt: timeSet(now)}); err != nil {
			logging.Errorf("discord: mark op %d announced: %v", op.ID, err)
		}
	}
	return posted
}

// handleDiscordSettings stores whether the account wants its
// notifications as direct messages.
func (app *Application) handleDiscordSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err == nil && userID != 0 {
		if err := app.queries.SetDiscordDMNotifications(ctx, db.SetDiscordDMNotificationsParams{
			UserID: userID, DmNotifications: r.Form.Get("dm") == "1",
		}); err != nil {
			logging.Errorf("discord: save settings of user %d: %v", userID, err)
			app.flash(ctx, "The Discord setting could not be saved; check the server log.")
		} else {
			app.flash(ctx, "Discord setting saved.")
		}
	}
	http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
}

// handleDiscordRolesRefresh checks the account's roles in every server
// now, without waiting for the worker's turn: for someone who has just
// joined a server.
func (app *Application) handleDiscordRolesRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	link, linked := app.discordLinkFor(r, userID)
	switch {
	case !app.discordHasBot():
		app.flash(ctx, "The Discord bot is not set up on this site.")
	case !linked:
		app.flash(ctx, "Connect a Discord account first.")
	default:
		budget, cancel := context.WithTimeout(ctx, discordUserBudget)
		defer cancel()
		servers, err := app.discordSyncAccount(budget, link, time.Now().UTC())
		if err != nil {
			logging.Errorf("discord: refresh roles of user %d: %v", userID, err)
			app.flash(ctx, "Your roles could not be checked; the server log has the reason.")
		} else {
			app.flash(ctx, fmt.Sprintf("Your Discord roles were checked. Your characters earn roles in %d server(s); you hold them in those you have joined.", servers))
		}
	}
	http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
}

// handleDiscordTest sends the account a direct message, so the user
// can see that the bot reaches them, and says why when it does not.
func (app *Application) handleDiscordTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	link, linked := app.discordLinkFor(r, userID)
	switch {
	case !app.discordHasBot():
		app.flash(ctx, "The Discord bot is not set up on this server.")
	case !linked:
		app.flash(ctx, "Connect a Discord account first.")
	default:
		err := app.discord.SendDM(ctx, link.DiscordID, discord.Message{
			Content: "EveSynapse can reach you here. Notifications you switch on will arrive as messages like this one.\n" + app.siteURL(discordSettingsPath),
		})
		app.discordNoteDM(ctx, link, err)
		switch {
		case err == nil:
			app.flash(ctx, "Test message sent to "+link.Username+" on Discord.")
		case discord.IsStatus(err, http.StatusForbidden):
			app.flash(ctx, "Discord would not deliver the message. In Discord, open the server you share with the bot, then Privacy Settings, and allow direct messages from server members; then try again.")
		default:
			logging.Warnf("discord: test message to user %d: %v", userID, err)
			app.flash(ctx, "The test message could not be sent; the server log has the reason.")
		}
	}
	http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
}
