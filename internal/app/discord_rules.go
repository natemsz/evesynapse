package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// A server's role rules, as its directors see and change them on
// /discord/servers. What a rule means is in discord_roles.go; this is
// the list, the form that adds one, and the checks on what may be
// added.
// ---------------------------------------------------------------------------

// discordRuleView is one rule on the page.
type discordRuleView struct {
	ID   int64
	Who  string // "Members of the corporation", "Group: Logistics"
	Role string // the role's name in the server, or a note that it is gone
}

// discordRuleViews builds a server's rule list for the page.
func (app *Application) discordRuleViews(ctx context.Context, guild db.DiscordGuild, roles []discord.Role) []discordRuleView {
	rules, err := app.queries.ListDiscordRoleRulesForGuild(ctx, guild.GuildID)
	if err != nil {
		logging.Errorf("discord: rules of %s: %v", guild.GuildID, err)
		return nil
	}
	names := map[string]string{}
	for _, role := range roles {
		names[role.ID] = role.Name
	}
	owner := orgOwner{guild.OwnerKind, guild.OwnerID}
	out := make([]discordRuleView, 0, len(rules))
	for _, rule := range rules {
		name, exists := names[rule.RoleID]
		if !exists {
			name = "a role that is no longer in the server"
		}
		out = append(out, discordRuleView{ID: rule.ID, Who: app.ruleWho(ctx, owner, rule), Role: name})
	}
	return out
}

// handleDiscordRuleAdd adds a rule to a server. The role has to be one
// Discord lists for that very server and that can be given.
func (app *Application) handleDiscordRuleAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	if !ok || r.ParseForm() != nil {
		back("That server is not yours to change.")
		return
	}
	owner := orgOwner{guild.OwnerKind, guild.OwnerID}
	kind, ref, ok := app.parseRuleWho(ctx, owner, r.Form.Get("who"))
	if !ok {
		back("That is not something a rule in this server can be about.")
		return
	}
	roles, err := app.discord.GuildRoles(ctx, guild.GuildID)
	if err != nil {
		logging.Warnf("discord: read roles of %s: %v", guild.GuildID, err)
		back("Discord would not list that server's roles. Is the bot still in it?")
		return
	}
	roleID, giveable := r.Form.Get("role"), false
	for _, role := range roles {
		giveable = giveable || (role.ID == roleID && !role.Managed)
	}
	if !giveable {
		back("That is not a role of that server that can be given.")
		return
	}
	if err := app.queries.InsertDiscordRoleRule(ctx, db.InsertDiscordRoleRuleParams{
		GuildID: guild.GuildID, Kind: kind, Ref: ref, RoleID: roleID, CreatedBy: userID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("discord: add rule to %s: %v", guild.GuildID, err)
		back("The rule could not be saved; check the server log.")
		return
	}
	logging.Infof("discord: user %d added rule %s %q -> role %s in server %s", userID, kind, ref, roleID, guild.GuildID)
	back("Rule added. Roles follow within a few minutes.")
}

// handleDiscordRuleRemove removes a rule. Whoever held the role only
// through it loses the role on the worker's next pass.
func (app *Application) handleDiscordRuleRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	ruleID, err := strconv.ParseInt(chi.URLParam(r, "ruleID"), 10, 64)
	if !ok || err != nil {
		back("That server is not yours to change.")
		return
	}
	if err := app.queries.DeleteDiscordRoleRule(ctx, db.DeleteDiscordRoleRuleParams{ID: ruleID, GuildID: guild.GuildID}); err != nil {
		logging.Errorf("discord: remove rule %d of %s: %v", ruleID, guild.GuildID, err)
		back("The rule could not be removed; check the server log.")
		return
	}
	logging.Infof("discord: user %d removed rule %d of server %s", userID, ruleID, guild.GuildID)
	back(fmt.Sprintf("Rule removed from %s. The role is taken back from whoever held it only through that rule, within a few minutes.", guild.Name))
}
