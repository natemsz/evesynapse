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
	"evesynapse/internal/fitview"
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

	// intelSystems is the system-name dictionary pasted intel chat
	// lines are read against (channels.go).
	intelSystems systemDict

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

	// /healthz's last check (core_health.go): the answer is
	// cached for a few seconds, and its error logging throttled,
	// so a flood of health checks cannot become a flood of
	// database pings and log lines.
	healthMu          sync.Mutex
	healthCacheAt     time.Time
	healthCacheResult []string
	healthLogAt       time.Time

	// allianceSeedMu guards lastAllianceSeed, the worker's daily
	// full-universe alliance seeding pass (intel_org_worker.go).
	allianceSeedMu   sync.Mutex
	lastAllianceSeed time.Time

	// demoMu guards the cached demo account id (demo.go).
	demoMu           sync.Mutex
	demoUserIDCached int64
	demoUserKnown    bool

	// tokenLocks serializes access-token refreshes one character at a
	// time: CCP rotates refresh tokens on every refresh, so two
	// concurrent refreshes on the same stored token could invalidate
	// each other. Different characters' tokens have nothing to do with
	// each other, and one character's slow refresh does not hold up
	// anyone else's.
	tokenLocks keyedLocks

	// tokens seals and opens the EVE SSO tokens stored in the
	// characters table (auth_tokencrypt.go). Its key comes from
	// the configured provider (auth_tokenkey.go); with no key it
	// stores tokens as they are.
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
	// intelAnalysisMu serializes the intel reports' name and
	// affiliation lookups (intel_analysis.go).
	intelAnalysisMu sync.Mutex

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

	// Exact-name type lookups a market search set going in the
	// background (market.go): the normalized query and when its
	// resolution was last attempted, so a name ESI cannot settle
	// is not re-asked on every search. In memory only; a restart
	// forgets, and the stored type_names cache is the durable
	// half.
	typeLookupMu    sync.Mutex
	typeLookupTried map[string]time.Time

	// sweepMu serializes whole-region book sweep passes (P1
	// region stats, market_region_stats.go): regions advance in
	// parallel inside one pass, but passes never overlap, so a
	// region's staged book (schema 034) has exactly one writer.
	// Sweep progress lives on disk, not in this struct; a
	// restart resumes from the staged cursor.
	sweepMu sync.Mutex

	// pubContractMu serializes public-contract sweep passes
	// (pub_contracts.go): one pass walks every region's cursor,
	// and passes never overlap.
	pubContractMu sync.Mutex

	// The worker-stored market guide (market_guide_prices.go), cached
	// in memory under its fetched_at stamp so renders reload
	// only when a refresh has landed.
	storedPricesMu    sync.Mutex
	storedPricesCache map[int64]esi.MarketPrice
	storedPricesStamp time.Time

	// The worker's true values (truevalue.go), cached in
	// memory for a short window so renders and valuation
	// reload the table only a few times an hour; and the
	// pricing pass's own progress between cycles
	// (truevalue_worker.go): sweep cursor, the input
	// fingerprint the stored rows were computed under, and
	// the public-contract samples gathered so far.
	trueValuesMu     sync.Mutex
	trueValuesCache  map[int64]db.TrueValue
	trueValuesLoaded time.Time
	trueValuePass    tvPassState

	// The killboard (killfeed.go): each system's region, loaded
	// once from the SDE for kill ingest, and when retention last
	// pruned the year-old details.
	killRegionsMu  sync.Mutex
	killRegions    map[int64]int64
	killLastPruned time.Time
	killStatsMemo  *killboardStatsMemo // the feed's headline, held a minute

	// The Forge's swept typical sells (market_guide_prices.go),
	// the first tier of the house price order, cached in memory
	// on the same window as the true values so renders reload
	// the stats table only a few times an hour.
	forgeTypicalsMu     sync.Mutex
	forgeTypicalsCache  map[int64]float64
	forgeTypicalsLoaded time.Time

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

	// anonMarketOffState is the live anonymous-market switch
	// (market_anon.go): seeded from the stored setting or the
	// MARKET_ANON boot default at startup, flipped by the Admin
	// page without a restart.
	anonMarketOffState atomic.Bool
}

// New opens the database (applying the embedded baseline schema
// on first boot), builds the application, and starts the
// background worker and the one-shot SSO reachability probe.
// Call Close to stop the worker and release the database.
func New(cfg Config) (*Application, error) {
	tokens, tokenKeyProvider, err := tokenBoxForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateCredentialSources(cfg); err != nil {
		return nil, err
	}
	if cfg.creds == nil {
		cfg.creds = newCredentialStore()
	}
	if tokenKeyFetchedRemotely(tokenKeyProvider) && cfg.tokenKey != "" {
		// The provider wins; the env key stays only as the
		// operator's way back (TOKEN_KEY_PROVIDER=env).
		logging.Warnf("evesynapse: TOKEN_ENCRYPTION_KEY is set but TOKEN_KEY_PROVIDER=%s, so the env key is ignored", tokenKeyProvider)
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
	app.esi = esi.New(esi.NewHTTPClient(), app.queries, app.validAccessToken)
	app.esi.SetUserAgent(cfg.esiUserAgent())
	// Every stored name is in memory before the first request, and
	// every name learned from here on is kept (entity_names.go).
	app.loadAnonMarketSetting(context.Background())
	app.loadEntityNames(context.Background())
	app.names = newNameWriter(app.db)
	app.esi.SetNameSink(app.names.enqueue)

	// Before anything reads a token: check the stored ones open
	// with the configured key, and encrypt any that predate it.
	// A remote provider's key has not arrived yet (it comes from
	// the network, in the background — see startTokenKeyUnlock),
	// so its pass runs at unlock instead; the app serves meanwhile.
	if tokenKeyFetchedRemotely(tokenKeyProvider) {
		logging.Infof("evesynapse: the token encryption key comes from the %s provider; stored tokens unlock when it answers", tokenKeyProvider)
	} else if err := app.prepareStoredTokens(context.Background()); err != nil {
		dbConn.Close()
		pool.Close()
		return nil, err
	}

	// Each module starts once the store is open; a module whose
	// settings are wrong stops the start (the swarm refuses to share
	// unsigned, for one).
	for _, m := range registeredModules {
		if m.Init == nil {
			continue
		}
		if err := m.Init(app); err != nil {
			dbConn.Close()
			pool.Close()
			return nil, err
		}
	}

	// When each account was last seen, from before this start
	// (worker_activity.go).
	app.loadActivity(context.Background())
	app.admins.load(context.Background(), app)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	app.stopWorker = stopWorker
	app.workerCtx = workerCtx
	app.startedAt = time.Now()
	if tokenKeyFetchedRemotely(tokenKeyProvider) {
		app.startTokenKeyUnlock(workerCtx)
	}
	// The working credentials' external sources resolve the
	// same way (auth_credentials.go): in the background, each
	// taking effect through the Config accessors as it lands.
	if hasExternalCredentials(cfg) {
		app.startCredentialResolution(workerCtx)
	}
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

	// The shared demo account (demo.go): created and seeded here
	// so the landing page's Try it now button always has an
	// account to open. A failure is a warning, not a boot
	// failure: the sign-in route retries, and the worker's
	// reset pass keeps retrying until the seed lands.
	if err := app.ensureDemoAccount(context.Background()); err != nil {
		logging.Warnf("demo: prepare the demo account: %v", err)
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
	pageThrottle, _ := app.anonThrottles()
	anonPages := app.throttleAnon(pageThrottle)
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
	// The demo account's write safeguards (demo.go): turns away
	// mutating requests from demo sessions before any handler
	// runs. Inert for every other session.
	r.Use(app.demoWriteGate)

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
	// The About page (about, contact, privacy policy) is public:
	// the footer on every page links into its anchored sections.
	r.Get("/about", app.handleAbout)
	r.Get("/about/", app.handleAbout)
	r.Get("/license", app.handleLicense)
	r.Get("/license/", app.handleLicense)
	// The Report a bug page is public for the same reason: the
	// people who hit a bug may not have an account yet, and the
	// footer on every page links here.
	r.Get("/report", app.handleReport)
	r.Get("/report/", app.handleReport)
	r.Get("/auth/link", app.handleLinkChoose)
	r.Get("/auth/eve", app.handleEVELogin)
	r.Get("/auth/login", app.handleEVELoginIdentity)
	// The landing page's Try it now: a session on the shared
	// demo account, no EVE SSO (demo.go).
	r.Get("/auth/demo", app.handleDemoSignIn)
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
		r.Post("/market-anon", app.handleAdminMarketAnon)
	})

	r.Route("/corporations", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCorporations)
		r.Get("/settings/", app.handleCorpSettings)
		r.Post("/settings/save", app.handleCorpSettingsSave)
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

	// Notifications (notify_pages.go): the list, and the two actions
	// that change what is unread.
	r.Route("/notifications", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleNotifications)
		r.Get(strings.TrimPrefix(notifyBadgePath, "/notifications"), app.handleNotifyBadge)
		r.Post("/read", app.handleNotificationsRead)
		r.Post("/open", app.handleNotificationsOpen)
		r.Post("/prompt", app.handleNotifyPrompt)
		r.Get("/settings", app.handleNotificationSettings)
		r.Post("/settings", app.handleNotificationSettingsSave)
		r.Post("/push/subscribe", app.handlePushSubscribe)
		r.Post("/push/unsubscribe", app.handlePushUnsubscribe)
		r.Post("/push/test", app.handlePushTest)
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
		// data only (the order book is the stored sweep snapshot,
		// never a fetch); they may queue market wants only, and
		// even that the admin can switch off live (market_anon.go).
		// The personal pieces (restock, watches) stay signed-in.
		r.With(anonPages).Get("/", app.handleMarket)
		r.With(anonPages).Get("/suggest", app.handleMarketSuggest)
		r.With(anonPages).Get("/history-fragment", app.handleMarketHistoryFragment)
		r.With(anonPages).Get("/trader-fragment", app.handleMarketTraderFragment)
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
		r.With(app.requireAuth).Post("/watch", app.handlePilotWatch)
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

	// The universe map: the page shell and its stored payload
	// (universe_map.go), public like the place pages it links to.
	// The killboard (killboard.go): core pages in the Intel section,
	// beside the Intel module's /intel routes.
	r.With(anonPages).Get("/intel/kills/", app.handleKillboard)
	r.With(anonPages).Get("/intel/kill/{id}/", app.handleKillPage)
	// Scans (scans.go): the paste box, and each reading at its
	// unlisted link. Anyone may paste, throttled like the pages.
	r.With(anonPages).Get("/intel/scan/", app.handleScanForm)
	r.With(anonPages).Post("/intel/scan/", app.handleScanSubmit)
	r.With(anonPages).Get("/intel/scan/{id}/", app.handleScanPage)
	// Intel channels (channels.go): the list explains itself to a
	// visitor; everything else is an account's.
	r.With(anonPages).Get("/intel/channels/", app.handleChannels)
	r.Group(func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Post("/intel/scan/{id}/post", app.handleScanPost)
		r.Get("/intel/feed/", app.handleIntelFeed)
		r.Get("/intel/feed.json", app.handleIntelFeedJSON)
		r.Post("/intel/channels/create", app.handleChannelCreate)
		r.Get("/intel/channels/{channelID}/", app.handleChannel)
		r.Get("/intel/channels/{channelID}/feed.json", app.handleChannelFeed)
		r.Post("/intel/channels/{channelID}/report", app.handleChannelReport)
		r.Post("/intel/channels/{channelID}/reports/{reportID}/delete", app.handleReportDelete)
		r.Post("/intel/channels/{channelID}/edit", app.handleChannelEdit)
		r.Post("/intel/channels/{channelID}/delete", app.handleChannelDelete)
		r.Post("/intel/channels/{channelID}/grants", app.handleGrantAdd)
		r.Post("/intel/channels/{channelID}/grants/remove", app.handleGrantRemove)
		r.Post("/intel/channels/pairing", app.handlePairingCodeCreate)
		r.Post("/intel/channels/tokens/{tokenID}/revoke", app.handleIntelTokenRevoke)
	})
	// EveSynapse Axon's API (intel_tokens.go): a posting
	// token, not a session, is the credential. The pairing
	// exchange has no credential at all, so it takes the
	// per-address anonymous throttle like the public pages.
	r.With(anonPages).Post("/api/intel/v1/pair", app.handleAPIPair)
	r.Route("/api/intel/v1", func(r chi.Router) {
		r.Use(app.intelTokenAuth)
		r.Get("/channels", app.handleAPIChannels)
		r.Post("/channels/{channelID}/lines", app.handleAPILines)
	})

	r.Route("/map", func(r chi.Router) {
		r.Use(anonPages)
		r.Get("/", app.handleMapPage)
		r.Get("/universe.json", app.handleUniverseJSON)
		r.Get("/layers.json", app.handleMapLayersJSON)
		r.Get("/scans.json", app.handleMapScans)
		r.Get("/system.json", app.handleMapSystem)
		r.Get("/intel.json", app.handleMapIntel)
		r.Get("/mine.json", app.handleMapMine)
		r.Get("/jump.json", app.handleMapJump)
		// A planned route to the game's autopilot (map_route.go);
		// it answers JSON, signed-in or not.
		r.Post("/route", app.handleMapRoute)
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

	r.Route("/killmails", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleKillmails)
	})

	// Public tools: the build planner, the PI planner and the Intel
	// pages render from static and public data, so visitors without
	// a session use them too (throttled per address; a build plan
	// can be large, so the planner shares the simulator's tighter
	// allowance). A signed-in visitor also gets their own characters'
	// stock, blueprints, skills and colonies. The Industry pages
	// (Blueprints, My Jobs, Mining Ledger) stay signed-in only.
	r.Route("/industry", func(r chi.Router) {
		r.With(app.requireAuth).Get("/", app.handleIndustry)
		r.With(app.requireAuth).Get("/jobs/", app.handleIndustryJobs)
		r.With(app.requireAuth).Get("/mining/", app.handleIndustryMining)
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

// SystemSecurity mirrors the fleet overview's location badge: the
// stored sde_systems row's security, rounded by secRound.
func (app *Application) SystemSecurity(ctx context.Context, systemID int64) (string, int, bool) {
	sys, err := app.queries.GetSDESystem(ctx, systemID)
	if err != nil {
		return "", 0, false
	}
	text, tier := secRound(sys.Security)
	return text, tier, true
}

// SystemRegion resolves a system's region name from the stored
// SDE rows (the system's region_id, then the region's name).
func (app *Application) SystemRegion(ctx context.Context, systemID int64) (string, bool) {
	sys, err := app.queries.GetSDESystem(ctx, systemID)
	if err != nil || sys.RegionID <= 0 {
		return "", false
	}
	reg, err := app.queries.GetSDERegion(ctx, sys.RegionID)
	if err != nil || reg.Name == "" {
		return "", false
	}
	return reg.Name, true
}

func (app *Application) ConstellationName(id int64) (string, bool) {
	return app.esi.CachedConstellationName(id)
}

func (app *Application) FormatFinish(ts string) string { return formatFinish(ts) }

func (app *Application) HumanizeEnum(s string) string { return humanizeEnum(s) }

func (app *Application) WarDetailWindow() int { return maxWarsShown }

func (app *Application) ModelConfig() module.ModelConfig {
	return module.ModelConfig{
		MetaKey:             app.cfg.MetaAIKey(),
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
		ClientSecret: app.cfg.DiscordClientSecret(),
		BotToken:     app.cfg.DiscordBotToken(),
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
	return app.pickOwner(ctx, r, offered)
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

func (app *Application) SkillsViewOpened(ctx context.Context, userID, characterID int64) {
	app.foresightOpened(ctx, userID, foresightSkillsView(characterID))
}

func (app *Application) LiveSkillqueue(queue esi.Skillqueue, now time.Time) esi.Skillqueue {
	return liveSkillqueue(queue, now)
}

func (app *Application) RenderFragmentStatus(w http.ResponseWriter, status int, page, define string, data any) {
	app.renderFragmentStatus(w, status, page, define, data)
}

func (app *Application) MarketPrices(ctx context.Context) (map[int64]esi.MarketPrice, error) {
	return app.marketPrices(ctx)
}

func (app *Application) MarketRegions() []module.RegionOption {
	out := make([]module.RegionOption, 0, len(marketRegions))
	for _, r := range marketRegions {
		out = append(out, module.RegionOption{ID: r.ID, Name: r.Name})
	}
	return out
}

func (app *Application) DefaultMarketRegion() int64 { return defaultMarketRegion }

func (app *Application) GlobalMarketRegion() int64 { return globalMarketRegion }

func (app *Application) MarketRegionName(id int64) (string, bool) { return marketRegionName(id) }

func (app *Application) QueueHistoryWant(ctx context.Context, regionID, typeID int64) error {
	return app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: regionID, TypeID: typeID, LastRequestedAt: time.Now().UTC(), Priority: wantOrbit,
	})
}

func (app *Application) AgeText(t time.Time) string { return ageText(time.Now(), t) }

func (app *Application) TypeNameOrID(ctx context.Context, typeID int64) string {
	return app.typeNameOrID(ctx, typeID)
}

func (app *Application) DoctrineChosenFor(ctx context.Context, userID int64, raw string) *module.NamedChoice {
	return app.doctrineChosenFor(ctx, userID, raw)
}

func (app *Application) FleetRoles() []string { return opFleetRoles }

func (app *Application) FitViewsPending(ctx context.Context, fitID int64) int64 {
	if isPrefetch(ctx) {
		return app.fitViews.peek(fitID)
	}
	return app.fitViews.count(fitID)
}

func (app *Application) PublicPageURL(path string) string { return app.publicPageURL(path) }

func (app *Application) HeadMeta(meta module.PageMeta) template.HTML {
	return module.RenderPageMeta(meta)
}

func (app *Application) RefreshPublicFitPages(ctx context.Context) int {
	return app.refreshPublicFitPages(ctx)
}

func (app *Application) FlushFitViews(ctx context.Context) { app.flushFitViews(ctx) }

func (app *Application) StoredSnapshot(ctx context.Context, ch db.Character, kind string, out any) error {
	return app.storedSnapshot(ctx, ch, kind, out)
}

func (app *Application) RenderFragment(w http.ResponseWriter, page, define string, data any) {
	app.renderFragment(w, page, define, data)
}

func (app *Application) SuggestTypes(ctx context.Context, query, pool string, limit int64) []module.SuggestItem {
	rows := app.suggestTypes(ctx, query, pool, limit)
	out := make([]module.SuggestItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, module.SuggestItem(r))
	}
	return out
}

func (app *Application) AccessToken(ctx context.Context, ch db.Character) (string, error) {
	return app.validAccessToken(ctx, ch)
}

func (app *Application) PostESI(ctx context.Context, token, path string, body, out any) error {
	return app.esi.PostJSONAuthed(ctx, token, path, body, out)
}

// PostESINoContent is module.Deps.PostESINoContent.
func (app *Application) PostESINoContent(ctx context.Context, token, path string, body any) error {
	return app.esi.PostAuthedNoContent(ctx, token, path, body)
}

func (app *Application) RefreshSnapshot(ctx context.Context, ch db.Character, kind string) error {
	return app.esi.FetchAndStoreSnapshot(ctx, ch, kind)
}

func (app *Application) ModuleEnabled(id string) bool { return moduleCompiledIn(id) }

func (app *Application) StorePublicFitPageNow(ctx context.Context, fitID int64) {
	app.storePublicFitPageNow(ctx, fitID)
}

func (app *Application) DoctrineFitDoc(ctx context.Context, userID, fitID int64) (*fitview.FitDoc, bool) {
	return app.doctrineFitDoc(ctx, userID, fitID)
}

func (app *Application) CorpSection(ctx context.Context, characterID int64, kind string, out any) module.CorpSectionState {
	return app.corpSection(ctx, characterID, kind, out)
}

func (app *Application) PickCorpPage(ctx context.Context, r *http.Request) (module.CorpPageSelection, bool, error) {
	return app.pickCorpPage(ctx, r)
}

func (app *Application) CorpStructureNames(ctx context.Context, characterID int64) map[int64]string {
	return app.corpStructureNames(ctx, characterID)
}

func (app *Application) CorpLocationTitle(ctx context.Context, locationID int64, structureNames map[int64]string) string {
	return app.corpLocationTitle(ctx, locationID, structureNames)
}

func (app *Application) LinkPlaceMemo(ctx context.Context, memo map[int64]module.PlaceRef, locationID int64, title string) module.PlaceRef {
	return app.linkPlaceMemo(ctx, memo, locationID, title)
}

func (app *Application) ResolvedStructureTitle(ctx context.Context, structureID int64) string {
	return app.resolvedStructureTitle(ctx, structureID)
}

func (app *Application) TxnCounterparty(ctx context.Context, id int64) (string, bool, bool) {
	return app.txnCounterparty(ctx, id)
}

func (app *Application) AssetLocations(ctx context.Context, items []esi.Asset, extraTitles, nameOverrides map[int64]string) any {
	return app.buildAssetLocationsWith(ctx, items, extraTitles, nameOverrides)
}

func (app *Application) OrderHealthText(myPrice float64, status string, stationBest, regionBest float64) (string, bool) {
	return orderHealthText(myPrice, status, stationBest, regionBest)
}

func (app *Application) CorpDisplayName(ctx context.Context, corpID int64) string {
	return app.corpDisplayName(ctx, corpID)
}

func (app *Application) CorpPermits(ctx context.Context, st standing.Standing, corp int64, permission string) bool {
	return app.corpPermits(ctx, st, corp, permission)
}

func (app *Application) PlayerCorps(ctx context.Context, userID int64) []int64 {
	return app.playerCorps(ctx, userID)
}

func (app *Application) PickCorporation(ctx context.Context, r *http.Request, offered []int64) (int64, []module.OwnerOption) {
	return app.pickCorporation(ctx, r, offered)
}

func (app *Application) OwnerCorporations(ctx context.Context, o standing.Owner) []int64 {
	return app.ownerCorporations(ctx, o)
}

func (app *Application) ResolvedAllianceName(ctx context.Context, allianceID int64) (string, bool) {
	return app.resolvedAllianceName(ctx, allianceID)
}

func (app *Application) SendEVEMail(ctx context.Context, from db.Character, recipientID int64, recipientType, subject, body string, approvedCost int) error {
	return app.sendEVEMail(ctx, from, recipientID, recipientType, subject, body, approvedCost)
}

func (app *Application) ESIRefusalDetail(err error) string { return esiRefusalDetail(err) }

func (app *Application) EconSection(ctx context.Context, characterID int64, kind string, out any) module.EconSectionState {
	return app.econSection(ctx, characterID, kind, out)
}

func (app *Application) OpCorps(ctx context.Context, userID int64, managedOnly bool) []int64 {
	return opCorps(app.opMembers(ctx, userID), managedOnly)
}

func (app *Application) RelinkURL(moduleID string, characterID int64) string {
	return relinkURL(moduleID, characterID)
}

func (app *Application) CharLinkHTML(ctx context.Context, characterID int64, name string) template.HTML {
	return charLink(app.viewerCharSet(ctx), characterID, name)
}

func (app *Application) NoteCharacterWant(ctx context.Context, characterID int64) {
	app.notePageWant(ctx, pageWantCharacter, characterID, 0)
}

func (app *Application) MarkMailReadLocally(ctx context.Context, characterID, mailID int64) {
	app.markMailReadLocally(ctx, characterID, mailID)
}

func (app *Application) AttachWalletGraph(ctx context.Context, userID, characterID int64, journal esi.WalletJournal, journalLoaded bool) any {
	return app.attachWalletGraph(ctx, userID, characterID, journal, journalLoaded)
}

func (app *Application) EmptyWalletGraph() any { return &walletGraphView{State: walletGraphEmpty} }

func (app *Application) WalletFlow(ctx context.Context, ch db.Character) any {
	return app.buildWalletFlow(ctx, app.loadCharSnaps(ctx, ch.UserID, []db.Character{ch}, []string{widgetWalletFlow}))
}

func (app *Application) AllianceDisplayName(ctx context.Context, allianceID int64) string {
	return app.allianceDisplayName(ctx, allianceID)
}

func (app *Application) ResolvedCharacterName(ctx context.Context, characterID int64) (string, bool) {
	return app.resolvedCharacterName(ctx, characterID)
}

func (app *Application) CharacterLabelPending(ctx context.Context, characterID int64) bool {
	return app.characterLabelPending(ctx, characterID)
}

func (app *Application) CachedCharacterName(id int64) (string, bool) {
	return app.esi.CachedCharacterName(id)
}

func (app *Application) CachedCorpName(id int64) (string, bool) { return app.esi.CachedCorpName(id) }

func (app *Application) LinkPlace(ctx context.Context, locationID int64, title string) module.PlaceRef {
	return app.linkPlace(ctx, locationID, title)
}

func (app *Application) EconLocationTitle(ctx context.Context, locationID int64) string {
	return app.econLocationTitle(ctx, locationID)
}

func (app *Application) RewriteSnapshot(ctx context.Context, characterID int64, kind string, edit func(payload string) (string, bool)) {
	app.rewriteSnapshot(ctx, characterID, kind, edit)
}

func (app *Application) PutESI(ctx context.Context, token, path string, body any) error {
	return app.esi.PutJSONAuthed(ctx, token, path, body)
}

func (app *Application) PostESIPublic(ctx context.Context, path string, body, out any) error {
	return app.esi.PostJSON(ctx, path, body, out)
}

func (app *Application) MarketFetchDue(ctx context.Context, kind string, gate time.Duration) bool {
	return app.marketFetchDue(ctx, kind, gate)
}

func (app *Application) RecordMarketFetchAt(ctx context.Context, kind, state, detail string, at time.Time) {
	app.recordMarketFetchAt(ctx, kind, state, detail, at)
}

func (app *Application) HistoryCandidates(ctx context.Context) ([]module.MarketKey, error) {
	return app.historyCandidates(ctx)
}

func (app *Application) FetchMarketHistory(ctx context.Context, key module.MarketKey) ([]esi.MarketHistoryDay, error) {
	return app.fetchMarketHistory(ctx, key)
}

func (app *Application) NoteStructureIDs(ctx context.Context, ids ...int64) {
	app.noteStructureIDs(ctx, ids...)
}

func (app *Application) WantCorporation(ctx context.Context, id, closeness int64) error {
	return app.wantCorporation(ctx, id, closeness)
}

func (app *Application) WantAlliance(ctx context.Context, id, closeness int64) error {
	return app.wantAlliance(ctx, id, closeness)
}

func (app *Application) ESIGet(ctx context.Context, path string, out any) error {
	return app.esi.Get(ctx, "", path, out)
}

func (app *Application) FetchStructure(ctx context.Context, ch db.Character, id int64) (esi.UniverseStructure, error) {
	return app.esi.FetchStructure(ctx, ch, id)
}

func (app *Application) LearnTypeName(id int64, name string) { app.esi.LearnTypeName(id, name) }

func (app *Application) ForgetTypeName(id int64) { app.esi.ForgetTypeName(id) }

func (app *Application) MissingTypeNames(limit int) []int64 { return app.esi.MissingTypeNames(limit) }

func (app *Application) NoteTypeNameMiss(id int64) { app.esi.NoteTypeNameMiss(id) }

func (app *Application) PublicOrigin() string { return app.cfg.publicOrigin() }

func (app *Application) RequireAdmin() func(http.Handler) http.Handler { return app.requireAdmin }

// FitBuilder is the fit foundation over this application's stored state.
func (app *Application) FitBuilder() *fitview.Builder { return app.fitBuilder() }

func (app *Application) fitBuilder() *fitview.Builder { return fitview.New(app.queries, app) }

// CachedGroupNames, LoadSnapshot, LocationTitle and SnapshotRoleMissing
// complete fitview.Env (the name caches, snapshots and price guide the
// foundation reads).
func (app *Application) CachedGroupNames(ctx context.Context, ids []int64) map[int64]string {
	return app.esi.CachedGroupNames(ctx, ids)
}

func (app *Application) LoadSnapshot(ctx context.Context, characterID int64, kind string, out any) bool {
	return app.loadCorpSnapshot(ctx, characterID, kind, out)
}

func (app *Application) LocationTitle(ctx context.Context, id int64, locType string) string {
	return app.locationTitle(ctx, id, locType)
}

func (app *Application) SnapshotRoleMissing(ctx context.Context, characterID int64, kind string) bool {
	state, _, found := app.corpKindState(ctx, characterID, kind)
	return found && state == fetchStateRoleMissing
}

func (app *Application) AnonPages() func(http.Handler) http.Handler {
	pageThrottle, _ := app.anonThrottles()
	return app.throttleAnon(pageThrottle)
}
