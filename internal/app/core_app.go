// Package app is the EveSynapse web application: configuration,
// EVE SSO auth and sessions, the HTTP routes and page handlers, and
// the background refresh worker. The database and its schema are
// the store package's. The two entrypoints
// (cmd/evesynapse for release, cmd/evesynapse-dev for local dev)
// only wire configuration into New and serve Handler.
package app

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"evesynapse/internal/standing"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
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
	// discordHTTP and discordBase inject the Discord API for tests; nil
	// and empty mean the real one (read by the Discord module through
	// Deps.DiscordConfig).
	discordHTTP *http.Client
	discordBase string

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

	// allianceSeedMu guards lastAllianceSeed, the worker's daily
	// full-universe alliance seeding pass (intel_org_worker.go).
	allianceSeedMu   sync.Mutex
	lastAllianceSeed time.Time

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
	// orgFetchMu serializes organization record drains (cycle and
	// urgent) on their own, so a long pass under fetchMu (a market
	// sweep) never keeps a name someone is looking at waiting.
	orgFetchMu sync.Mutex
	// histFetchMu serializes market history fetches (cycle and urgent)
	// apart from fetchMu, for the same reason.
	histFetchMu sync.Mutex

	// urgentMu guards urgentHoldUntil: after ESI pushes back, the
	// urgent drain stays quiet for a while instead of nudging
	// every few seconds into a wall.
	urgentMu        sync.Mutex
	urgentHoldUntil time.Time

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
	activity    activityLog
	admins      adminOwners
	notifyQuiet notifyQuietLog // which accounts' data the notification pass has read lately (notify_quiet.go)
	// foresight is what warming on game events keeps between cycles
	// (foresight.go), and books the order books fetched lately
	// (market_book.go).
	foresight     foresightState
	books         bookCache
	priorityMu    sync.Mutex
	priorityChars map[int64]bool

	// Public fit page views counted since the worker last wrote
	// them (char_fit_public_store.go).
	fitViews fitViewCounter

	// Per-address throttles on the expensive anonymous routes
	// (core_throttle.go), made on first use.
	anonThrottleOnce sync.Once
	anonPageThrottle *anonThrottle
	anonSimThrottle  *anonThrottle

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

	// Page-serve metrics (sync_metrics.go): page renders and the
	// ages of the records they showed, for the Sync page. Guarded
	// by serveMu.
	serveMu       sync.Mutex
	serveRenders  uint64
	serveFull     uint64
	serveViews    uint64
	serveAgeHour  uint64 // rendered records under an hour old
	serveAgeOlder uint64 // ... over a day old (the rest fall between)

	// Typing-intent guess sequences (worker_typing.go): one
	// keystroke's replace-plus-note must land as one step, or two
	// keystrokes in a box interleave and the older one's guesses
	// survive beside the newer. The step is per box, so the lock
	// is too: setTypingGuesses holds the box's stripe for the
	// replace and the notes, and nothing else. Searching happens
	// before it, unlocked.
	typingLocks [typingLockStripes]sync.Mutex
	// typingPrunedAt is when the guess ledger was last pruned
	// (unix seconds).
	typingPrunedAt atomic.Int64

	// Shared cache across instances (swarm_*.go): this
	// instance's signing identity and its peer client, set at
	// startup when SHARED_CACHE enables sharing, nil otherwise.
	// swarmMu guards the next gossip time only; the identity
	// never changes after startup.
	swarmID         *swarmIdentity
	swarmCli        *swarmClient
	swarmMu         sync.Mutex
	swarmNextGossip time.Time
	swarmNextLoaded bool
	// swarmStreakMu guards the consecutive-failure streaks
	// (signer to current run length). In-memory only: a
	// restart forgets velocity but the ledger keeps every
	// mark, so the trust floor still holds.
	swarmStreakMu sync.Mutex
	swarmStreak   map[string]int
	// swarmCover is what peers warm for the swarm and which misses
	// have been left to them (swarm_warm.go).
	swarmCover swarmCoverage

	// Trade ideas (tradeideas.go): the model client and the Meta API
	// base, set in tests; production builds a timeout client per
	// refresh and calls api.meta.ai.
	tradeIdeasHTTP *http.Client
	metaAIAPIBase  string
	// moduleWG counts module goroutines started through Background;
	// Close waits on it.
	moduleWG sync.WaitGroup

	// names writes learned character, corporation and alliance
	// names to entity_names (entity_names.go).
	names   *nameWriter
	namesWG sync.WaitGroup // the first-start bulk name fill
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
	if cfg.swarm.err != nil {
		return nil, cfg.swarm.err
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
		sessions:      sessionManager,
		queries:       db.New(dbConn),
		jwks:          &jwksCache{},
		db:            dbConn,
		pool:          pool,
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
		pageWants:     make(map[string]map[string]pageWant),
		tokens:        tokens,
	}
	app.esi = esi.New(loginHTTPClient, app.queries, app.validAccessToken)
	app.esi.SetUserAgent(cfg.esiUserAgent())
	// Every stored name is in memory before the first request, and
	// every name learned from here on is kept (entity_names.go).
	app.loadEntityNames(context.Background())
	app.names = newNameWriter(app.db)
	app.esi.SetNameSink(app.names.enqueue)

	// Before anything reads a token: check the stored ones open
	// with the configured key, and encrypt any that predate it.
	if err := app.prepareStoredTokens(context.Background()); err != nil {
		dbConn.Close()
		pool.Close()
		return nil, err
	}

	// The swarm's signing identity (swarm_identity.go): minted on
	// first enable, sealed under the shared secret. Sharing stays
	// off without it, and startup fails rather than sharing
	// unsigned.
	if cfg.swarm.enabled() {
		id, err := loadSwarmIdentity(context.Background(), app.queries, cfg.swarm.secret)
		if err != nil {
			dbConn.Close()
			pool.Close()
			return nil, err
		}
		app.swarmID = id
		app.swarmCli = newSwarmClient(cfg.swarm.secret)
		logging.Infof("evesynapse: shared cache enabled (%s)", cfg.swarm.mode)
	}

	// When each account was last seen, from before this start
	// (worker_activity.go).
	app.loadActivity(context.Background())
	app.admins.load(context.Background(), app)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	app.stopWorker = stopWorker
	app.workerCtx = workerCtx
	app.startedAt = time.Now()
	if !cfg.noWorker {
		app.workerDone = make(chan struct{})
		go func() {
			defer close(app.workerDone)
			app.runWorker(workerCtx)
		}()
		// A database that has never been filled with names fills
		// itself once, in the background (entity_names_populate.go).
		app.namesWG.Add(1)
		go func() {
			defer app.namesWG.Done()
			app.populateNamesIfEmpty(workerCtx)
		}()
	}

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
	app.moduleWG.Wait()
	app.namesWG.Wait()
	if app.names != nil {
		app.names.close()
	}
	if app.db != nil {
		// Views counted since the worker last wrote them.
		app.flushFitViews(context.Background())
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
	// Anonymous visitors to the routes that cost real work are
	// throttled per address (core_throttle.go).
	pageThrottle, simThrottle := app.anonThrottles()
	anonPages, anonSim := app.throttleAnon(pageThrottle), app.throttleAnon(simThrottle)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(app.securityHeaders)
	r.Use(navStateMiddleware)
	// Pages, the stylesheet and the scripts are text and compress to
	// a fraction of their size; fonts and images are left alone.
	r.Use(middleware.Compress(5))
	r.Use(app.crossOriginGuard())
	r.Use(markPrefetch)
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
	// The statement that names the site's Android app (core_assetlinks.go).
	r.Get(assetLinksPath, app.handleAssetLinks)

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
		r.Post("/accounts/remove", app.handleAdminAccountRemove)
		// Shared cache (swarm_pages.go): add, approve,
		// exclude, re-admit and remove swarm members.
		r.Post("/swarm/add", app.handleSwarmAddPeer)
		r.Post("/swarm/approve", app.handleSwarmApprove)
		r.Post("/swarm/exclude", app.handleSwarmExclude)
		r.Post("/swarm/admit", app.handleSwarmAdmit)
		r.Post("/swarm/remove", app.handleSwarmRemove)
	})

	r.Route("/corporations", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCorporations)
		r.Get("/members/", app.handleCorpMembers)
		r.Post("/members/group", app.handleCorpMembersGroup)
		r.Get("/settings/", app.handleCorpSettings)
		r.Post("/settings/save", app.handleCorpSettingsSave)
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

	// Ship replacement (corp_srp.go).
	r.Route("/srp", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSRP)
		r.Post("/request", app.handleSRPRequest)
		r.Post("/paylist", app.handleSRPPayList)
		r.Post("/{id}/decide", app.handleSRPDecide)
		r.Post("/{id}/withdraw", app.handleSRPWithdraw)
	})

	// Doctrines and their fits (corp_doctrines.go).
	r.Route("/doctrines", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleDoctrines)
		r.Get("/fits/", app.handleCorpFits)
		r.Post("/create", app.handleDoctrineCreate)
		r.Get("/suggest", app.handleDoctrineSuggest)
		r.Get("/{doctrineID}", app.handleDoctrine)
		r.Post("/{doctrineID}/save", app.handleDoctrineSave)
		r.Post("/{doctrineID}/delete", app.handleDoctrineDelete)
		r.Post("/{doctrineID}/fits/add", app.handleDoctrineFitAdd)
		r.Post("/{doctrineID}/fits/{fitID}/remove", app.handleDoctrineFitRemove)
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
	// Name labels answer a pending label's poll from stored rows
	// only, so the public record pages can use them too.
	r.Route("/labels", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/character-fragment", app.handleCharacterLabelFragment)
		r.Get("/corporation-fragment", app.handleCorporationLabelFragment)
		r.Get("/alliance-fragment", app.handleAllianceLabelFragment)
		r.Get("/place-fragment", app.handlePlaceLabelFragment)
	})

	r.Route("/market", func(r chi.Router) {
		// Public: the market reads shared data. Anonymous visitors
		// are throttled per address and their renders read stored
		// data only (no live order book, no queued work); the
		// personal pieces (restock, watches) stay signed-in.
		r.With(anonPages).Get("/", app.handleMarket)
		r.With(anonPages).Get("/scanner/", app.handleMarketScanner)
		r.With(anonPages).Get("/tradefinder/", app.handleMarketTradefinder)
		r.With(anonPages).Get("/leaderboard/", app.handleMarketLeaderboard)
		r.With(anonPages).Get("/suggest", app.handleMarketSuggest)
		r.With(anonPages).Get("/history-fragment", app.handleMarketHistoryFragment)
		r.With(anonPages).Get("/trader-fragment", app.handleMarketTraderFragment)
		r.With(app.requireAuth).Get("/restock/", app.handleRestock)
		r.With(app.requireAuth).Post("/restock/save/", app.handleRestockSave)
		r.With(app.requireAuth).Post("/restock/delete/", app.handleRestockDelete)
		r.With(app.requireAuth).Post("/watch", app.handleMarketWatch)
	})

	r.Route("/items", func(r chi.Router) {
		// The browser is public: market browse data is shared by
		// nature and the render depends on no session or
		// character state. Anonymous renders read stored rows
		// only (a description not stored yet is not queued).
		r.With(anonPages).Get("/", app.handleItems)
		r.With(anonPages).Get("/type/{typeID}/", app.handleItemType)
		r.With(anonPages).Get("/search.json", app.handleItemSearchJSON)
		r.With(anonPages).Get("/description-fragment", app.handleItemDescriptionFragment)
		r.With(anonPages).Get("/category/{categoryID}/", app.handleItemsCategory)
		r.With(anonPages).Get("/group/{groupID}/", app.handleItemsGroup)
	})

	// Top banner global search (items + own characters + warmed
	// pilot records, all local).
	r.With(anonPages).Get("/search.json", app.handleTopbarSearch)

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
		r.Use(anonPages)
		r.Get("/", app.handlePilot)
		r.Get("/fragment", app.handlePilotFragment)
	})

	// Public record pages (pilot, corporation, alliance, system,
	// station, structure). Every render reads stored rows only: the
	// instance's own data and the swarm data it has applied (which
	// lands in the same tables; staged entries are never read). A
	// miss renders the not-yet-known state and is queued for the
	// worker; nothing is fetched while the page renders.
	r.Route("/corporation", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleCorporationPage)
		r.Get("/fragment", app.handleCorporationFragment)
	})
	r.Route("/alliance", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleAlliancePage)
		r.Get("/fragment", app.handleAllianceFragment)
	})

	// Solar system & station pages: an SDE place record, the
	// destination every system/station name links to. Nothing to
	// warm — both render from the local SDE.
	r.Route("/system", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleSystemPage)
	})
	r.Route("/region", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleRegionPage)
	})
	r.Route("/station", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleStationPage)
	})

	// Player structure page: what this instance knows about one
	// Upwell structure (resolved name, owning corporation, system,
	// type), filled in by the background structure resolution.
	r.Route("/structure", func(r chi.Router) {
		r.Use(anonPages)
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

	// Crawler and anonymous routes: no session middleware.
	// Crawlers carry no cookie; anonymous readers get stored
	// rows only, never per-user data.
	r.Get("/robots.txt", app.handleRobots)
	r.Get("/sitemap.xml", app.handleSitemap)

	r.Route("/ships", func(r chi.Router) {
		r.With(anonPages).Get("/{typeID}/", app.handlePublicShip)
	})

	r.Route("/fittings", func(r chi.Router) {
		// Anonymous-safe surfaces: the simulator (All-V without
		// a session), the public browser and fit pages, and the
		// SDE-only picker. Handlers branch on the session.
		r.With(anonPages).Get("/", app.handleFittings)
		r.With(anonSim).Post("/simulate/", app.handleFitSimulate)
		r.With(anonPages).Get("/picker.json", app.handleFitPickerJSON)
		r.With(anonPages).Get("/public/", app.handlePublicFits)
		r.With(anonPages).Get("/public/suggest", app.handlePublicFitSuggest)
		r.With(anonPages).Get("/public/fit/{fitID}/", app.handlePublicFitPage)
		r.With(app.requireAuth).Post("/public/fit/{fitID}/rate", app.handlePublicFitRate)
		r.Group(func(r chi.Router) {
			r.Use(app.requireAuth)
			r.Get("/saved/", app.handleFittingsSaved)
			r.Get("/clones.json", app.handleFitClonesJSON)
			r.Post("/save/", app.handleFitSave)
			r.Post("/save-to-eve/", app.handleFitSaveToEVE)
			r.Post("/delete/", app.handleFitDelete)
			r.Post("/import/", app.handleFitImport)
			r.Post("/export/", app.handleFitExport)
			r.Get("/mine.json", app.handleFitMineJSON)
			r.Post("/fork/", app.handleFitFork)
			r.Get("/shopping/{id}/", app.handleFitShopping)
		})
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

	// Public tools: the build planner, the PI planner and the Intel
	// pages render from static and public data, so visitors without
	// a session use them too (throttled per address; a build plan
	// can be large, so the planner shares the simulator's tighter
	// allowance). A signed-in visitor also gets their own characters'
	// stock, blueprints, skills and colonies. The Industry overview
	// stays signed-in only.
	r.Route("/industry", func(r chi.Router) {
		r.With(app.requireAuth).Get("/", app.handleIndustry)
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

	// The swarm's machine API (swarm_api.go): bearer-gated by the
	// shared secret, dark (404) while sharing is off. No session
	// middleware — instances are not browsers.
	r.Route("/swarm", func(r chi.Router) {
		r.Get("/members", app.handleSwarmMembers)
		r.Post("/pull", app.handleSwarmPull)
		r.Get("/warms", app.handleSwarmWarms)
		r.Post("/seed/register", app.handleSwarmSeedRegister)
	})

	for _, m := range registeredModules {
		if m.Routes != nil {
			m.Routes(r, app)
		}
	}

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

// --- module.Deps implementation ---

var _ module.Deps = (*Application)(nil)

func (app *Application) DB() *db.Queries {
	return app.queries
}

func (app *Application) ValuationPrices(ctx context.Context) map[int64]esi.MarketPrice {
	return app.valuationPrices(ctx)
}

func (app *Application) LoadCorpSnapshot(ctx context.Context, characterID int64, kind string, out any) bool {
	return app.loadCorpSnapshot(ctx, characterID, kind, out)
}

func (app *Application) PINotEnabled(ctx context.Context, ch db.Character) bool {
	return app.piNotEnabled(ctx, ch)
}

func (app *Application) PublicShell(ctx context.Context) module.ShellData {
	loggedIn := app.sessions.GetBool(ctx, sessionAuthenticated)
	return module.ShellData{
		LoggedIn:      loggedIn,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

func (app *Application) PickCharacter(ctx context.Context, r *http.Request, path string) ([]db.Character, db.Character, []module.CharLink, error) {
	return app.pickCharacter(ctx, r, path)
}

func (app *Application) Render(ctx context.Context, w http.ResponseWriter, status int, page string, data any) {
	app.render(ctx, w, status, page, data)
}

func (app *Application) UserID(ctx context.Context) int64 { return app.userID(ctx) }

func (app *Application) LoadGlobalSnapshot(ctx context.Context, kind string, out any) bool {
	return app.loadGlobalSnapshot(ctx, kind, out)
}

func (app *Application) UserCorporationIDs(ctx context.Context, userID int64) map[int64]bool {
	return app.userCorporationIDs(ctx, userID)
}

func (app *Application) OrgLink(ctx context.Context, corporationID, allianceID int64) template.HTML {
	return app.orgDisplay(ctx, corporationID, allianceID)
}

func (app *Application) SystemRef(ctx context.Context, systemID int64) module.PlaceRef {
	return app.linkPlace(ctx, systemID, app.locationTitle(ctx, systemID, "solar_system"))
}

func (app *Application) ConstellationName(id int64) (string, bool) {
	return app.esi.CachedConstellationName(id)
}

func (app *Application) FormatFinish(ts string) string { return formatFinish(ts) }

func (app *Application) HumanizeEnum(s string) string { return humanizeEnum(s) }

func (app *Application) WarDetailWindow() int { return maxWarsShown }

func (app *Application) ModelConfig() module.ModelConfig {
	return module.ModelConfig{
		MetaKey:             app.cfg.metaAIKey,
		CloudflareAccountID: app.cfg.cloudflareAccountID,
		CloudflareToken:     app.cfg.cloudflareAuthToken,
		HTTP:                app.tradeIdeasHTTP,
		MetaAPIBase:         app.metaAIAPIBase,
	}
}

func (app *Application) Background(fn func()) {
	app.moduleWG.Add(1)
	go func() {
		defer app.moduleWG.Done()
		fn()
	}()
}

func (app *Application) TypeName(ctx context.Context, typeID int64) string {
	name := app.typeNameOrID(ctx, typeID)
	if strings.HasPrefix(name, "Type #") {
		// The SDE knows most types by name; fall back to the local SDE
		// row before settling for the placeholder.
		if row, err := app.queries.GetSDEType(ctx, typeID); err == nil && row.Name != "" {
			name = row.Name
		}
	}
	return name
}

func (app *Application) StationRef(ctx context.Context, memo map[int64]module.PlaceRef, locationID int64) module.PlaceRef {
	return app.scannerStationRef(ctx, memo, locationID)
}

func (app *Application) SpreadDefaults() (float64, int) {
	return defaultScannerMinSpreadPct, scannerRowCap
}

func (app *Application) RequireAuth() func(http.Handler) http.Handler { return app.requireAuth }

func (app *Application) Sessions() *scs.SessionManager { return app.sessions }

func (app *Application) Flash(ctx context.Context, msg string) { app.flash(ctx, msg) }

func (app *Application) FlashBack(w http.ResponseWriter, r *http.Request, path string) func(string) {
	return app.flashBack(w, r, path)
}

func (app *Application) SiteURL(path string) string {
	return app.cfg.publicOrigin() + notifyTarget(path)
}

func (app *Application) PageShell(ctx context.Context) module.ShellData {
	return app.page(ctx).ShellData
}

func (app *Application) Fragment(page, define string, data any) (template.HTML, error) {
	ts, err := parsedTemplate(&fragmentTemplates, "fragment", page, "templates/balancechart.html", "templates/walletflow.html", "templates/charselector.html", "templates/fit_workbench.html", "templates/locked.html", "templates/sectiontabs.html")
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, define, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func (app *Application) SQLDB() *sql.DB { return app.db }

func (app *Application) SealToken(plain string, userID int64, field string) (string, error) {
	return app.tokens.seal(plain, userID, field)
}

func (app *Application) OpenToken(sealed string, userID int64, field string) (string, error) {
	return app.tokens.open(sealed, userID, field)
}

func (app *Application) DiscordConfig() module.DiscordConfig {
	return module.DiscordConfig{
		ClientID:     app.cfg.discordClientID,
		ClientSecret: app.cfg.discordClientSecret,
		BotToken:     app.cfg.discordBotToken,
		HTTP:         app.discordHTTP,
		BaseURL:      app.discordBase,
	}
}

func (app *Application) StandingFor(ctx context.Context, userID int64) (standing.Standing, error) {
	return app.standingFor(ctx, userID)
}

func (app *Application) ManageableOwners(ctx context.Context, userID int64) []standing.Owner {
	return app.manageableOwners(ctx, userID)
}

func (app *Application) OwnerName(ctx context.Context, o standing.Owner) string {
	return app.ownerName(ctx, o)
}

func (app *Application) CorpAlliance(ctx context.Context, corpID int64) (int64, int64) {
	return app.corpAlliance(ctx, corpID)
}

func (app *Application) WhoChoices(ctx context.Context, owner standing.Owner) []module.WhoChoice {
	var out []module.WhoChoice
	for _, c := range app.whoChoices(ctx, owner) {
		out = append(out, module.WhoChoice{Value: c.Value, Label: c.Label})
	}
	return out
}

func (app *Application) RuleWho(ctx context.Context, owner standing.Owner, rule standing.Rule) string {
	return app.ruleWho(ctx, owner, db.DiscordRoleRule{Kind: rule.Kind, Ref: rule.Ref})
}

func (app *Application) ParseRuleWho(ctx context.Context, owner standing.Owner, raw string) (string, string, bool) {
	return app.parseRuleWho(ctx, owner, raw)
}

func (app *Application) PickOwner(ctx context.Context, r *http.Request, offered []standing.Owner) (standing.Owner, []module.OwnerOption, bool) {
	owner, options, ok := app.pickOwner(ctx, r, offered)
	out := make([]module.OwnerOption, len(options))
	for i, o := range options {
		out[i] = module.OwnerOption{ID: o.ID, Name: o.Name, Active: o.Active, Param: o.Param, Value: o.Value}
	}
	return owner, out, ok
}

func (app *Application) ResolvedCorpName(ctx context.Context, corpID int64) (string, bool) {
	return app.resolvedCorpName(ctx, corpID)
}

func (app *Application) DisplayCharacter(ctx context.Context, characterID int64) string {
	return app.displayCharacter(ctx, characterID)
}

func (app *Application) AnonSim() func(http.Handler) http.Handler {
	_, simThrottle := app.anonThrottles()
	return app.throttleAnon(simThrottle)
}

func (app *Application) CachedTypeNames(ctx context.Context, ids []int64) map[int64]string {
	return app.esi.CachedTypeNames(ctx, ids)
}

func (app *Application) CachedPlaceName(ctx context.Context, id int64) (string, bool) {
	return app.esi.CachedPlaceName(ctx, id)
}

func (app *Application) NotePlaceWant(ctx context.Context, id int64) {
	app.notePageWantFromContext(ctx, pageWantPlace, id)
}

func (app *Application) SessionCharacterID(ctx context.Context) int64 {
	return sessionCharID(app.sessions, ctx)
}

func (app *Application) SlotSkillIDs(ctx context.Context) map[string]int64 {
	return app.slotSkillIDs(ctx)
}

func (app *Application) WriteSuggest(w http.ResponseWriter, rows []module.SuggestItem) {
	out := make([]suggestItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, suggestItem(r))
	}
	writeSuggestJSON(w, out)
}

func (app *Application) ServeCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) {
	serveCSV(w, filename, header, rows)
}

func (app *Application) AnonPages() func(http.Handler) http.Handler {
	pageThrottle, _ := app.anonThrottles()
	return app.throttleAnon(pageThrottle)
}
