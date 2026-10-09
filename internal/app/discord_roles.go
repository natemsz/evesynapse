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
//	DISCORD_ROLE_LINKED   every account that has connected Discord
//	DISCORD_ROLE_CORPS    one role per corporation, for accounts with
//	                      a character in it
//
// What it rests on, and so how far to trust it: a corporation role
// says that an EveSynapse account holding that Discord account has a
// character whose link to EVE is in good standing and which the last
// sync saw in that corporation. It follows a character leaving within
// a sync or two, not at that instant.
//
// The bot only touches the roles named in those two settings. Every
// other role a member has is left exactly as it is, whoever gave it.
// ---------------------------------------------------------------------------

const (
	// discordRolesPerPass bounds how many accounts one worker pass
	// brings up to date, and discordRolesRecheck is how often an
	// account whose roles look right is checked against Discord
	// anyway (a role removed by hand is given back then).
	discordRolesPerPass = 20
	discordRolesRecheck = 6 * time.Hour
)

// discordManagedRoles lists every role the bot may give or take.
func (app *Application) discordManagedRoles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(role string) {
		if role != "" && !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	add(app.cfg.discordRoleLinked)
	for _, role := range app.cfg.discordCorpRoles {
		add(role)
	}
	sort.Strings(out)
	return out
}

// discordWantedRoles is the set of managed roles an account should
// hold now, sorted.
func (app *Application) discordWantedRoles(ctx context.Context, userID int64) ([]string, error) {
	corps, err := app.queries.ListLinkedCorporationsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(role string) {
		if role != "" && !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	add(app.cfg.discordRoleLinked)
	for _, corp := range corps {
		add(app.cfg.discordCorpRoles[corp])
	}
	sort.Strings(out)
	return out, nil
}

// discordApplyRoles makes one member's managed roles match wanted. It
// reports the set now held ("" for a member who is not in the server)
// and whether Discord could be asked at all.
func (app *Application) discordApplyRoles(ctx context.Context, discordID string, wanted []string) (applied string, err error) {
	held, err := app.discord.MemberRoles(ctx, discordID)
	if err != nil {
		if discord.IsStatus(err, http.StatusNotFound) {
			return "", nil // not in the server: nothing to give, nothing held
		}
		return "", err
	}
	has := map[string]bool{}
	for _, role := range held {
		has[role] = true
	}
	want := map[string]bool{}
	for _, role := range wanted {
		want[role] = true
	}
	for _, role := range app.discordManagedRoles() {
		if want[role] == has[role] {
			continue
		}
		if err := app.discord.SetRole(ctx, discordID, role, want[role]); err != nil {
			return "", err
		}
	}
	return strings.Join(wanted, ","), nil
}

// discordSyncRoles brings connected accounts' roles up to date: those
// whose wanted set has changed since it was last applied, then those
// not checked for a while. It talks to Discord only for those.
func (app *Application) discordSyncRoles(ctx context.Context, now time.Time) (changed int) {
	if !app.discordHasBot() || len(app.discordManagedRoles()) == 0 {
		return 0
	}
	links, err := app.queries.ListDiscordLinks(ctx)
	if err != nil {
		logging.Errorf("discord: list links: %v", err)
		return 0
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
		applied, err := app.discordApplyRoles(ctx, link.DiscordID, wanted)
		if err != nil {
			logging.Warnf("discord: roles of user %d: %v", link.UserID, err)
			if discord.IsStatus(err, http.StatusTooManyRequests) || discord.IsStatus(err, http.StatusForbidden) || discord.IsStatus(err, http.StatusUnauthorized) {
				// Asked to slow down, or the bot is not allowed: the
				// same answer waits for every other account too.
				return changed
			}
			continue
		}
		if applied != link.RolesApplied {
			changed++
		}
		if err := app.queries.SetDiscordRolesApplied(ctx, db.SetDiscordRolesAppliedParams{
			UserID: link.UserID, RolesApplied: applied, RolesSyncedAt: timeSet(now),
		}); err != nil {
			logging.Errorf("discord: record roles of user %d: %v", link.UserID, err)
		}
	}
	return changed
}

// discordDropRoles takes back every managed role from an account's
// Discord member, for when the account disconnects Discord. Best
// effort: a failure is logged and the disconnect goes ahead.
func (app *Application) discordDropRoles(ctx context.Context, userID int64) {
	if !app.discordHasBot() || len(app.discordManagedRoles()) == 0 {
		return
	}
	link, err := app.queries.GetDiscordLink(ctx, userID)
	if err != nil {
		return
	}
	if _, err := app.discordApplyRoles(ctx, link.DiscordID, nil); err != nil {
		logging.Warnf("discord: take back roles of user %d: %v", userID, err)
	}
}
