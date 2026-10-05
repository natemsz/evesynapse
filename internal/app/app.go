// Package app is the EveSynapse web application: configuration,
// EVE SSO auth and sessions, the HTTP routes and page handlers, the
// background refresh worker, and DB bootstrap. The two entrypoints
// (cmd/evesynapse for release, cmd/evesynapse-dev for local dev)
// only wire configuration into New and serve Handler.
package app

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"
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
	"evesynapse/internal/esi"
)

//go:embed templates/*.html
var templatesFS embed.FS

// The collapsed Postgres baseline (schema_pg/001): the one-time
// fold of the 35 SQLite migrations, applied by openDB on a fresh
// database. The SQLite files stay in schema/ for the rollback
// binary and the -migrate-pg source reader; new schema changes
// land as new numbered files here.

//go:embed schema_pg/001_baseline.sql
var pgBaselineSchema string

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

	// db, pool, and stopWorker/workerDone are the resources Close
	// releases: it cancels the worker, waits for workerDone to
	// signal the worker has fully stopped, and only then closes
	// the handles.
	db         *sql.DB
	pool       *pgxpool.Pool
	stopWorker context.CancelFunc
	workerDone chan struct{}
	workerCtx  context.Context // the worker's context, captured in New for background jobs (SDE import)

	// tokenMu serializes access-token refreshes: CCP rotates refresh
	// tokens on every refresh, so two concurrent refreshes on the same
	// stored token could invalidate each other.
	tokenMu sync.Mutex

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
	// header of the response it was built from (corporation.go).
	corpMu    sync.Mutex
	corpCache map[int64]corpCacheEntry

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

	// The worker-stored market guide (guide_prices.go), cached
	// in memory under its fetched_at stamp so renders reload
	// only when a refresh has landed.
	storedPricesMu    sync.Mutex
	storedPricesCache map[int64]esi.MarketPrice
	storedPricesStamp string

	// Character IDs flagged for first-in-line warm-up on the next
	// worker cycle (fresh SSO logins, Sync-page re-warm requests).
	priorityMu    sync.Mutex
	priorityChars map[int64]bool

	// The worker's user-visible status (Sync/Admin pages).
	workerMu sync.Mutex
	worker   workerStatus

	// The SDE importer's user-visible status (Sync page), guarded
	// the same way as the worker status. See sde.go.
	sdeMu sync.Mutex
	sde   sdeStatus

	// Current-page wants (pagewants.go): what each signed-in page
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

	dbConn, pool, err := openDB(context.Background(), cfg.databaseURL)
	if err != nil {
		return nil, err
	}

	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = sessionLifetime
	sessionManager.Cookie.Name = "evesynapse_session"
	// TODO(https): set Cookie.Secure = true once served over TLS.
	sessionManager.Cookie.Secure = false

	app := &Application{
		cfg:           cfg,
		sessions:      sessionManager,
		queries:       db.New(dbConn),
		jwks:          &jwksCache{},
		db:            dbConn,
		pool:          pool,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
		pageWants:     make(map[string]map[string]pageWant),
	}
	app.esi = esi.New(loginHTTPClient, app.queries, app.validAccessToken)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	app.stopWorker = stopWorker
	app.workerCtx = workerCtx
	app.workerDone = make(chan struct{})
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
			log.Printf("evesynapse: WARNING EVE SSO discovery not reachable: %v", err)
			return
		}
		defer resp.Body.Close()
		log.Printf("evesynapse: EVE SSO discovery reachable (HTTP %d)", resp.StatusCode)
	}()

	return app, nil
}

// Close shuts the application down in dependency order: cancel
// the worker's context, wait for the worker (and every pass it
// runs) to fully stop, and only then close the database handle
// and the session pool. Closing the handle first is what used to
// flood the log with "sql: database is closed" on every restart:
// the worker passes still in flight kept querying a dead handle.
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
// during route setup, exactly where dev-only routes used to be
// registered inline; production routes are identical with or
// without hooks.
func (app *Application) Handler(hooks ...RouteHook) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(app.sessions.LoadAndSave)
	r.Use(app.slideSession)
	r.Use(app.pageWantScopeMiddleware)

	r.Get("/", app.handleHome)
	r.Route("/home", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Post("/layout", app.handleHomeLayout)
		r.Post("/widget-config", app.handleWidgetConfig)
	})
	r.Get("/healthz", handleHealthz)
	r.Get("/favicon.ico", handleFavicon)

	// Embedded static assets (2013 wallpaper, stylesheet).
	// Font files never change at a given URL, so they cache
	// forever; everything else revalidates normally.
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		fileServer := http.FileServer(http.FS(sub))
		r.Handle("/static/*", http.StripPrefix("/static", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasPrefix(req.URL.Path, "/fonts/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, req)
		})))
	}
	r.Get("/auth/eve", app.handleEVELogin)
	r.Get("/auth/callback", app.handleEVECallback)
	r.Get("/auth/logout", app.handleSignOut)

	// Route hooks (dev entrypoint only) mount here.
	for _, hook := range hooks {
		hook(r, app)
	}

	r.Route("/admin", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleAdmin)
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
	})

	r.Route("/planets", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handlePlanets)
	})

	r.Route("/mail", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleMail)
	})

	r.Route("/calendar", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCalendar)
	})

	r.Route("/contacts", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleContacts)
		r.Get("/name-fragment", app.handleContactNameFragment)
	})

	// Generic entity-label live regions (pagewants.go): a pending
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
		r.Get("/plans/fit", app.handleSkillPlanFitPreview)
		r.Post("/plans/create", app.handleSkillPlanCreate)
		r.Post("/plans/item-add", app.handleSkillPlanItemAdd)
		r.Post("/plans/item-remove", app.handleSkillPlanItemRemove)
		r.Post("/plans/item-move", app.handleSkillPlanItemMove)
		r.Post("/plans/delete", app.handleSkillPlanDelete)
		r.Post("/plans/from-template", app.handleSkillPlanFromTemplate)
		r.Post("/plans/from-fit", app.handleSkillPlanFromFit)
	})

	r.Route("/character", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCharacter)
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
		r.Post("/simulate/", app.handleFitSimulate)
		r.Get("/picker.json", app.handleFitPickerJSON)
		r.Post("/save/", app.handleFitSave)
		r.Post("/delete/", app.handleFitDelete)
		r.Post("/import/", app.handleFitImport)
		r.Post("/export/", app.handleFitExport)
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
		r.Use(app.requireAuth)
		r.Get("/", app.handleSync)
		r.Get("/page-status", app.handlePageSyncStatus)
		r.Post("/warm", app.handleSyncWarm)
		r.Post("/sde", app.handleSyncSDE)
	})

	return r
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleFavicon serves the app icon. Browsers request
// /favicon.ico by default even though the icon is SVG (the modern
// format, also linked from base.html as /static/favicon.svg);
// serving the SVG here with its real content type keeps the
// request from 404ing in the logs.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	icon, err := fs.ReadFile(staticFS, "static/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800")
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

// requestLogger logs method, path, status and duration. The query string
// is deliberately never logged: /auth/callback carries an OAuth
// authorization code and error details that don't belong in logs.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, ww.Status(),
			time.Since(start).Round(time.Millisecond))
	})
}
