package app

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Which roles can open which channels. A server's directors pick a
// channel and tick the roles that may see it; the bot then keeps that
// channel private to those roles:
//
//  1. the bot itself is let in by name first, so that it cannot shut
//     itself out,
//  2. each ticked role is let in,
//  3. everyone else (@everyone) is shut out,
//  4. a role that was ticked before and is not now has its entry
//     removed.
//
// The order matters: nobody who should see the channel loses sight of
// it for a moment, and if a step fails the channel is left more open
// than asked, never locked with nobody in it.
//
// Together with the role rules this is the whole chain: a rule gives
// someone a role because of what they are in EVE, and the role opens
// the channels. The bot only touches channels a director has listed
// here. Unticking every role stops the bot managing a channel; it is
// left hidden, since reopening a private channel is not something to
// do as a side effect.
//
// Discord lets a bot do this with the Manage Roles permission it
// already has, provided it can see the channel itself.
// ---------------------------------------------------------------------------

// discordChannelView is one managed channel on the page.
type discordChannelView struct {
	ID    string
	Name  string
	Roles string // the roles that can open it, by name
}

// channelLabel names a channel the way Discord shows its kind.
func channelLabel(ch discord.Channel) string {
	switch ch.Type {
	case 4:
		return "Category: " + ch.Name
	case 2, 13:
		return "Voice: " + ch.Name
	}
	return "#" + ch.Name
}

// discordChannelViews lists a server's managed channels for the page.
func (app *Application) discordChannelViews(ctx context.Context, guildID string, channels []discord.Channel, roles []discord.Role) []discordChannelView {
	rows, err := app.queries.ListDiscordChannelAccess(ctx, guildID)
	if err != nil {
		logging.Errorf("discord: channel access of %s: %v", guildID, err)
		return nil
	}
	channelName := map[string]string{}
	for _, ch := range channels {
		channelName[ch.ID] = channelLabel(ch)
	}
	roleName := map[string]string{}
	for _, role := range roles {
		roleName[role.ID] = role.Name
	}
	byChannel := map[string][]string{}
	var order []string
	for _, row := range rows {
		if _, seen := byChannel[row.ChannelID]; !seen {
			order = append(order, row.ChannelID)
		}
		name, known := roleName[row.RoleID]
		if !known {
			name = "a role that is no longer in the server"
		}
		byChannel[row.ChannelID] = append(byChannel[row.ChannelID], name)
	}
	out := make([]discordChannelView, 0, len(order))
	for _, id := range order {
		name, known := channelName[id]
		if !known {
			name = "a channel that is no longer in the server"
		}
		sort.Strings(byChannel[id])
		out = append(out, discordChannelView{ID: id, Name: name, Roles: strings.Join(byChannel[id], ", ")})
	}
	return out
}

// discordApplyChannel makes a channel private to the roles in now,
// given the roles the bot let in before. With now empty it only takes
// the old roles' entries away.
func (app *Application) discordApplyChannel(ctx context.Context, guildID, channelID string, now, before []string) error {
	if len(now) > 0 {
		bot, err := app.discord.BotUserID(ctx)
		if err != nil {
			return err
		}
		if err := app.discord.AllowChannel(ctx, channelID, bot, discord.ForMember); err != nil {
			return err
		}
		for _, role := range now {
			if err := app.discord.AllowChannel(ctx, channelID, role, discord.ForRole); err != nil {
				return err
			}
		}
		// @everyone is the role whose id is the server's own.
		if err := app.discord.DenyChannel(ctx, channelID, guildID, discord.ForRole); err != nil {
			return err
		}
	}
	keep := roleSet{}
	keep.add(now...)
	for _, role := range before {
		if keep[role] {
			continue
		}
		if err := app.discord.ClearChannel(ctx, channelID, role); err != nil && !discord.IsStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return nil
}

// handleDiscordChannelSave sets which roles can open one channel. The
// channel and every role have to be ones Discord lists for that very
// server. Discord is changed first and the record after, so the record
// never claims more than was done.
func (app *Application) handleDiscordChannelSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := func(message string) {
		app.flash(ctx, message)
		http.Redirect(w, r, discordServersPath, http.StatusSeeOther)
	}
	guild, ok := app.discordGuildFor(r, userID)
	if !ok || r.ParseForm() != nil {
		back("That server is not yours to change.")
		return
	}
	channels, cerr := app.discord.GuildChannels(ctx, guild.GuildID)
	roles, rerr := app.discord.GuildRoles(ctx, guild.GuildID)
	if cerr != nil || rerr != nil {
		logging.Warnf("discord: read server %s: %v %v", guild.GuildID, cerr, rerr)
		back("Discord would not list that server's channels and roles. Is the bot still in it?")
		return
	}
	channelID, label := r.Form.Get("channel"), ""
	for _, ch := range channels {
		if ch.ID == channelID {
			label = channelLabel(ch)
		}
	}
	if label == "" {
		back("That is not a channel of that server.")
		return
	}
	giveable := map[string]bool{}
	for _, role := range roles {
		giveable[role.ID] = !role.Managed
	}
	now := roleSet{}
	for _, id := range r.Form["role"] {
		if !giveable[id] {
			back("One of those is not a role of that server.")
			return
		}
		now.add(id)
	}
	rows, err := app.queries.ListDiscordChannelAccess(ctx, guild.GuildID)
	if err != nil {
		logging.Errorf("discord: channel access of %s: %v", guild.GuildID, err)
		back("The channel's settings could not be read; check the server log.")
		return
	}
	var before []string
	for _, row := range rows {
		if row.ChannelID == channelID {
			before = append(before, row.RoleID)
		}
	}
	if err := app.discordApplyChannel(ctx, guild.GuildID, channelID, now.list(), before); err != nil {
		logging.Warnf("discord: set access of channel %s in %s: %v", channelID, guild.GuildID, err)
		if discord.IsStatus(err, http.StatusForbidden) {
			back("Discord would not let the bot change " + label + ". The bot has to be able to see the channel itself, and its own role has to sit above the roles being let in.")
			return
		}
		back("The channel could not be changed; the server log has the reason. Nothing was recorded, and the channel may be partly changed: save again to finish.")
		return
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		q := app.queries.WithTx(tx)
		err = q.DeleteDiscordChannelAccessForChannel(ctx, db.DeleteDiscordChannelAccessForChannelParams{GuildID: guild.GuildID, ChannelID: channelID})
		for _, role := range now.list() {
			if err != nil {
				break
			}
			err = q.InsertDiscordChannelAccess(ctx, db.InsertDiscordChannelAccessParams{
				GuildID: guild.GuildID, ChannelID: channelID, RoleID: role, SetBy: userID, SetAt: time.Now().UTC(),
			})
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		logging.Errorf("discord: record access of channel %s: %v", channelID, err)
		back("The channel was changed on Discord but could not be recorded here; save again.")
		return
	}
	logging.Infof("discord: user %d set channel %s of server %s to roles %q", userID, channelID, guild.GuildID, strings.Join(now.list(), ","))
	if len(now) == 0 {
		back(label + " is no longer managed. It is still hidden from everyone it was hidden from; open it in Discord if that is what you want.")
		return
	}
	back(label + " is now private to the roles you ticked.")
}
