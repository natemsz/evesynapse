package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Discord servers (/discord/servers). The bot is one bot, added to
// however many servers: a corporation's, an alliance's. Each server's
// settings are kept here, not in the install's environment, and are
// set by the people whose corporation or alliance the server belongs
// to.
//
// Who may do what:
//
//   - A server belongs to a corporation or to an alliance (its owner).
//     An account may manage a corporation's servers when one of its
//     characters, with a working link to EVE, holds the Director role
//     there (a CEO does). It may manage an alliance's servers when
//     that character is a Director of the alliance's executor
//     corporation.
//   - Adding the bot to a server is done on Discord's own page, which
//     only lets someone who may manage that server do it. Which server
//     it was is taken from Discord's answer, never from the browser.
//   - A corporation's ops go to its own servers. They go to its
//     alliance's server only when a Director of that corporation has
//     said so.
//
// Which channels a role can see is set in Discord, on the channel
// itself, as for any role. The bot gives the roles; it does not have
// permission to change channels.
// ---------------------------------------------------------------------------

const (
	ownerCorporation = "corporation"
	ownerAlliance    = "alliance"

	// sessionDiscordInstall holds, while someone is on Discord's page
	// adding the bot, whose server it is to be ("corporation:98000001").
	sessionDiscordInstall = "discord_install_owner"

	discordServersPath = "/discord/servers"
)

// discordOwner is a corporation or alliance that servers belong to.
type discordOwner struct {
	Kind string
	ID   int64
}

func (o discordOwner) key() string { return o.Kind + ":" + strconv.FormatInt(o.ID, 10) }

func parseDiscordOwner(raw string) (discordOwner, bool) {
	kind, id, found := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(id, 10, 64)
	if !found || err != nil || n <= 0 || (kind != ownerCorporation && kind != ownerAlliance) {
		return discordOwner{}, false
	}
	return discordOwner{Kind: kind, ID: n}, true
}

// corpAlliance reports the alliance a corporation is in and that
// alliance's executor corporation, from the stored corporation record.
// Both are 0 for a corporation in no alliance, or one whose record has
// not been fetched yet; the record is asked for then, so that a later
// look finds it.
func (app *Application) corpAlliance(ctx context.Context, corpID int64) (allianceID, executorCorpID int64) {
	rec, err := app.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		app.notePageWantFromContext(ctx, pageWantCorporation, corpID)
		return 0, 0
	}
	var payload corporationRecordPayload
	if json.Unmarshal([]byte(rec.Payload), &payload) != nil || payload.Corp.AllianceID == 0 {
		return 0, 0
	}
	return payload.Corp.AllianceID, payload.Alliance.ExecutorCorporationID
}

// discordManageable lists the corporations and alliances whose servers
// an account may manage, in the order of its characters.
func (app *Application) discordManageable(ctx context.Context, userID int64) []discordOwner {
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("discord: characters of user %d: %v", userID, err)
		return nil
	}
	working := map[int64]bool{}
	if characters, cerr := app.queries.ListCharactersByUser(ctx, userID); cerr == nil {
		for _, ch := range characters {
			working[ch.CharacterID] = ch.LinkState == "ok"
		}
	}
	seen := map[string]bool{}
	var out []discordOwner
	add := func(o discordOwner) {
		if !seen[o.key()] {
			seen[o.key()] = true
			out = append(out, o)
		}
	}
	for _, row := range rows {
		if !working[row.CharacterID] || row.CorporationID == 0 {
			continue
		}
		var roles esi.CharacterRoles
		if !app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) || !holdsDirector(roles.Roles) {
			continue
		}
		add(discordOwner{ownerCorporation, row.CorporationID})
		if alliance, executor := app.corpAlliance(ctx, row.CorporationID); alliance != 0 && executor == row.CorporationID {
			add(discordOwner{ownerAlliance, alliance})
		}
	}
	return out
}

// holdsDirector reports whether a character's corporation roles
// include Director. This is the in-game role itself, not the
// OPS_MANAGER_ROLES setting: who runs a Discord server is not a
// thing to widen by configuration.
func holdsDirector(roles []string) bool {
	for _, role := range roles {
		if strings.EqualFold(role, "Director") {
			return true
		}
	}
	return false
}

func discordManages(owners []discordOwner, o discordOwner) bool {
	for _, owner := range owners {
		if owner == o {
			return true
		}
	}
	return false
}

// ownerName is how an owner is shown.
func (app *Application) ownerName(ctx context.Context, o discordOwner) string {
	if o.Kind == ownerAlliance {
		if name, ok := app.resolvedAllianceName(ctx, o.ID); ok && name != "" {
			return name
		}
		return fmt.Sprintf("Alliance #%d", o.ID)
	}
	return app.corpDisplayName(ctx, o.ID)
}

// discordServersView is the /discord/servers page.
type discordServersView struct {
	CanInstall   bool
	OwnerOptions []corpOption
	Owner        *discordOwnerView // the corporation or alliance the page is about
	Shares       []discordShareView
}

type discordOwnerView struct {
	Key     string // "corporation:98000001"
	Kind    string
	Name    string
	Servers []discordServerView
}

type discordServerView struct {
	GuildID  string
	Name     string
	Rules    []discordRuleView  // who gets which role (discord_rules.go)
	Who      []discordWhoChoice // what a new rule can be about
	Roles    []discordChoice    // the roles a new rule can give
	Channels []discordChoice
	// AutoJoin: members of the owner who have connected Discord are
	// added to the server without an invite (discord_join.go).
	AutoJoin bool
	// Removals (discord_kick.go): whether members owed no role are
	// removed, and the roles whose holders never are.
	Kick   bool
	Exempt []discordChoice
	// Channel access (discord_channels.go): the channels the bot
	// keeps private, and what a new one can be picked from.
	Managed     []discordChannelView
	AllChannels []discordChoice
	// Unreachable: Discord would not list the server's roles and
	// channels (the bot was removed from it, or Discord is down).
	Unreachable bool
	MemberWord  string // "corporation" or "alliance"
}

type discordChoice struct {
	ID, Name string
	Selected bool
}

// discordShareView is one corporation the account directs that is in
// an alliance: whether its ops go to the alliance's server.
type discordShareView struct {
	CorporationID int64
	Name          string
	Alliance      string
	On            bool
}

func discordRoleChoices(roles []discord.Role, current string) []discordChoice {
	var out []discordChoice
	for _, role := range roles {
		if role.Managed {
			continue // a bot's or integration's own role: cannot be given
		}
		out = append(out, discordChoice{ID: role.ID, Name: role.Name, Selected: role.ID == current})
	}
	return out
}

func discordChannelChoices(channels []discord.Channel, current string) []discordChoice {
	out := []discordChoice{{ID: "", Name: "Do not post ops", Selected: current == ""}}
	for _, ch := range channels {
		out = append(out, discordChoice{ID: ch.ID, Name: "#" + ch.Name, Selected: ch.ID == current})
	}
	return out
}

func (app *Application) handleDiscordServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	view := &discordServersView{CanInstall: app.discord != nil && app.discord.Config().CanInstall()}
	owners := app.discordManageable(ctx, userID)
	guilds, err := app.queries.ListDiscordGuilds(ctx)
	if err != nil {
		logging.Errorf("discord: list servers: %v", err)
		data.Error = "Could not load the servers; check the server log."
	}
	var shown []discordOwner
	if owner, options, ok := app.pickOwner(ctx, r, owners); ok {
		shown, view.OwnerOptions = []discordOwner{owner}, options
	}
	for _, owner := range shown {
		ov := discordOwnerView{Key: owner.key(), Kind: owner.Kind, Name: app.ownerName(ctx, owner)}
		for _, guild := range guilds {
			if guild.OwnerKind != owner.Kind || guild.OwnerID != owner.ID {
				continue
			}
			sv := discordServerView{GuildID: guild.GuildID, Name: guild.Name, MemberWord: owner.Kind, AutoJoin: guild.AutoJoin}
			roles, rerr := app.discord.GuildRoles(ctx, guild.GuildID)
			channels, cerr := app.discord.GuildTextChannels(ctx, guild.GuildID)
			if rerr != nil || cerr != nil {
				sv.Unreachable = true
			} else {
				sv.Rules = app.discordRuleViews(ctx, guild, roles)
				sv.Who = app.discordWhoChoices(ctx, owner)
				sv.Roles = discordRoleChoices(roles, "")
				if all, aerr := app.discord.GuildChannels(ctx, guild.GuildID); aerr == nil {
					sv.Managed = app.discordChannelViews(ctx, guild.GuildID, all, roles)
					for _, ch := range all {
						sv.AllChannels = append(sv.AllChannels, discordChoice{ID: ch.ID, Name: channelLabel(ch)})
					}
				}
				sv.Kick = guild.KickEnabled
				exempt := roleSet{}
				exempt.add(splitRoles(guild.KickExemptRoles)...)
				for _, role := range roles {
					if role.Managed {
						continue // a bot's own role: bots are never removed anyway
					}
					sv.Exempt = append(sv.Exempt, discordChoice{ID: role.ID, Name: role.Name, Selected: exempt[role.ID]})
				}
				sv.Channels = discordChannelChoices(channels, guild.OpsChannel)
			}
			ov.Servers = append(ov.Servers, sv)
		}
		view.Owner = &ov
		if owner.Kind == ownerCorporation {
			if alliance, _ := app.corpAlliance(ctx, owner.ID); alliance != 0 {
				_, serr := app.queries.GetDiscordOpsShare(ctx, owner.ID)
				view.Shares = append(view.Shares, discordShareView{
					CorporationID: owner.ID, Name: ov.Name,
					Alliance: app.ownerName(ctx, discordOwner{ownerAlliance, alliance}), On: serr == nil,
				})
			}
		}
	}
	data.DiscordServers = view
	app.render(ctx, w, http.StatusOK, "discord_servers.html", data)
}

// handleDiscordServerAdd sends a director to Discord to add the bot to
// a server of theirs, remembering whose server it is to be.
func (app *Application) handleDiscordServerAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	if app.discord == nil || !app.discord.Config().CanInstall() {
		back("The Discord bot is not set up on this site.")
		return
	}
	_ = r.ParseForm()
	owner, ok := parseDiscordOwner(r.Form.Get("owner"))
	if !ok || !discordManages(app.discordManageable(ctx, userID), owner) {
		back("Only a director can add the bot for that corporation or alliance.")
		return
	}
	state, err := newOAuthState()
	if err != nil {
		logging.Errorf("discord: generate state: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	app.sessions.Put(ctx, sessionDiscordState, state)
	app.sessions.Put(ctx, sessionDiscordInstall, owner.key())
	http.Redirect(w, r, app.discord.InstallURL(state), http.StatusFound)
}

// discordFinishInstall is the return from Discord after the bot was
// added to a server: the server is recorded as the owner's. The
// account's right to manage that owner is checked again here.
func (app *Application) discordFinishInstall(w http.ResponseWriter, r *http.Request, userID int64, ownerKey, code string) {
	ctx := r.Context()
	back := app.flashBack(w, r, ownerAddress(discordServersPath, ownerKey))
	owner, ok := parseDiscordOwner(ownerKey)
	if !ok || !discordManages(app.discordManageable(ctx, userID), owner) {
		back("Only a director can add the bot for that corporation or alliance.")
		return
	}
	_, guild, err := app.discord.IdentifyInstall(ctx, code)
	if err != nil {
		logging.Errorf("discord install: user %d: %v", userID, err)
		back("Discord could not confirm that the bot was added. Please try again.")
		return
	}
	if guild.ID == "" {
		back("The bot was not added to a server.")
		return
	}
	if err := app.queries.UpsertDiscordGuild(ctx, db.UpsertDiscordGuildParams{
		GuildID: guild.ID, Name: clip(guild.Name, 100), OwnerKind: owner.Kind, OwnerID: owner.ID,
		AddedBy: userID, AddedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("discord install: store server %s: %v", guild.ID, err)
		back("The server could not be saved; check the server log.")
		return
	}
	logging.Infof("discord: user %d added the bot to server %s (%s) for %s", userID, guild.ID, guild.Name, owner.key())
	back("The bot was added to " + guild.Name + ". Choose its roles and channel below.")
}

// discordBack answers a form on the servers page by sending the
// account back to the page of the owner it was about: the owner of the
// server in the address, else the one the form names.
func (app *Application) discordBack(w http.ResponseWriter, r *http.Request) func(string) {
	_ = r.ParseForm()
	key := r.Form.Get("owner")
	if guild, err := app.queries.GetDiscordGuild(r.Context(), chi.URLParam(r, "guildID")); err == nil {
		key = discordOwner{guild.OwnerKind, guild.OwnerID}.key()
	} else if corp := r.Form.Get("corporation"); key == "" && corp != "" {
		key = ownerCorporation + ":" + corp
	}
	return app.flashBack(w, r, ownerAddress(discordServersPath, key))
}

// discordGuildFor loads the server named in the address, for an
// account that may manage it.
func (app *Application) discordGuildFor(r *http.Request, userID int64) (db.DiscordGuild, bool) {
	id := chi.URLParam(r, "guildID")
	if !discord.ValidID(id) {
		return db.DiscordGuild{}, false
	}
	guild, err := app.queries.GetDiscordGuild(r.Context(), id)
	if err != nil {
		return db.DiscordGuild{}, false
	}
	if !discordManages(app.discordManageable(r.Context(), userID), discordOwner{guild.OwnerKind, guild.OwnerID}) {
		return db.DiscordGuild{}, false
	}
	return guild, true
}

// handleDiscordServerSave stores where a server's new ops are posted.
// The channel has to be one Discord lists for that very server. (Who
// gets which role is the server's rules: discord_rules.go.)
func (app *Application) handleDiscordServerSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	if !ok || r.ParseForm() != nil {
		back("That server is not yours to change.")
		return
	}
	channels, cerr := app.discord.GuildTextChannels(ctx, guild.GuildID)
	if cerr != nil {
		logging.Warnf("discord: read server %s: %v", guild.GuildID, cerr)
		back("Discord would not list that server's channels. Is the bot still in it?")
		return
	}
	postable := map[string]bool{"": true}
	for _, ch := range channels {
		postable[ch.ID] = true
	}
	channel := r.Form.Get("ops_channel")
	if !postable[channel] {
		back("That is not a channel of that server a message can be posted in.")
		return
	}
	if err := app.queries.SetDiscordGuildAutoJoin(ctx, db.SetDiscordGuildAutoJoinParams{GuildID: guild.GuildID, AutoJoin: r.Form.Get("auto_join") == "1"}); err != nil {
		logging.Errorf("discord: save server %s: %v", guild.GuildID, err)
		back("The settings could not be saved; check the server log.")
		return
	}
	if err := app.queries.SetDiscordGuildOpsChannel(ctx, db.SetDiscordGuildOpsChannelParams{GuildID: guild.GuildID, OpsChannel: channel}); err != nil {
		logging.Errorf("discord: save server %s: %v", guild.GuildID, err)
		back("The settings could not be saved; check the server log.")
		return
	}
	logging.Infof("discord: user %d set the ops channel of server %s to %q", userID, guild.GuildID, channel)
	back("Settings saved for " + guild.Name + ".")
}

// handleDiscordServerForget stops the bot acting in a server. The
// roles it gave there are taken back first, by the worker; until that
// is done the server is kept, with its rules cleared, so the bot
// still knows what to take back. Then the record is removed and the
// bot leaves the server.
func (app *Application) handleDiscordServerForget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	guild, ok := app.discordGuildFor(r, userID)
	if !ok {
		back("That server is not yours to change.")
		return
	}
	err := app.queries.DeleteDiscordRoleRulesForGuild(ctx, guild.GuildID)
	if err == nil {
		err = app.queries.SetDiscordGuildKick(ctx, db.SetDiscordGuildKickParams{GuildID: guild.GuildID})
	}
	if err == nil {
		err = app.queries.SetDiscordGuildOpsChannel(ctx, db.SetDiscordGuildOpsChannelParams{GuildID: guild.GuildID})
	}
	if err != nil {
		logging.Errorf("discord: clear server %s: %v", guild.GuildID, err)
		back("The server could not be changed; check the server log.")
		return
	}
	grants, err := app.queries.ListDiscordRoleGrantsForGuild(ctx, guild.GuildID)
	held := 0
	for _, grant := range grants {
		if grant.Roles != "" {
			held++
		}
	}
	if err != nil || held > 0 {
		back(fmt.Sprintf("%s is switched off. The bot is taking back the roles it gave there (%d member(s) to go); press Remove again in a few minutes to finish.", guild.Name, held))
		return
	}
	if err := app.queries.DeleteDiscordGuild(ctx, guild.GuildID); err != nil {
		logging.Errorf("discord: remove server %s: %v", guild.GuildID, err)
		back("The server could not be removed; check the server log.")
		return
	}
	if err := app.discord.LeaveGuild(ctx, guild.GuildID); err != nil && !discord.IsStatus(err, http.StatusNotFound) {
		logging.Warnf("discord: leave server %s: %v", guild.GuildID, err)
		back(guild.Name + " was removed here, but the bot could not leave it. Remove the bot in the server's own settings.")
		return
	}
	logging.Infof("discord: user %d removed server %s (%s)", userID, guild.GuildID, guild.Name)
	back(guild.Name + " was removed and the bot has left it.")
}

// handleDiscordOpsShare stores whether a corporation's ops are also
// posted in its alliance's server. Only that corporation's directors
// decide it.
func (app *Application) handleDiscordOpsShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.discordBack(w, r)
	_ = r.ParseForm()
	corp, _ := strconv.ParseInt(r.Form.Get("corporation"), 10, 64)
	if corp <= 0 || !discordManages(app.discordManageable(ctx, userID), discordOwner{ownerCorporation, corp}) {
		back("Only a director of that corporation can decide that.")
		return
	}
	var err error
	if r.Form.Get("share") == "1" {
		err = app.queries.SetDiscordOpsShare(ctx, db.SetDiscordOpsShareParams{CorporationID: corp, SetBy: userID, SetAt: time.Now().UTC()})
	} else {
		err = app.queries.DeleteDiscordOpsShare(ctx, corp)
	}
	if err != nil {
		logging.Errorf("discord: ops share of corporation %d: %v", corp, err)
		back("That could not be saved; check the server log.")
		return
	}
	back("Saved.")
}

// discordOpsChannels lists where a corporation's new op is posted: its
// own servers' channels, and its alliance's when the corporation has
// agreed to that.
func (app *Application) discordOpsChannels(ctx context.Context, guilds []db.DiscordGuild, corpID int64) []db.DiscordGuild {
	alliance, _ := app.corpAlliance(ctx, corpID)
	shared := false
	if alliance != 0 {
		_, err := app.queries.GetDiscordOpsShare(ctx, corpID)
		shared = err == nil
	}
	var out []db.DiscordGuild
	for _, guild := range guilds {
		if guild.OpsChannel == "" {
			continue
		}
		if (guild.OwnerKind == ownerCorporation && guild.OwnerID == corpID) ||
			(guild.OwnerKind == ownerAlliance && shared && guild.OwnerID == alliance) {
			out = append(out, guild)
		}
	}
	return out
}
