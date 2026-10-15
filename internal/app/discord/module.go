// Package discord is the Discord module: connecting a Discord account,
// the bot's servers (roles given by rule, members joined and removed,
// channels kept private, ops announced) and notifications as direct
// messages. The Discord API client is internal/discord; the standing
// model the rules are asked against is foundation (internal/standing,
// loaded by core and read here through module.Deps).
//
// The module registers three worker jobs (announcing ops, syncing
// roles, the kick pass), one notification deliverer (direct messages)
// and a section of the notification settings page.
package discord

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	dc "evesynapse/internal/discord"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// Module is the Discord module's registration.
var Module = module.Module{
	ID:       "discord",
	Title:    "Discord",
	Requires: []string{},
	// Connecting Discord, DMs and the bot need no EVE scope. The
	// in-game-role rules and who may manage a corporation's servers
	// come from the corporation roles scope.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-characters.read_corporation_roles.v1", Unlocks: "Directors can manage their corporation's Discord servers, and rules can give roles by in-game corporation role."},
	},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/discord/servers", Label: "Discord Server", Pages: []string{"discord_servers.html"}, After: "/srp/"},
		},
	}},
	Pages: []module.Page{
		{Template: "discord_servers.html", Section: "corporation"},
	},
	WorkerJobs: []module.WorkerJob{
		{Name: "discord announce ops", After: module.AfterNotifications, Start: announceJob},
		{Name: "discord role sync", After: module.AfterNotifications, Start: roleSyncJob},
		{Name: "discord kick pass", After: module.AfterNotifications, Start: kickJob},
	},
	Deliverers: []module.Deliverer{deliver},
	NotifySettings: &module.SettingsSection{
		Channel: settingsChannel,
		Account: settingsAccount,
		Save:    settingsSave,
	},
	Routes: routes,
}

// The worker jobs run in the cycle as the Discord block always did:
// new ops to their channels, roles brought in line with what was just
// synced, then the removals. Each is its own pass under its own guard.

// AnnounceOps, SyncRoles and KickPass are the three passes, with the
// time they judge by (the jobs pass now).
func AnnounceOps(ctx context.Context, deps module.Deps, now time.Time) int {
	return newMod(deps).discordAnnounceOps(ctx, now)
}

func SyncRoles(ctx context.Context, deps module.Deps, now time.Time) int {
	return newMod(deps).discordSyncRoles(ctx, now)
}

func KickPass(ctx context.Context, deps module.Deps, now time.Time) int {
	return newMod(deps).discordKickPass(ctx, now)
}

func announceJob(ctx context.Context, deps module.Deps) {
	if n := AnnounceOps(ctx, deps, time.Now()); n > 0 {
		logging.Infof("worker: %d op(s) posted to Discord", n)
	}
}

func roleSyncJob(ctx context.Context, deps module.Deps) {
	if n := SyncRoles(ctx, deps, time.Now()); n > 0 {
		logging.Infof("worker: Discord roles changed for %d account(s)", n)
	}
}

func kickJob(ctx context.Context, deps module.Deps) {
	if n := KickPass(ctx, deps, time.Now()); n > 0 {
		logging.Infof("worker: %d member(s) removed from Discord servers", n)
	}
}

// routes mounts /discord: connecting an account, and what it is used
// for.
func routes(r chi.Router, deps module.Deps) {
	h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { f(newMod(deps), w, r) }
	}
	r.Route("/discord", func(r chi.Router) {
		r.Use(deps.RequireAuth())
		r.Get("/connect", h((*mod).handleDiscordConnect))
		r.Get("/callback", h((*mod).handleDiscordCallback))
		r.Post("/disconnect", h((*mod).handleDiscordDisconnect))
		r.Post("/settings", h((*mod).handleDiscordSettings))
		r.Post("/test", h((*mod).handleDiscordTest))
		r.Post("/roles/refresh", h((*mod).handleDiscordRolesRefresh))
		r.Get("/servers", h((*mod).handleDiscordServers))
		r.Post("/servers/add", h((*mod).handleDiscordServerAdd))
		r.Post("/servers/share", h((*mod).handleDiscordOpsShare))
		r.Post("/servers/{guildID}/save", h((*mod).handleDiscordServerSave))
		r.Post("/servers/{guildID}/rules/add", h((*mod).handleDiscordRuleAdd))
		r.Post("/servers/{guildID}/channels", h((*mod).handleDiscordChannelSave))
		r.Post("/servers/{guildID}/removals", h((*mod).handleDiscordKickSave))
		r.Post("/servers/{guildID}/removals/preview", h((*mod).handleDiscordKickPreview))
		r.Post("/servers/{guildID}/rules/{ruleID}/remove", h((*mod).handleDiscordRuleRemove))
		r.Post("/servers/{guildID}/remove", h((*mod).handleDiscordServerForget))
	})
}

// PageData is the view model of discord_servers.html.
type PageData struct {
	module.ShellData
	DiscordServers *discordServersView
}

// Shell returns the embedded shell state for the core renderer.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// The standing model under this module's names.
type (
	orgOwner    = standing.Owner
	orgStanding = standing.Standing
	whoChoice   = module.WhoChoice
)

const (
	ownerCorporation = standing.OwnerCorporation
	ownerAlliance    = standing.OwnerAlliance
)

// meetsRule asks a standing whether it meets one stored rule.
func meetsRule(st orgStanding, owner orgOwner, rule db.DiscordRoleRule) bool {
	return st.Meets(owner, standing.Rule{Kind: rule.Kind, Ref: rule.Ref})
}

// mod is one use of the module: the foundation it reads through, the
// store, the session and the Discord client for the install's current
// configuration. It is made per request or job (cheap), so nothing it
// holds is shared or can go stale.
type mod struct {
	deps     module.Deps
	queries  *db.Queries
	sessions *scs.SessionManager
	db       *sql.DB
	// discord is the Discord client, nil when the install has no
	// Discord settings at all.
	discord *dc.Client
}

func newMod(deps module.Deps) *mod {
	return &mod{
		deps:     deps,
		queries:  deps.DB(),
		sessions: deps.Sessions(),
		db:       deps.SQLDB(),
		discord:  clientFor(deps),
	}
}

// clientFor builds the Discord client, or nil when the install has no
// Discord settings at all.
func clientFor(deps module.Deps) *dc.Client {
	cfg := deps.DiscordConfig()
	dcfg := dc.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, BotToken: cfg.BotToken}
	// A public address makes the callback absolute; without one the
	// sign-in is not offered.
	if full := deps.SiteURL("/discord/callback"); strings.HasPrefix(full, "http") {
		dcfg.RedirectURL = full
	}
	if !dcfg.CanLink() && !dcfg.HasBot() {
		return nil
	}
	return dc.New(dcfg, cfg.HTTP, cfg.BaseURL)
}

// --- adapters over the foundation, named as the code that moved here
// always called them ---

func (app *mod) userID(ctx context.Context) int64 { return app.deps.UserID(ctx) }

func (app *mod) flash(ctx context.Context, msg string) { app.deps.Flash(ctx, msg) }

func (app *mod) flashBack(w http.ResponseWriter, r *http.Request, path string) func(string) {
	return app.deps.FlashBack(w, r, path)
}

func (app *mod) page(ctx context.Context) PageData {
	return PageData{ShellData: app.deps.PageShell(ctx)}
}

func (app *mod) render(ctx context.Context, w http.ResponseWriter, status int, page string, data PageData) {
	app.deps.Render(ctx, w, status, page, &data)
}

func (app *mod) standingFor(ctx context.Context, userID int64) (orgStanding, error) {
	return app.deps.StandingFor(ctx, userID)
}

func (app *mod) manageableOwners(ctx context.Context, userID int64) []orgOwner {
	return app.deps.ManageableOwners(ctx, userID)
}

func (app *mod) ownerName(ctx context.Context, o orgOwner) string { return app.deps.OwnerName(ctx, o) }

func (app *mod) corpAlliance(ctx context.Context, corpID int64) (int64, int64) {
	return app.deps.CorpAlliance(ctx, corpID)
}

func (app *mod) whoChoices(ctx context.Context, owner orgOwner) []whoChoice {
	return app.deps.WhoChoices(ctx, owner)
}

func (app *mod) ruleWho(ctx context.Context, owner orgOwner, rule db.DiscordRoleRule) string {
	return app.deps.RuleWho(ctx, owner, standing.Rule{Kind: rule.Kind, Ref: rule.Ref})
}

func (app *mod) parseRuleWho(ctx context.Context, owner orgOwner, raw string) (string, string, bool) {
	return app.deps.ParseRuleWho(ctx, owner, raw)
}

func (app *mod) pickOwner(ctx context.Context, r *http.Request, offered []orgOwner) (orgOwner, []module.OwnerOption, bool) {
	return app.deps.PickOwner(ctx, r, offered)
}

func (app *mod) resolvedCorpName(ctx context.Context, corpID int64) (string, bool) {
	return app.deps.ResolvedCorpName(ctx, corpID)
}

func (app *mod) displayCharacter(ctx context.Context, characterID int64) string {
	return app.deps.DisplayCharacter(ctx, characterID)
}

// opPath is an op's page.
func opPath(id int64) string { return fmt.Sprintf("/ops/%d", id) }

// newOAuthState returns 32 random bytes hex-encoded (64 chars): the
// single-use state of a sign-in.
func newOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// timeSet is a set sql.NullTime.
func timeSet(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }

// clip shortens s to at most max bytes without splitting a character.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
