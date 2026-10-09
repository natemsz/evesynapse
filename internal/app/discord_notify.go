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
// A corporation's channel. A new op is posted once in the channel set
// for its corporation (DISCORD_OPS_CHANNELS). That is said to a
// channel, not to an account, so it has its own memory
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
			return // this account cannot be reached right now: do not try the rest
		}
	}
}

// discordAnnounceOps posts the ops planned since the last look to
// their corporations' channels, each once. An op with no channel set
// for its corporation is marked as looked at, so it is not asked about
// again.
func (app *Application) discordAnnounceOps(ctx context.Context, now time.Time) (posted int) {
	if !app.discordHasBot() || len(app.cfg.discordOpsChannels) == 0 {
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
		channel := app.cfg.discordOpsChannels[op.CorporationID]
		if channel == "" {
			channel = app.cfg.discordOpsChannels[0]
		}
		if channel != "" {
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
			if err := app.discord.SendChannel(ctx, channel, discord.Message{Content: text}); err != nil {
				logging.Warnf("discord: announce op %d in channel %s: %v", op.ID, channel, err)
				if discord.IsStatus(err, http.StatusTooManyRequests) {
					return posted // asked to slow down: the rest wait for the next pass
				}
				if !discord.IsStatus(err, http.StatusForbidden) && !discord.IsStatus(err, http.StatusNotFound) {
					continue // a passing failure: try this op again next pass
				}
				// The bot cannot post there at all: do not ask every minute.
			} else {
				posted++
			}
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
		switch {
		case err == nil:
			app.flash(ctx, "Test message sent to "+link.Username+" on Discord.")
		case discord.IsStatus(err, http.StatusForbidden):
			app.flash(ctx, "Discord would not deliver the message. Join the server the bot is in, and allow direct messages from its members (the server's Privacy Settings), then try again.")
		default:
			logging.Warnf("discord: test message to user %d: %v", userID, err)
			app.flash(ctx, "The test message could not be sent; the server log has the reason.")
		}
	}
	http.Redirect(w, r, discordSettingsPath, http.StatusSeeOther)
}
