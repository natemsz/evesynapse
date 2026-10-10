package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

// eveCorpRoles are the in-game corporation roles a rule can ask for,
// as ESI names them. Director is how a CEO's deputies are told apart;
// the rest are the ones corporations hand out for a job.
var eveCorpRoles = []string{
	"Director", "Personnel_Manager", "Accountant", "Junior_Accountant", "Security_Officer",
	"Station_Manager", "Factory_Manager", "Fitting_Manager", "Contract_Manager",
	"Communications_Officer", "Diplomat", "Trader", "Auditor", "Skill_Plan_Manager", "Brand_Manager",
}

func validEVECorpRole(name string) bool {
	for _, role := range eveCorpRoles {
		if role == name {
			return true
		}
	}
	return false
}

// discordRuleView is one rule on the page.
type discordRuleView struct {
	ID   int64
	Who  string // "Members of the corporation", "Group: Logistics"
	Role string // the role's name in the server, or a note that it is gone
}

// discordWhoChoice is one thing a new rule can be about. Value is
// "kind" or "kind:ref".
type discordWhoChoice struct {
	Value, Label string
}

// ownerCorporations lists the corporations that belong to an owner: the
// corporation itself, or the corporations of the alliance that
// EveSynapse has linked characters in.
func (app *Application) ownerCorporations(ctx context.Context, owner discordOwner) []int64 {
	if owner.Kind == ownerCorporation {
		return []int64{owner.ID}
	}
	known, err := app.queries.ListKnownCorporations(ctx)
	if err != nil {
		logging.Errorf("discord: known corporations: %v", err)
		return nil
	}
	var out []int64
	for _, corp := range known {
		if alliance, _ := app.corpAlliance(ctx, corp); alliance == owner.ID {
			out = append(out, corp)
		}
	}
	return out
}

// discordWhoChoices lists what a rule in an owner's server can be about.
func (app *Application) discordWhoChoices(ctx context.Context, owner discordOwner) []discordWhoChoice {
	out := []discordWhoChoice{
		{ruleLinked, "Everyone who has connected EveSynapse"},
		{ruleMember, "Members of the " + owner.Kind},
		{ruleCEO, "CEO"},
	}
	if owner.Kind == ownerAlliance {
		out[2].Label = "CEOs of its corporations"
		for _, corp := range app.ownerCorporations(ctx, owner) {
			out = append(out, discordWhoChoice{ruleCorp + ":" + strconv.FormatInt(corp, 10), "Members of " + app.corpDisplayName(ctx, corp)})
		}
	}
	for _, role := range eveCorpRoles {
		out = append(out, discordWhoChoice{ruleEVERole + ":" + role, "In-game role: " + strings.ReplaceAll(role, "_", " ")})
	}
	groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("discord: groups of %s: %v", owner.key(), err)
	}
	for _, group := range groups {
		out = append(out, discordWhoChoice{ruleGroup + ":" + strconv.FormatInt(group.ID, 10), "Group: " + group.Name})
	}
	return out
}

// discordRuleWho says in words who a stored rule is about.
func (app *Application) discordRuleWho(ctx context.Context, owner discordOwner, rule db.DiscordRoleRule) string {
	switch rule.Kind {
	case ruleLinked:
		return "Everyone who has connected EveSynapse"
	case ruleMember:
		return "Members of the " + owner.Kind
	case ruleCEO:
		if owner.Kind == ownerAlliance {
			return "CEOs of its corporations"
		}
		return "CEO"
	case ruleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return "Members of " + app.corpDisplayName(ctx, corp)
	case ruleEVERole:
		return "In-game role: " + strings.ReplaceAll(rule.Ref, "_", " ")
	case ruleGroup:
		id, _ := strconv.ParseInt(rule.Ref, 10, 64)
		if group, err := app.queries.GetOrgGroup(ctx, id); err == nil {
			return "Group: " + group.Name
		}
		return "A group that no longer exists"
	}
	return rule.Kind
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
	owner := discordOwner{guild.OwnerKind, guild.OwnerID}
	out := make([]discordRuleView, 0, len(rules))
	for _, rule := range rules {
		name, exists := names[rule.RoleID]
		if !exists {
			name = "a role that is no longer in the server"
		}
		out = append(out, discordRuleView{ID: rule.ID, Who: app.discordRuleWho(ctx, owner, rule), Role: name})
	}
	return out
}

// parseRuleWho reads a "kind" or "kind:ref" from the form and checks
// it is something a rule in this owner's server may be about: a
// corporation has to belong to the owner, a group has to be the
// owner's own, an in-game role has to be one that exists.
func (app *Application) parseRuleWho(ctx context.Context, owner discordOwner, raw string) (kind, ref string, ok bool) {
	kind, ref, _ = strings.Cut(raw, ":")
	switch kind {
	case ruleLinked, ruleMember, ruleCEO:
		return kind, "", ref == ""
	case ruleEVERole:
		return kind, ref, validEVECorpRole(ref)
	case ruleCorp:
		corp, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || corp <= 0 {
			return "", "", false
		}
		for _, mine := range app.ownerCorporations(ctx, owner) {
			if mine == corp {
				return kind, strconv.FormatInt(corp, 10), true
			}
		}
	case ruleGroup:
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil {
			return "", "", false
		}
		group, gerr := app.queries.GetOrgGroup(ctx, id)
		if gerr == nil && group.OwnerKind == owner.Kind && group.OwnerID == owner.ID {
			return kind, strconv.FormatInt(id, 10), true
		}
	}
	return "", "", false
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
	owner := discordOwner{guild.OwnerKind, guild.OwnerID}
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
