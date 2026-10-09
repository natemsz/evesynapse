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
// Discord roles from what EveSynapse knows. In each server the bot was
// added to (discord_servers.go), its directors may name two roles:
//
//	the linked role   for anyone there who has connected an EveSynapse
//	                  account with a character whose link to EVE works
//	the member role   for those of them with such a character in the
//	                  server's corporation, or in a corporation of the
//	                  server's alliance
//
// What a member role rests on, and so how far to trust it: an
// EveSynapse account holding that Discord account has a character
// whose link to EVE is in good standing and which the last sync saw in
// the corporation. It follows a character leaving within a sync or
// two, not at that instant.
//
// In a server the bot only touches the roles named in that server's
// settings, and roles it gave there earlier. Every other role a member
// has is left exactly as it is, whoever gave it.
//
// Taking back is not left to the account still being there. What the
// bot gives is written down against the Discord account itself
// (discord_role_grants, with no tie to the EveSynapse account), and
// each pass takes back whatever is written down for a Discord account
// that no EveSynapse account is connected to any more. So deleting an
// account, disconnecting Discord, or connecting a different Discord
// account cannot leave roles behind, whichever way it was done.
// ---------------------------------------------------------------------------

const (
	// discordRolesPerPass bounds how many members one worker pass
	// looks up on Discord, and discordRolesRecheck is how often a
	// member whose roles look right is checked against Discord anyway
	// (a role removed by hand is given back then, and someone who has
	// since joined the server gets theirs).
	discordRolesPerPass = 20
	discordRolesRecheck = 6 * time.Hour
)

// roleSet gathers role ids without repeats; list returns them sorted.
type roleSet map[string]bool

func (s roleSet) add(roles ...string) {
	for _, role := range roles {
		if role != "" {
			s[role] = true
		}
	}
}

func (s roleSet) list() []string {
	out := make([]string, 0, len(s))
	for role := range s {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func splitRoles(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}

// discordStanding is what an account's working characters amount to:
// whether it has any, and the corporations and alliances they are in.
type discordStanding struct {
	working   bool
	corps     map[int64]bool
	alliances map[int64]bool
}

// discordStandingFor works out an account's standing from stored data.
// A corporation whose alliance is not known yet counts for itself
// only.
func (app *Application) discordStandingFor(ctx context.Context, userID int64) (discordStanding, error) {
	st := discordStanding{corps: map[int64]bool{}, alliances: map[int64]bool{}}
	n, err := app.queries.CountLinkedCharactersByUser(ctx, userID)
	if err != nil || n == 0 {
		return st, err
	}
	st.working = true
	corps, err := app.queries.ListLinkedCorporationsByUser(ctx, userID)
	if err != nil {
		return st, err
	}
	for _, corp := range corps {
		st.corps[corp] = true
		if alliance, _ := app.corpAlliance(ctx, corp); alliance != 0 {
			st.alliances[alliance] = true
		}
	}
	return st, nil
}

// discordWanted is the roles an account of that standing should hold
// in a server now, sorted.
func discordWanted(guild db.DiscordGuild, st discordStanding) []string {
	set := roleSet{}
	if !st.working {
		return set.list()
	}
	set.add(guild.RoleLinked)
	if (guild.OwnerKind == ownerCorporation && st.corps[guild.OwnerID]) ||
		(guild.OwnerKind == ownerAlliance && st.alliances[guild.OwnerID]) {
		set.add(guild.RoleMember)
	}
	return set.list()
}

// discordApplyRoles makes one member's roles in a server match wanted,
// among the roles named in the server's settings and any in also
// (roles given earlier that may since have left the settings). It
// reports the set now held, and whether the account is in the server
// at all.
func (app *Application) discordApplyRoles(ctx context.Context, guild db.DiscordGuild, discordID string, wanted, also []string) (applied string, member bool, err error) {
	held, err := app.discord.MemberRoles(ctx, guild.GuildID, discordID)
	if err != nil {
		if discord.IsStatus(err, http.StatusNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	has := roleSet{}
	has.add(held...)
	want := roleSet{}
	want.add(wanted...)
	touch := roleSet{}
	touch.add(guild.RoleLinked, guild.RoleMember)
	touch.add(also...)
	for _, role := range touch.list() {
		if want[role] == has[role] {
			continue
		}
		if err := app.discord.SetRole(ctx, guild.GuildID, discordID, role, want[role]); err != nil {
			return "", true, err
		}
	}
	return strings.Join(wanted, ","), true, nil
}

// discordStop reports whether err is one that every other member of
// the same server would meet too: asked to slow down, or the bot not
// being allowed.
func discordStop(err error) bool {
	return discord.IsStatus(err, http.StatusTooManyRequests) || discord.IsStatus(err, http.StatusForbidden) || discord.IsStatus(err, http.StatusUnauthorized)
}

// discordSyncMember brings one connected account up to date in one
// server and writes down what it now holds there. The row is kept even
// when nothing is held, as the note of when the account was last
// looked for in that server.
func (app *Application) discordSyncMember(ctx context.Context, guild db.DiscordGuild, discordID string, wanted []string, had string, now time.Time) (changed bool, err error) {
	applied, member, err := app.discordApplyRoles(ctx, guild, discordID, wanted, splitRoles(had))
	if err != nil {
		return false, err
	}
	if err := app.queries.UpsertDiscordRoleGrant(ctx, db.UpsertDiscordRoleGrantParams{
		DiscordID: discordID, GuildID: guild.GuildID, Roles: applied, IsMember: member, CheckedAt: now,
	}); err != nil {
		logging.Errorf("discord: record roles given to %s in %s: %v", discordID, guild.GuildID, err)
	}
	return applied != had, nil
}

// discordSyncRoles brings roles up to date across every server. First
// it takes back what is written down for Discord accounts no
// EveSynapse account is connected to any more. Then, server by server,
// the connected accounts whose wanted roles differ from what was last
// given, and those not checked for a while. Discord is asked only
// about those, and about at most discordRolesPerPass members a pass.
func (app *Application) discordSyncRoles(ctx context.Context, now time.Time) (changed int) {
	if !app.discordHasBot() {
		return 0
	}
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil {
		logging.Errorf("discord: list servers: %v", err)
		return 0
	}
	byID := map[string]db.DiscordGuild{}
	for _, g := range guilds {
		byID[g.GuildID] = g
	}
	budget := discordRolesPerPass
	stopped := map[string]bool{} // servers that told the bot to stop for now

	orphans, err := app.queries.ListOrphanDiscordRoleGrants(ctx, discordRolesPerPass)
	if err != nil {
		logging.Errorf("discord: list roles to take back: %v", err)
		return 0
	}
	forget := func(grant db.DiscordRoleGrant) {
		if err := app.queries.DeleteDiscordRoleGrant(ctx, db.DeleteDiscordRoleGrantParams{DiscordID: grant.DiscordID, GuildID: grant.GuildID}); err != nil {
			logging.Errorf("discord: forget roles of %s in %s: %v", grant.DiscordID, grant.GuildID, err)
		}
	}
	for _, grant := range orphans {
		guild, registered := byID[grant.GuildID]
		if grant.Roles == "" || !registered {
			forget(grant) // nothing was held, or the bot no longer acts there
			continue
		}
		if ctx.Err() != nil || budget <= 0 {
			return changed
		}
		if stopped[guild.GuildID] {
			continue
		}
		budget--
		if _, _, err := app.discordApplyRoles(ctx, guild, grant.DiscordID, nil, splitRoles(grant.Roles)); err != nil {
			logging.Warnf("discord: take back roles of %s in %s: %v", grant.DiscordID, guild.Name, err)
			stopped[guild.GuildID] = discordStop(err)
			continue // kept written down: tried again next pass
		}
		forget(grant)
		logging.Infof("discord: took back the roles of %s in %s, which no account is connected to any more", grant.DiscordID, guild.Name)
		changed++
	}

	if len(guilds) == 0 {
		return changed
	}
	links, err := app.queries.ListDiscordLinks(ctx)
	if err != nil {
		logging.Errorf("discord: list links: %v", err)
		return changed
	}
	standings := map[int64]discordStanding{}
	for _, link := range links {
		st, err := app.discordStandingFor(ctx, link.UserID)
		if err != nil {
			logging.Errorf("discord: standing of user %d: %v", link.UserID, err)
			continue
		}
		standings[link.UserID] = st
	}
	for _, guild := range guilds {
		grants, err := app.queries.ListDiscordRoleGrantsForGuild(ctx, guild.GuildID)
		if err != nil {
			logging.Errorf("discord: roles given in %s: %v", guild.Name, err)
			continue
		}
		given := map[string]db.DiscordRoleGrant{}
		for _, grant := range grants {
			given[grant.DiscordID] = grant
		}
		for _, link := range links {
			st, known := standings[link.UserID]
			if !known {
				continue
			}
			wanted := discordWanted(guild, st)
			grant, looked := given[link.DiscordID]
			target := strings.Join(wanted, ",")
			switch {
			case !looked && target == "":
				continue // nothing given, nothing owed
			case looked && now.Sub(grant.CheckedAt) < discordRolesRecheck && (target == grant.Roles || !grant.IsMember):
				// As it should be and checked recently; or not in the
				// server when last looked for, which is not asked again
				// every minute (the "check my roles now" button is for
				// someone who has just joined).
				continue
			}
			if ctx.Err() != nil || budget <= 0 {
				return changed
			}
			if stopped[guild.GuildID] {
				break
			}
			budget--
			did, err := app.discordSyncMember(ctx, guild, link.DiscordID, wanted, grant.Roles, now)
			if err != nil {
				logging.Warnf("discord: roles of user %d in %s: %v", link.UserID, guild.Name, err)
				stopped[guild.GuildID] = discordStop(err)
				continue
			}
			if did {
				changed++
			}
		}
	}
	return changed
}

// discordSyncAccount brings one account's roles up to date in every
// server at once, whatever was checked when: for the "refresh my
// roles" button, pressed by someone who has just joined a server.
func (app *Application) discordSyncAccount(ctx context.Context, link db.DiscordLink, now time.Time) (servers int, err error) {
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil {
		return 0, err
	}
	st, err := app.discordStandingFor(ctx, link.UserID)
	if err != nil {
		return 0, err
	}
	for _, guild := range guilds {
		wanted := discordWanted(guild, st)
		had := ""
		if grant, gerr := app.queries.GetDiscordRoleGrant(ctx, db.GetDiscordRoleGrantParams{DiscordID: link.DiscordID, GuildID: guild.GuildID}); gerr == nil {
			had = grant.Roles
		}
		if len(wanted) == 0 && had == "" {
			continue
		}
		if _, err := app.discordSyncMember(ctx, guild, link.DiscordID, wanted, had, now); err != nil {
			logging.Warnf("discord: refresh roles of user %d in %s: %v", link.UserID, guild.Name, err)
			continue
		}
		if len(wanted) > 0 {
			servers++
		}
	}
	return servers, nil
}

// discordDropRoles takes back, at once, everything the bot gave an
// account's Discord member, in every server: for when the account
// disconnects Discord. Where Discord cannot be reached now, what was
// given stays written down and the worker takes it back on a later
// pass.
func (app *Application) discordDropRoles(ctx context.Context, userID int64) {
	if !app.discordHasBot() {
		return
	}
	link, err := app.queries.GetDiscordLink(ctx, userID)
	if err != nil {
		return
	}
	grants, err := app.queries.ListDiscordRoleGrantsForDiscordID(ctx, link.DiscordID)
	if err != nil {
		logging.Errorf("discord: roles given to user %d: %v", userID, err)
		return
	}
	for _, grant := range grants {
		guild, err := app.queries.GetDiscordGuild(ctx, grant.GuildID)
		if err == nil && grant.Roles != "" {
			if _, _, err := app.discordApplyRoles(ctx, guild, link.DiscordID, nil, splitRoles(grant.Roles)); err != nil {
				logging.Warnf("discord: take back roles of user %d in %s: %v", userID, guild.Name, err)
				continue
			}
		}
		if err := app.queries.DeleteDiscordRoleGrant(ctx, db.DeleteDiscordRoleGrantParams{DiscordID: grant.DiscordID, GuildID: grant.GuildID}); err != nil {
			logging.Errorf("discord: forget roles of user %d: %v", userID, err)
		}
	}
}
