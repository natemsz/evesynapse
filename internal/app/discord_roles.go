package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Discord roles from what EveSynapse knows. In each server the bot was
// added to (discord_servers.go), its directors write rules, as many as
// they have roles to give: "whoever is this gets that role". A member
// gets every role they qualify for, and loses each one when they stop
// qualifying for it. Who "this" can be:
//
//	linked    anyone there who has connected an EveSynapse account
//	          with a character whose link to EVE works
//	member    such a character is in the server's corporation, or in
//	          a corporation of the server's alliance
//	corp      such a character is in one named corporation
//	ceo       is the CEO of the corporation (or of one in the alliance)
//	eve_role  holds an in-game corporation role there (Director,
//	          Accountant, ...)
//	group     is in a group the directors keep (groups.go)
//
// What a role rests on, and so how far to trust it: an EveSynapse
// account holding that Discord account has a character whose link to
// EVE is in good standing and for which the last sync saw the thing
// the rule asks. It follows a change within a sync or two, not at that
// instant.
//
// In a server the bot only touches the roles named in that server's
// rules, and roles it gave there earlier. Every other role a member
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

// The kinds of rule (discord_role_rules.kind). Stored: never rename.
const (
	ruleLinked  = "linked"
	ruleMember  = "member"
	ruleCorp    = "corp"
	ruleCEO     = "ceo"
	ruleEVERole = "eve_role"
	ruleGroup   = "group"
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

// discordStanding is what an account's working characters amount to.
type discordStanding struct {
	working   bool
	corps     map[int64]bool            // corporations a character is in
	alliance  map[int64]int64           // corporation -> its alliance, where known
	ceoOf     map[int64]bool            // corporations a character is CEO of
	eveRoles  map[int64]map[string]bool // corporation -> in-game roles held there (lower case)
	groups    map[int64]bool            // groups a character is in, and still belongs to the owner of
	alliances map[int64]bool
}

// corpCEO reports a corporation's CEO from its stored record; 0 when
// the record has not been fetched.
func (app *Application) corpCEO(ctx context.Context, corpID int64) int64 {
	rec, err := app.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		return 0
	}
	var payload corporationRecordPayload
	if json.Unmarshal([]byte(rec.Payload), &payload) != nil {
		return 0
	}
	return payload.Corp.CEOID
}

// discordStandingFor works out an account's standing from stored data.
// A corporation whose alliance is not known yet counts for itself
// only.
func (app *Application) discordStandingFor(ctx context.Context, userID int64) (discordStanding, error) {
	st := discordStanding{
		corps: map[int64]bool{}, alliance: map[int64]int64{}, alliances: map[int64]bool{},
		ceoOf: map[int64]bool{}, eveRoles: map[int64]map[string]bool{}, groups: map[int64]bool{},
	}
	n, err := app.queries.CountLinkedCharactersByUser(ctx, userID)
	if err != nil || n == 0 {
		return st, err
	}
	st.working = true
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		return st, err
	}
	corpOf := map[int64]int64{} // character -> corporation
	for _, row := range rows {
		corp := row.CorporationID
		if corp == 0 {
			continue
		}
		corpOf[row.CharacterID] = corp
		if !st.corps[corp] {
			st.corps[corp] = true
			if alliance, _ := app.corpAlliance(ctx, corp); alliance != 0 {
				st.alliance[corp] = alliance
				st.alliances[alliance] = true
			}
		}
		if app.corpCEO(ctx, corp) == row.CharacterID {
			st.ceoOf[corp] = true
		}
		var roles esi.CharacterRoles
		if app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) {
			for _, role := range roles.Roles {
				if st.eveRoles[corp] == nil {
					st.eveRoles[corp] = map[string]bool{}
				}
				st.eveRoles[corp][strings.ToLower(role)] = true
			}
		}
	}
	memberships, err := app.queries.ListOrgGroupsByUser(ctx, userID)
	if err != nil {
		return st, err
	}
	for _, m := range memberships {
		// A character counts for a group only while it is in the
		// corporation or alliance the group belongs to.
		corp := corpOf[m.CharacterID]
		if corp != 0 && st.within(discordOwner{m.OwnerKind, m.OwnerID}, corp) {
			st.groups[m.GroupID] = true
		}
	}
	return st, nil
}

// within reports whether a corporation the account has a character in
// is the owner, or is in the owner alliance.
func (st discordStanding) within(owner discordOwner, corp int64) bool {
	if owner.Kind == ownerCorporation {
		return corp == owner.ID
	}
	return st.alliance[corp] == owner.ID
}

// meets reports whether the account satisfies one rule of a server
// belonging to owner.
func (st discordStanding) meets(owner discordOwner, rule db.DiscordRoleRule) bool {
	if !st.working {
		return false
	}
	switch rule.Kind {
	case ruleLinked:
		return true
	case ruleMember:
		for corp := range st.corps {
			if st.within(owner, corp) {
				return true
			}
		}
	case ruleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return corp != 0 && st.corps[corp] && st.within(owner, corp)
	case ruleCEO:
		for corp := range st.ceoOf {
			if st.within(owner, corp) {
				return true
			}
		}
	case ruleEVERole:
		want := strings.ToLower(rule.Ref)
		for corp, roles := range st.eveRoles {
			if roles[want] && st.within(owner, corp) {
				return true
			}
		}
	case ruleGroup:
		group, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return st.groups[group]
	}
	return false
}

// discordWanted is the roles an account of that standing should hold
// in a server now, sorted: every role a rule gives it.
func discordWanted(guild db.DiscordGuild, rules []db.DiscordRoleRule, st discordStanding) []string {
	owner := discordOwner{guild.OwnerKind, guild.OwnerID}
	set := roleSet{}
	for _, rule := range rules {
		if st.meets(owner, rule) {
			set.add(rule.RoleID)
		}
	}
	return set.list()
}

// ruleRoles lists the roles a server's rules name.
func ruleRoles(rules []db.DiscordRoleRule) []string {
	set := roleSet{}
	for _, rule := range rules {
		set.add(rule.RoleID)
	}
	return set.list()
}

// discordRulesByGuild loads every server's rules.
func (app *Application) discordRulesByGuild(ctx context.Context) (map[string][]db.DiscordRoleRule, error) {
	rules, err := app.queries.ListDiscordRoleRules(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]db.DiscordRoleRule{}
	for _, rule := range rules {
		out[rule.GuildID] = append(out[rule.GuildID], rule)
	}
	return out, nil
}

// discordApplyRoles makes one member's roles in a server match wanted,
// among managed (the roles the server's rules name) and any in also
// (roles given earlier that may since have left the rules). It reports
// the set now held, and whether the account is in the server at all.
func (app *Application) discordApplyRoles(ctx context.Context, guildID, discordID string, wanted, managed, also []string) (applied string, member bool, err error) {
	held, err := app.discord.MemberRoles(ctx, guildID, discordID)
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
	touch.add(managed...)
	touch.add(also...)
	for _, role := range touch.list() {
		if want[role] == has[role] {
			continue
		}
		if err := app.discord.SetRole(ctx, guildID, discordID, role, want[role]); err != nil {
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
// server and writes down what it now holds there. An account that
// belongs to the server's owner and is not in the server is added to
// it (discord_join.go). The row is kept even when nothing is held, as
// the note of when the account was last looked for in that server.
func (app *Application) discordSyncMember(ctx context.Context, guild db.DiscordGuild, rules []db.DiscordRoleRule, link db.DiscordLink, st discordStanding, wanted []string, had string, now time.Time) (changed bool, err error) {
	discordID := link.DiscordID
	applied, member, err := app.discordApplyRoles(ctx, guild.GuildID, discordID, wanted, ruleRoles(rules), splitRoles(had))
	if err != nil {
		return false, err
	}
	if !member {
		joined, jerr := app.discordJoin(ctx, guild, link, st, wanted, now)
		switch {
		case jerr != nil:
			// Noted, and looked at again at the periodic check: a
			// server that will not take members is not asked every pass.
			logging.Warnf("discord: add user %d to %s: %v", link.UserID, guild.Name, jerr)
		case joined:
			applied, member = strings.Join(wanted, ","), true
		}
	}
	if err := app.queries.UpsertDiscordRoleGrant(ctx, db.UpsertDiscordRoleGrantParams{
		DiscordID: discordID, GuildID: guild.GuildID, Roles: applied, Wanted: strings.Join(wanted, ","), IsMember: member, CheckedAt: now,
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
	rulesBy, err := app.discordRulesByGuild(ctx)
	if err != nil {
		logging.Errorf("discord: list role rules: %v", err)
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
		if _, _, err := app.discordApplyRoles(ctx, guild.GuildID, grant.DiscordID, nil, ruleRoles(rulesBy[guild.GuildID]), splitRoles(grant.Roles)); err != nil {
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
		rules := rulesBy[guild.GuildID]
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
			wanted := discordWanted(guild, rules, st)
			grant, looked := given[link.DiscordID]
			target := strings.Join(wanted, ",")
			switch {
			case !looked && target == "":
				continue // nothing given, nothing owed
			case looked && now.Sub(grant.CheckedAt) < discordRolesRecheck && (target == grant.Roles || (!grant.IsMember && target == grant.Wanted)):
				// As it should be and checked recently; or not in the
				// server when last looked for and owed the same as then,
				// which is not asked again every minute (the "check my
				// roles now" button is for someone who has just joined).
				continue
			}
			if ctx.Err() != nil || budget <= 0 {
				return changed
			}
			if stopped[guild.GuildID] {
				break
			}
			budget--
			did, err := app.discordSyncMember(ctx, guild, rules, link, st, wanted, grant.Roles, now)
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
// server at once, whatever was checked when: for the "check my roles
// now" button, pressed by someone who has just joined a server.
func (app *Application) discordSyncAccount(ctx context.Context, link db.DiscordLink, now time.Time) (servers int, err error) {
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil {
		return 0, err
	}
	rulesBy, err := app.discordRulesByGuild(ctx)
	if err != nil {
		return 0, err
	}
	st, err := app.discordStandingFor(ctx, link.UserID)
	if err != nil {
		return 0, err
	}
	for _, guild := range guilds {
		rules := rulesBy[guild.GuildID]
		wanted := discordWanted(guild, rules, st)
		had := ""
		if grant, gerr := app.queries.GetDiscordRoleGrant(ctx, db.GetDiscordRoleGrantParams{DiscordID: link.DiscordID, GuildID: guild.GuildID}); gerr == nil {
			had = grant.Roles
		}
		if len(wanted) == 0 && had == "" {
			continue
		}
		if _, err := app.discordSyncMember(ctx, guild, rules, link, st, wanted, had, now); err != nil {
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
		if grant.Roles != "" {
			if _, _, err := app.discordApplyRoles(ctx, grant.GuildID, link.DiscordID, nil, nil, splitRoles(grant.Roles)); err != nil {
				logging.Warnf("discord: take back roles of user %d in %s: %v", userID, grant.GuildID, err)
				continue
			}
		}
		if err := app.queries.DeleteDiscordRoleGrant(ctx, db.DeleteDiscordRoleGrantParams{DiscordID: grant.DiscordID, GuildID: grant.GuildID}); err != nil {
			logging.Errorf("discord: forget roles of user %d: %v", userID, err)
		}
	}
}
