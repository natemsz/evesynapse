package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Removing people who do not belong from a server. Off unless the
// directors of a server switch it on.
//
// When on, the bot goes through the whole member list of the server
// and removes (kicks; nobody is banned, and anyone can come back) each
// member who is owed no role by the server's rules. That is not only
// people the bot once gave a role to: it is also everyone who never
// connected EveSynapse. Switching it on in a busy server removes every
// such person, which is the point, and is why it is guarded:
//
//   - Never removed: bots, the server's owner, and anyone holding a
//     role the directors marked as exempt (guests, diplomats).
//   - Nobody is removed in their first 24 hours in the server, so that
//     a newcomer has time to connect.
//   - A server with no rules removes nobody: with no rules everybody
//     is owed nothing, and the switch would empty the server.
//   - A member list that could not be read to its end is not acted on.
//   - The page shows who would be removed before anything is switched
//     on, and every removal is written to the log and to the server's
//     own audit log with the reason.
//
// It needs two things from Discord beyond the rest: the Kick Members
// permission in the server, and the Server Members intent switched on
// for the application (to read the member list at all).
// ---------------------------------------------------------------------------

const (
	// kickGrace: nobody is removed this soon after joining.
	kickGrace = 24 * time.Hour
	// kickEvery: a server's member list is gone through this often.
	kickEvery = 30 * time.Minute
	// kickPerPass bounds removals in one worker pass; the rest follow
	// on the next ones.
	kickPerPass = 10
	// kickMostMembers: a server with more members than this is not
	// gone through at all, since the list could not be read whole.
	kickMostMembers = 20000
)

// kickPlan is what going through a server's members found.
type kickPlan struct {
	Members  int              // members looked at
	Complete bool             // the list was read to its end
	NoRules  bool             // the server has no rules: nobody is removed
	Remove   []discord.Member // who is owed nothing and not protected
}

// names lists up to n of the members to remove, for showing.
func (p kickPlan) names(n int) string {
	var out []string
	for i, m := range p.Remove {
		if i == n {
			out = append(out, fmt.Sprintf("and %d more", len(p.Remove)-n))
			break
		}
		out = append(out, m.User.Username)
	}
	return strings.Join(out, ", ")
}

// discordKickPlan goes through a server's members and lists who would
// be removed now. It changes nothing.
func (app *Application) discordKickPlan(ctx context.Context, guild db.DiscordGuild, now time.Time) (kickPlan, error) {
	rules, err := app.queries.ListDiscordRoleRulesForGuild(ctx, guild.GuildID)
	if err != nil {
		return kickPlan{}, err
	}
	if len(rules) == 0 {
		return kickPlan{NoRules: true, Complete: true}, nil
	}
	members, complete, err := app.discord.ListMembers(ctx, guild.GuildID, kickMostMembers)
	if err != nil {
		return kickPlan{}, err
	}
	plan := kickPlan{Members: len(members), Complete: complete}
	owner, err := app.discord.GuildOwner(ctx, guild.GuildID)
	if err != nil {
		return kickPlan{}, err
	}
	exempt := roleSet{}
	exempt.add(splitRoles(guild.KickExemptRoles)...)
	for _, m := range members {
		if m.User.Bot || m.User.ID == owner || now.Sub(m.JoinedAt) < kickGrace {
			continue
		}
		protected := false
		for _, role := range m.Roles {
			protected = protected || exempt[role]
		}
		if protected {
			continue
		}
		// Connected, and owed a role here: stays.
		if link, lerr := app.queries.GetDiscordLinkByDiscordID(ctx, m.User.ID); lerr == nil {
			st, serr := app.discordStandingFor(ctx, link.UserID)
			if serr != nil {
				return kickPlan{}, serr // not knowing is not a reason to remove
			}
			if len(discordWanted(guild, rules, st)) > 0 {
				continue
			}
		}
		plan.Remove = append(plan.Remove, m)
	}
	return plan, nil
}

// discordKickPass removes, in each server that has it switched on, the
// members who do not belong. A server is gone through at most every
// kickEvery, and at most kickPerPass members are removed a pass.
func (app *Application) discordKickPass(ctx context.Context, now time.Time) (removed int) {
	if !app.discordHasBot() {
		return 0
	}
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil {
		logging.Errorf("discord: list servers: %v", err)
		return 0
	}
	for _, guild := range guilds {
		if ctx.Err() != nil || removed >= kickPerPass {
			return removed
		}
		if !guild.KickEnabled || (guild.KickCheckedAt.Valid && now.Sub(guild.KickCheckedAt.Time) < kickEvery) {
			continue
		}
		plan, err := app.discordKickPlan(ctx, guild, now)
		done := true
		switch {
		case err != nil:
			logging.Warnf("discord: go through members of %s: %v", guild.Name, err)
		case !plan.Complete:
			logging.Warnf("discord: %s has too many members to go through; nobody is removed", guild.Name)
		default:
			for _, m := range plan.Remove {
				if removed >= kickPerPass {
					done = false // the rest on the next pass
					break
				}
				if err := app.discord.Kick(ctx, guild.GuildID, m.User.ID, "EveSynapse: owed no role by this server's rules"); err != nil {
					logging.Warnf("discord: remove %s (%s) from %s: %v", m.User.Username, m.User.ID, guild.Name, err)
					if discordStop(err) {
						break
					}
					continue
				}
				removed++
				logging.Infof("discord: removed %s (%s) from %s: owed no role by its rules", m.User.Username, m.User.ID, guild.Name)
				// Nothing is held there any more.
				_ = app.queries.DeleteDiscordRoleGrant(ctx, db.DeleteDiscordRoleGrantParams{DiscordID: m.User.ID, GuildID: guild.GuildID})
			}
		}
		if done {
			if err := app.queries.SetDiscordGuildKickChecked(ctx, db.SetDiscordGuildKickCheckedParams{GuildID: guild.GuildID, KickCheckedAt: timeSet(now)}); err != nil {
				logging.Errorf("discord: note members of %s gone through: %v", guild.GuildID, err)
			}
		}
	}
	return removed
}

// handleDiscordKickPreview says who would be removed from a server if
// removals were on, and changes nothing.
func (app *Application) handleDiscordKickPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	if !ok {
		back("That server is not yours to change.")
		return
	}
	plan, err := app.discordKickPlan(ctx, guild, time.Now().UTC())
	switch {
	case err != nil && discord.IsStatus(err, http.StatusForbidden):
		back("Discord would not hand over that server's member list. Switch on the Server Members intent for the application (Developer Portal, Bot), and check the bot is still in the server.")
	case err != nil:
		logging.Warnf("discord: preview removals in %s: %v", guild.GuildID, err)
		back("The member list could not be read; the server log has the reason.")
	case plan.NoRules:
		back(guild.Name + " has no role rules, so nobody would be removed. Add rules first: with none, nobody is owed a role.")
	case !plan.Complete:
		back(guild.Name + " has too many members to go through; nobody would be removed.")
	case len(plan.Remove) == 0:
		back(fmt.Sprintf("Nobody would be removed from %s: all %d members are owed a role, protected, or joined in the last 24 hours.", guild.Name, plan.Members))
	default:
		back(fmt.Sprintf("%d of %d members would be removed from %s: %s.", len(plan.Remove), plan.Members, guild.Name, plan.names(15)))
	}
}

// handleDiscordKickSave stores a server's removal settings. Switching
// removals on is refused for a server with no rules.
func (app *Application) handleDiscordKickSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	if !ok || r.ParseForm() != nil {
		back("That server is not yours to change.")
		return
	}
	roles, err := app.discord.GuildRoles(ctx, guild.GuildID)
	if err != nil {
		logging.Warnf("discord: read roles of %s: %v", guild.GuildID, err)
		back("Discord would not list that server's roles. Is the bot still in it?")
		return
	}
	known := map[string]bool{}
	for _, role := range roles {
		known[role.ID] = true
	}
	exempt := roleSet{}
	for _, id := range r.Form["exempt"] {
		if known[id] {
			exempt.add(id)
		}
	}
	enabled := r.Form.Get("kick_enabled") == "1"
	if enabled {
		rules, rerr := app.queries.ListDiscordRoleRulesForGuild(ctx, guild.GuildID)
		if rerr != nil || len(rules) == 0 {
			back("Removals were not switched on: " + guild.Name + " has no role rules, and with none everybody would be removed.")
			return
		}
	}
	if err := app.queries.SetDiscordGuildKick(ctx, db.SetDiscordGuildKickParams{
		GuildID: guild.GuildID, KickEnabled: enabled, KickExemptRoles: strings.Join(exempt.list(), ","),
	}); err != nil {
		logging.Errorf("discord: save removals of %s: %v", guild.GuildID, err)
		back("The settings could not be saved; check the server log.")
		return
	}
	logging.Infof("discord: user %d set removals in server %s: on=%v, exempt roles %q", userID, guild.GuildID, enabled, strings.Join(exempt.list(), ","))
	if enabled {
		back("Removals are on for " + guild.Name + ". Members owed no role are removed within half an hour, a few at a time.")
		return
	}
	back("Removals are off for " + guild.Name + ".")
}
