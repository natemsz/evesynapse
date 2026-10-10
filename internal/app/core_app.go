// Package app is the EveSynapse web application: configuration,
// EVE SSO auth and sessions, the HTTP routes and page handlers, and
// the background refresh worker. The database and its schema are
// the store package's. The two entrypoints
// (cmd/evesynapse for release, cmd/evesynapse-dev for local dev)
// only wire configuration into New and serve Handler.
package app

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/store"
	"evesynapse/internal/webpush"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed version.txt
var versionFile string

// appVersion is the product version rendered in the footer
// ("v0.3.00.002"). version.txt is the single source of truth:
// every shipped build bumps it, nothing else stamps versions.
var appVersion = "v" + strings.TrimSpace(versionFile)

// Application is the EveSynapse server: config, sessions, the sqlc
// handle, the ESI client, and the in-memory caches the page
// handlers share. Construct with New.
type Application struct {
	cfg      Config
	sessions *scs.SessionManager
	queries  *db.Queries
	jwks     *jwksCache

	// esi is the ESI client: HTTP access, the snapshot cache, and
	// the type/group/place name-resolution caches.
	esi *esi.Client

	// push is the server's identity to browser push services; nil
	// when no VAPID key pair is configured, which turns push off.
	// pushClient is the HTTP client sends go through (tests swap it).
	push       *webpush.VAPID
	pushClient *http.Client

	// discord is the Discord client (discord_link.go), nil when the
	// install has no Discord settings.
	discord *discord.Client

	// db, pool, and stopWorker/workerDone are the resources Close
	// releases: it cancels the worker, waits for workerDone to
	// signal the worker has fully stopped, and only then closes
	// the handles.
	db         *sql.DB
	pool       *pgxpool.Pool
	stopWorker context.CancelFunc
	workerDone chan struct{}
	workerCtx  context.Context // the worker's context, captured in New for background jobs (SDE import)
	startedAt  time.Time       // when New built this application; /healthz measures a worker that never ran from here

	// tokenLocks serializes access-token refreshes one character at a
	// time: CCP rotates refresh tokens on every refresh, so two
	// concurrent refreshes on the same stored token could invalidate
	// each other. Different characters' tokens have nothing to do with
	// each other, and one character's slow refresh does not hold up
	// anyone else's.
	tokenLocks keyedLocks

	// tokens seals and opens the EVE SSO tokens stored in the
	// characters table (auth_tokencrypt.go). Without a
	// TOKEN_ENCRYPTION_KEY it stores them as they are.
	tokens *tokenBox

	// fetchMu serializes the market-history, pilot-record, and
	// type-detail fetch passes between the minute cycle and the
	// urgent want drain, so the two never fetch the same queue row
	// in the same moment.
	fetchMu sync.Mutex

	// urgentMu guards urgentHoldUntil: after ESI pushes back, the
	// urgent drain stays quiet for a while instead of nudging
	// every few seconds into a wall.
	urgentMu        sync.Mutex
	urgentHoldUntil time.Time

	// In-memory cache of built corporation views, keyed by
	// corporation ID; each entry expires with the ESI Expires
	// header of the response it was built from (corp_overview.go).
	// corpCalls holds the refreshes currently running, so
	// concurrent readers of an expired entry share one.
	corpMu    sync.Mutex
	corpCache map[int64]corpCacheEntry
	corpCalls map[int64]*corpCall

	// In-memory cache of GET /markets/prices/ keyed by type ID,
	// refreshed once stale per the response Expires header
	// (market.go).
	pricesMu     sync.Mutex
	prices       map[int64]esi.MarketPrice
	pricesExpiry time.Time

	// sweepMu serializes whole-region book sweep passes (P1
	// region stats, market_region_stats.go): regions advance in
	// parallel inside one pass, but passes never overlap, so a
	// region's staged book (schema 034) has exactly one writer.
	// Sweep progress lives on disk, not in this struct; a
	// restart resumes from the staged cursor.
	sweepMu sync.Mutex

	// The worker-stored market guide (market_guide_prices.go), cached
	// in memory under its fetched_at stamp so renders reload
	// only when a refresh has landed.
	storedPricesMu    sync.Mutex
	storedPricesCache map[int64]esi.MarketPrice
	storedPricesStamp time.Time

	// Character IDs flagged for first-in-line warm-up on the next
	// worker cycle (fresh SSO logins, Sync-page re-warm requests).
	// activity is when each account was last seen, which sets how
	// often the worker refreshes its characters (worker_tiers.go).
	activity      activityLog
	admins        adminOwners
	notifyQuiet   notifyQuietLog // which accounts' data the notification pass has read lately (notify_quiet.go)
	priorityMu    sync.Mutex
	priorityChars map[int64]bool

	// The worker's user-visible status (Sync/Admin pages).
	workerMu sync.Mutex
	worker   workerStatus

	// The SDE importer's user-visible status (Sync page), guarded
	// the same way as the worker status. See sde.go.
	sdeMu sync.Mutex
	sde   sdeStatus

	// Current-page wants (worker_pagewants.go): what each signed-in page
	// is still waiting on, keyed by page scope, so the banner
	// sync indicator can count it. Guarded by pageWantMu.
	pageWantMu sync.Mutex
	pageWants  map[string]map[string]pageWant
}

// New opens the database (applying the embedded baseline schema
// on first boot), builds the application, and starts the
// background worker and the one-shot SSO reachability probe.
// Call Close to stop the worker and release the database.
func New(cfg Config) (*Application, error) {
	tokens, err := newTokenBox(cfg.tokenKey)
	if err != nil {
		return nil, err
	}
	if cfg.signUp.err != nil {
		return nil, cfg.signUp.err
	}
	if p := cfg.signUp; p.restricted() {
		logging.Infof("evesynapse: new accounts are limited to %d listed character(s), %d corporation(s) and %d alliance(s)",
			len(p.characterIDs), len(p.corporationIDs), len(p.allianceIDs))
	} else {
		logging.Infof("evesynapse: new accounts are open to anyone who can sign in with EVE (EVE_ALLOWED_*_IDS limits that)")
	}

	dbConn, pool, err := store.Open(context.Background(), cfg.databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	if cfg.dbConns > 0 {
		store.SizePool(dbConn, cfg.dbConns)
	}

	sessionManager := newSessionManager(pgxstore.New(pool), cfg)
	warnIfServedInTheClear(cfg)

	app := &Application{
		cfg:           cfg,
		push:          pushConfigFromEnv(cfg),
		discord:       discordFromConfig(cfg),
		sessions:      sessionManager,
		queries:       db.New(dbConn),
		jwks:          &jwksCache{},
		db:            dbConn,
		pool:          pool,
		corpCache:     make(map[int64]corpCacheEntry),
		corpCalls:     make(map[int64]*corpCall),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
		pageWants:     make(map[string]map[string]pageWant),
		tokens:        tokens,
	}
	app.esi = esi.New(loginHTTPClient, app.queries, app.validAccessToken)
	app.esi.SetUserAgent(cfg.esiUserAgent())

	// Before anything reads a token: check the stored ones open
	// with the configured key, and encrypt any that predate it.
	if err := app.prepareStoredTokens(context.Background()); err != nil {
		dbConn.Close()
		pool.Close()
		return nil, err
	}

	// When each account was last seen, from before this start
	// (worker_activity.go).
	app.loadActivity(context.Background())
	app.admins.load(context.Background(), app)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	app.stopWorker = stopWorker
	app.workerCtx = workerCtx
	app.workerDone = make(chan struct{})
	app.startedAt = time.Now()
	go func() {
		defer close(app.workerDone)
		app.runWorker(workerCtx)
	}()

	// One-shot reachability probe so phone (Termux) logs immediately
	// show whether CCP is reachable — DNS trouble there otherwise only
	// surfaces midway through a login attempt.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, eveDiscoveryURL, nil)
		if err != nil {
			return
		}
		resp, err := loginHTTPClient.Do(req)
		if err != nil {
			logging.Warnf("evesynapse: EVE SSO discovery not reachable: %v", err)
			return
		}
		defer resp.Body.Close()
		logging.Infof("evesynapse: EVE SSO discovery reachable (HTTP %d)", resp.StatusCode)
	}()

	return app, nil
}

// Close shuts the application down in dependency order: cancel the
// worker's context, wait for the worker (and every pass it runs) to
// fully stop, and only then close the database handle and the session
// pool. Closing the handle first would flood the log with "sql:
// database is closed": the worker passes still in flight would be
// querying a dead handle.
func (app *Application) Close() {
	if app.stopWorker != nil {
		app.stopWorker()
	}
	if app.workerDone != nil {
		<-app.workerDone
	}
	if app.db != nil {
		app.db.Close()
	}
	if app.pool != nil {
		app.pool.Close()
	}
}

// RouteHook registers extra routes on the application's router. The
// dev entrypoint passes devtools.Register; the release entrypoint
// passes none, so dev-only routes are absent from the release
// binary entirely.
type RouteHook func(r chi.Router, a *Application)

// Handler builds the application's HTTP handler. Optional hooks run
// during route setup (the dev-only routes); production routes are
// identical with or without hooks.
func (app *Application) Handler(hooks ...RouteHook) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(app.securityHeaders)
	r.Use(navStateMiddleware)
	// Pages, the stylesheet and the scripts are text and compress to
	// a fraction of their size; fonts and images are left alone.
	r.Use(middleware.Compress(5))
	r.Use(app.crossOriginGuard())
	r.Use(app.sessions.LoadAndSave)
	r.Use(app.slideSession)
	r.Use(app.trackActivity)
	r.Use(app.pageWantScopeMiddleware)

	r.Get("/", app.handleHome)
	r.Route("/home", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Post("/layout", app.handleHomeLayout)
		r.Post("/widget-config", app.handleWidgetConfig)
	})
	r.Get("/healthz", app.handleHealthz)
	r.Get("/favicon.ico", handleFavicon)
	// The push service worker, from the root so it can act for the
	// whole site (notify_push.go).
	r.Get("/sw.js", handleServiceWorker)

	// Embedded static assets (stylesheet, scripts, fonts, the 2013
	// wallpaper); core_static.go has the caching rules. A failure here
	// is loud and fail-closed (503 on /static/*): silently
	// skipping the route served every page unstyled with no
	// signal anywhere.
	if assets, err := staticHandler(); err != nil {
		logging.Errorf("evesynapse: static assets unavailable: %v", err)
		r.Handle("/static/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "static assets unavailable", http.StatusServiceUnavailable)
		}))
	} else {
		r.Handle("/static/*", assets)
	}
	r.Get("/auth/eve", app.handleEVELogin)
	r.Get("/auth/callback", app.handleEVECallback)
	// Signing out changes state, so it is a POST: a link or an image
	// tag on another page cannot sign the visitor out.
	r.Post("/auth/logout", app.handleSignOut)

	// Route hooks (dev entrypoint only) mount here.
	for _, hook := range hooks {
		hook(r, app)
	}

	r.Route("/admin", func(r chi.Router) {
		r.Use(app.requireAdmin)
		r.Get("/", app.handleAdmin)
		// What the Sync and Admin search boxes suggest while typing.
		r.Get("/suggest", app.handleCharacterSuggest)
	})

	r.Route("/corporations", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCorporations)
		r.Get("/members/", app.handleCorpMembers)
		r.Get("/wallets/", app.handleCorpWallets)
		r.Get("/orders/", app.handleCorpOrders)
		r.Get("/assets/", app.handleCorpAssets)
		r.Get("/structures/", app.handleCorpStructures)
		r.Get("/killmails/", app.handleCorpKillmails)
	})

	r.Route("/assets", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleAssets)
		r.Get("/suggest", app.handleAssetsSuggest)
	})

	r.Route("/planets", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handlePlanets)
	})

	r.Route("/mail", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleMail)
		r.Post("/read/", app.handleMailMarkRead)
		r.Get("/compose/", app.handleMailCompose)
		r.Post("/send/", app.handleMailSend)
	})

	// Notifications (notify_pages.go): the list, and the two actions
	// that change what is unread.
	r.Route("/notifications", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleNotifications)
		r.Get(strings.TrimPrefix(notifyBadgePath, "/notifications"), app.handleNotifyBadge)
		r.Post("/read", app.handleNotificationsRead)
		r.Post("/open", app.handleNotificationsOpen)
		r.Get("/settings", app.handleNotificationSettings)
		r.Post("/settings", app.handleNotificationSettingsSave)
		r.Post("/push/subscribe", app.handlePushSubscribe)
		r.Post("/push/unsubscribe", app.handlePushUnsubscribe)
		r.Post("/push/test", app.handlePushTest)
	})

	// Discord (discord_link.go): connecting an account, and what it
	// is used for.
	r.Route("/discord", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/connect", app.handleDiscordConnect)
		r.Get("/callback", app.handleDiscordCallback)
		r.Post("/disconnect", app.handleDiscordDisconnect)
		r.Post("/settings", app.handleDiscordSettings)
		r.Post("/test", app.handleDiscordTest)
		r.Post("/roles/refresh", app.handleDiscordRolesRefresh)
		r.Get("/servers", app.handleDiscordServers)
		r.Post("/servers/add", app.handleDiscordServerAdd)
		r.Post("/servers/share", app.handleDiscordOpsShare)
		r.Post("/servers/{guildID}/save", app.handleDiscordServerSave)
		r.Post("/servers/{guildID}/rules/add", app.handleDiscordRuleAdd)
		r.Post("/servers/{guildID}/channels", app.handleDiscordChannelSave)
		r.Post("/servers/{guildID}/removals", app.handleDiscordKickSave)
		r.Post("/servers/{guildID}/removals/preview", app.handleDiscordKickPreview)
		r.Post("/servers/{guildID}/rules/{ruleID}/remove", app.handleDiscordRuleRemove)
		r.Post("/servers/{guildID}/remove", app.handleDiscordServerForget)
	})

	// Groups (corp_groups.go): sets of characters the directors of a
	// corporation or alliance keep by hand.
	r.Route("/groups", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleGroups)
		r.Post("/create", app.handleGroupCreate)
		r.Post("/{groupID}/delete", app.handleGroupDelete)
		r.Post("/{groupID}/members/add", app.handleGroupMemberAdd)
		r.Post("/{groupID}/members/remove", app.handleGroupMemberRemove)
	})

	r.Route("/calendar", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCalendar)
		r.Post("/respond", app.handleCalendarRespond)
	})

	// Ops (ops.go): EveSynapse's own calendar entries and the
	// sign-ups to them.
	r.Route("/ops", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/new", app.handleOpNew)
		r.Post("/save", app.handleOpSave)
		r.Get("/paps", app.handleOpPAPs)
		r.Get("/{opID}", app.handleOp)
		r.Get("/{opID}/edit", app.handleOpEdit)
		r.Post("/{opID}/signup", app.handleOpSignup)
		r.Post("/{opID}/cancel", app.handleOpCancel)
		r.Post("/{opID}/attendance", app.handleOpAttendance)
	})

	r.Route("/contacts", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleContacts)
		r.Get("/name-fragment", app.handleContactNameFragment)
	})

	// Generic entity-label live regions (worker_pagewants.go): a pending
	// character/structure label anywhere polls its own fragment.
	r.Route("/labels", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/character-fragment", app.handleCharacterLabelFragment)
		r.Get("/corporation-fragment", app.handleCorporationLabelFragment)
		r.Get("/alliance-fragment", app.handleAllianceLabelFragment)
		r.Get("/place-fragment", app.handlePlaceLabelFragment)
	})

	r.Route("/market", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleMarket)
		r.Get("/scanner/", app.handleMarketScanner)
		r.Get("/tradefinder/", app.handleMarketTradefinder)
		r.Get("/leaderboard/", app.handleMarketLeaderboard)
		r.Get("/restock/", app.handleRestock)
		r.Post("/restock/save/", app.handleRestockSave)
		r.Post("/restock/delete/", app.handleRestockDelete)
		r.Get("/suggest", app.handleMarketSuggest)
		r.Get("/history-fragment", app.handleMarketHistoryFragment)
		r.Get("/trader-fragment", app.handleMarketTraderFragment)
		r.Post("/watch", app.handleMarketWatch)
	})

	r.Route("/items", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleItems)
		r.Get("/search.json", app.handleItemSearchJSON)
		r.Get("/description-fragment", app.handleItemDescriptionFragment)
		r.Get("/category/{categoryID}/", app.handleItemsCategory)
		r.Get("/group/{groupID}/", app.handleItemsGroup)
		r.Get("/type/{typeID}/", app.handleItemType)
	})

	// Top banner global search (items + own characters + warmed
	// pilot records, all local).
	r.With(app.requireAuth).Get("/search.json", app.handleTopbarSearch)

	r.Route("/skills", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSkills)
		r.Get("/plans", app.handleSkillPlans)
		r.Get("/plans/", app.handleSkillPlans) // old links and bookmarks carry the slash
		r.Get("/plans/fit", app.handleSkillPlanFitPreview)
		r.Post("/plans/create", app.handleSkillPlanCreate)
		r.Post("/plans/item-add", app.handleSkillPlanItemAdd)
		r.Post("/plans/item-remove", app.handleSkillPlanItemRemove)
		r.Post("/plans/item-move", app.handleSkillPlanItemMove)
		r.Post("/plans/delete", app.handleSkillPlanDelete)
		r.Post("/plans/from-fit", app.handleSkillPlanFromFit)
	})

	r.Route("/character", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCharacter)
		r.Post("/clones/rename", app.handleCloneRename)
	})

	// Public pilot page: a stranger's public record (profile +
	// employment history), warmed by the worker from public ESI.
	r.Route("/pilot", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handlePilot)
		r.Get("/fragment", app.handlePilotFragment)
	})

	// Public corporation & alliance pages: an organization's
	// public record, warmed by the worker from public ESI.
	r.Route("/corporation", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCorporationPage)
		r.Get("/fragment", app.handleCorporationFragment)
	})
	r.Route("/alliance", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleAlliancePage)
		r.Get("/fragment", app.handleAllianceFragment)
	})

	// Solar system & station pages: an SDE place record, the
	// destination every system/station name links to. Nothing to
	// warm — both render from the local SDE.
	r.Route("/system", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSystemPage)
	})
	r.Route("/station", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleStationPage)
	})

	// Player structure page: what this instance knows about one
	// Upwell structure (resolved name, owning corporation, system,
	// type), filled in by the background structure resolution.
	r.Route("/structure", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleStructurePage)
		r.Get("/fragment", app.handleStructureFragment)
	})

	r.Route("/characters", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCharacters)
		r.Get("/switch", app.handleCharacterSwitch)
		r.Post("/tags", app.handleCharacterTags)
		r.Post("/unlink", app.handleCharacterUnlink)
	})

	r.Route("/fittings", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleFittings)
		r.Get("/saved/", app.handleFittingsSaved)
		r.Post("/simulate/", app.handleFitSimulate)
		r.Get("/clones.json", app.handleFitClonesJSON)
		r.Get("/picker.json", app.handleFitPickerJSON)
		r.Post("/save/", app.handleFitSave)
		r.Post("/save-to-eve/", app.handleFitSaveToEVE)
		r.Post("/delete/", app.handleFitDelete)
		r.Post("/import/", app.handleFitImport)
		r.Post("/export/", app.handleFitExport)
		r.Get("/mine.json", app.handleFitMineJSON)
		r.Post("/fork/", app.handleFitFork)
		r.Get("/shopping/{id}/", app.handleFitShopping)
	})

	r.Route("/killmails", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleKillmails)
	})

	r.Route("/wallet", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleWallet)
		r.Get("/graph-fragment", app.handleWalletGraphFragment)
	})

	r.Route("/orders", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleOrders)
	})

	r.Route("/contracts", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleContracts)
	})

	r.Route("/industry", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleIndustry)
	})

	r.Route("/planner", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handlePlanner)
	})

	r.Route("/intel", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/wars/", app.handleIntelWars)
		r.Get("/incursions/", app.handleIntelIncursions)
		r.Get("/fw/", app.handleIntelFW)
	})

	r.Route("/sync", func(r chi.Router) {
		// The banner indicator's poll runs on every signed-in
		// page (app.js), so it is gated on login only; the handler
		// reads nothing but the caller's own pending wants.
		r.With(app.requireAuth).Get("/page-status", app.handlePageSyncStatus)
		r.Group(func(r chi.Router) {
			r.Use(app.requireAdmin)
			r.Get("/", app.handleSync)
			r.Post("/warm", app.handleSyncWarm)
			r.Post("/sde", app.handleSyncSDE)
		})
	})

	return r
}

// handleFavicon serves the app icon as a real .ico (16, 32 and 48px
// PNG images inside), which is what browsers that ignore SVG icons
// (Safari, older Edge, feed readers, link previews) and the default
// /favicon.ico request expect. The same drawing, as SVG, is linked from
// base.html as /static/favicon.svg for the browsers that use it.
// The cache is a day, not a week: when the logo changes, a favicon
// should not linger.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	icon, err := fs.ReadFile(staticFS, "static/favicon.ico")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(icon)
}

// requireAuth gates the admin on the session "authenticated" flag, which
// is only set by a completed EVE SSO login (or the dev-only /dev-login).
func (app *Application) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !app.sessions.GetBool(r.Context(), sessionAuthenticated) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAdmin reports whether the request's session belongs to an
// administrator's account (Issues 23/24): authenticated, and one of
// the characters linked to the account is listed in
// EVE_ADMIN_CHARACTER_IDS. It is the account that is admin, not
// whichever of its characters is selected at the moment: choosing
// an alt in the header switcher must not make the Admin and Sync
// pages disappear.
func (app *Application) isAdmin(ctx context.Context) bool {
	if len(app.cfg.adminCharIDs) == 0 || !app.sessions.GetBool(ctx, sessionAuthenticated) {
		return false
	}
	return app.adminAmong(app.sessionCharacters(ctx))
}

// adminAmong reports whether any of an account's characters is an
// administrator (auth_admin.go).
func (app *Application) adminAmong(characters []db.Character) bool {
	for _, ch := range characters {
		if app.adminHolds(ch) {
			return true
		}
	}
	return false
}

// requireAdmin gates debugging/dev pages (Admin, Sync) on an admin
// account, not just login. Non-admins get 403.
func (app *Application) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !app.isAdmin(r.Context()) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs method, path, status and duration. The query string
// is deliberately never logged: /auth/callback carries an OAuth
// authorization code and error details that don't belong in logs.
// A request the server itself failed (5xx) is logged as an error;
// every other request, refusals included, is routine.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		logf := logging.Infof
		switch {
		case ww.Status() >= http.StatusInternalServerError:
			logf = logging.Errorf
		case r.URL.Path == notifyBadgePath:
			// Every open page asks this every few seconds; at info
			// it would bury everything else in the log.
			logf = logging.Debugf
		}
		logf("%s %s -> %d (%s)", r.Method, r.URL.Path, ww.Status(),
			time.Since(start).Round(time.Millisecond))
	})
}
