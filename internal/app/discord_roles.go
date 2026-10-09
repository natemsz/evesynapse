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
// Discord roles from what EveSynapse knows. The bot gives a connected
// account the roles its characters earn and takes them away when they
// no longer do:
//
//	DISCORD_ROLE_LINKED   every connected account that has at least
//	                      one character whose link to EVE works
//	DISCORD_ROLE_CORPS    one role per corporation, for accounts with
//	                      such a character in it
//
// What it rests on, and so how far to trust it: a corporation role
// says that an EveSynapse account holding that Discord account has a
// character whose link to EVE is in good standing and which the last
// sync saw in that corporation. It follows a character leaving within
// a sync or two, not at that instant.
//
// The bot only touches the roles named in those two settings. Every
// other role a member has is left exactly as it is, whoever gave it.
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
	// discordRolesPerPass bounds how many accounts one worker pass
	// brings up to date, and discordRolesRecheck is how often an
	// account whose roles look right is checked against Discord
	// anyway (a role removed by hand is given back then).
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

// discordManagedRoles lists every role the bot may give or take.
func (app *Application) discordManagedRoles() []string {
	set := roleSet{}
	set.add(app.cfg.discordRoleLinked)
	for _, role := range app.cfg.discordCorpRoles {
		set.add(role)
	}
	return set.list()
}

// discordWantedRoles is the set of managed roles an account should
// hold now, sorted. An account with no character whose link to EVE
// works earns none: being connected is not enough on its own.
func (app *Application) discordWantedRoles(ctx context.Context, userID int64) ([]string, error) {
	working, err := app.queries.CountLinkedCharactersByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	set := roleSet{}
	if working == 0 {
		return set.list(), nil
	}
	set.add(app.cfg.discordRoleLinked)
	corps, err := app.queries.ListLinkedCorporationsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, corp := range corps {
		set.add(app.cfg.discordCorpRoles[corp])
	}
	return set.list(), nil
}

// discordApplyRoles makes one member's roles match wanted, among the
// managed roles and any in also (roles given earlier that may since
// have left the settings). It returns the set now held: "" for a
// member who is not in the server, where there is nothing to give or
// take.
func (app *Application) discordApplyRoles(ctx context.Context, discordID string, wanted, also []string) (applied string, err error) {
	held, err := app.discord.MemberRoles(ctx, discordID)
	if err != nil {
		if discord.IsStatus(err, http.StatusNotFound) {
			return "", nil
		}
		return "", err
	}
	has := roleSet{}
	has.add(held...)
	want := roleSet{}
	want.add(wanted...)
	touch := roleSet{}
	touch.add(app.discordManagedRoles()...)
	touch.add(also...)
	for _, role := range touch.list() {
		if want[role] == has[role] {
			continue
		}
		if err := app.discord.SetRole(ctx, discordID, role, want[role]); err != nil {
			return "", err
		}
	}
	return strings.Join(wanted, ","), nil
}

// discordRecordGrant writes down what a Discord account now holds
// from the bot, or that it holds nothing.
func (app *Application) discordRecordGrant(ctx context.Context, discordID, applied string, now time.Time) {
	guild := app.discord.Config().GuildID
	var err error
	if applied == "" {
		err = app.queries.DeleteDiscordRoleGrant(ctx, db.DeleteDiscordRoleGrantParams{DiscordID: discordID, GuildID: guild})
	} else {
		err = app.queries.UpsertDiscordRoleGrant(ctx, db.UpsertDiscordRoleGrantParams{
			DiscordID: discordID, GuildID: guild, Roles: applied, UpdatedAt: now,
		})
	}
	if err != nil {
		logging.Errorf("discord: record roles given to %s: %v", discordID, err)
	}
}

// discordStop reports whether err is one that every other account
// would meet too: asked to slow down, or the bot not being allowed.
func discordStop(err error) bool {
	return discord.IsStatus(err, http.StatusTooManyRequests) || discord.IsStatus(err, http.StatusForbidden) || discord.IsStatus(err, http.StatusUnauthorized)
}

// discordSyncRoles brings roles up to date. First it takes back what
// is written down for Discord accounts no EveSynapse account is
// connected to any more. Then, for connected accounts, those whose
// wanted set has changed since it was last applied, and those not
// checked for a while. It talks to Discord only for those.
func (app *Application) discordSyncRoles(ctx context.Context, now time.Time) (changed int) {
	if !app.discordHasBot() {
		return 0
	}
	guild := app.discord.Config().GuildID
	orphans, err := app.queries.ListOrphanDiscordRoleGrants(ctx, discordRolesPerPass)
	if err != nil {
		logging.Errorf("discord: list roles to take back: %v", err)
		return 0
	}
	for _, grant := range orphans {
		if ctx.Err() != nil {
			return changed
		}
		if grant.GuildID != guild {
			continue // given in a server this install no longer acts in
		}
		if _, err := app.discordApplyRoles(ctx, grant.DiscordID, nil, splitRoles(grant.Roles)); err != nil {
			logging.Warnf("discord: take back roles of %s: %v", grant.DiscordID, err)
			if discordStop(err) {
				return changed
			}
			continue // kept written down: tried again next pass
		}
		app.discordRecordGrant(ctx, grant.DiscordID, "", now)
		logging.Infof("discord: took back the roles of %s, which no account is connected to any more", grant.DiscordID)
		changed++
	}

	if len(app.discordManagedRoles()) == 0 {
		return changed
	}
	links, err := app.queries.ListDiscordLinks(ctx)
	if err != nil {
		logging.Errorf("discord: list links: %v", err)
		return changed
	}
	done := 0
	for _, link := range links {
		if ctx.Err() != nil || done >= discordRolesPerPass {
			return changed
		}
		wanted, err := app.discordWantedRoles(ctx, link.UserID)
		if err != nil {
			logging.Errorf("discord: roles wanted for user %d: %v", link.UserID, err)
			continue
		}
		target := strings.Join(wanted, ",")
		fresh := link.RolesSyncedAt.Valid && now.Sub(link.RolesSyncedAt.Time) < discordRolesRecheck
		if fresh && target == link.RolesApplied {
			continue
		}
		done++
		applied, err := app.discordApplyRoles(ctx, link.DiscordID, wanted, splitRoles(link.RolesApplied))
		if err != nil {
			logging.Warnf("discord: roles of user %d: %v", link.UserID, err)
			if discordStop(err) {
				return changed
			}
			continue
		}
		if applied != link.RolesApplied {
			changed++
		}
		// Written down against the Discord account first: if the
		// EveSynapse account vanished this instant, the roles just
		// given would still be found and taken back.
		app.discordRecordGrant(ctx, link.DiscordID, applied, now)
		if err := app.queries.SetDiscordRolesApplied(ctx, db.SetDiscordRolesAppliedParams{
			UserID: link.UserID, RolesApplied: applied, RolesSyncedAt: timeSet(now),
		}); err != nil {
			logging.Errorf("discord: record roles of user %d: %v", link.UserID, err)
		}
	}
	return changed
}

// discordDropRoles takes back every managed role from an account's
// Discord member at once, for when the account disconnects Discord.
// If Discord cannot be reached now, what was given stays written down
// and the worker takes it back on its next pass.
func (app *Application) discordDropRoles(ctx context.Context, userID int64) {
	if !app.discordHasBot() {
		return
	}
	link, err := app.queries.GetDiscordLink(ctx, userID)
	if err != nil {
		return
	}
	if _, err := app.discordApplyRoles(ctx, link.DiscordID, nil, splitRoles(link.RolesApplied)); err != nil {
		logging.Warnf("discord: take back roles of user %d: %v", userID, err)
		return
	}
	app.discordRecordGrant(ctx, link.DiscordID, "", time.Now().UTC())
}
