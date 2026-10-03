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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "golang.org/x/crypto/x509roots/fallback" // embedded Mozilla roots when no system cert store is visible (Android/Termux)
	_ "modernc.org/sqlite"                     // pure-Go SQLite driver, registers as "sqlite"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed schema/001_init.sql
var initSchema string

//go:embed schema/002_snapshots.sql
var snapshotsSchema string

//go:embed schema/003_sde.sql
var sdeSchema string

//go:embed schema/004_module_sweep.sql
var moduleSweepSchema string

//go:embed schema/005_corp.sql
var corpSchema string

//go:embed schema/006_economy.sql
var economySchema string

//go:embed schema/007_intel.sql
var intelSchema string

//go:embed schema/008_marketable.sql
var marketableSchema string

//go:embed schema/009_character_foundation.sql
var characterFoundationSchema string

//go:embed schema/010_home_overview.sql
var homeLayoutSchema string

//go:embed static
var staticFS embed.FS

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

	// db and stopWorker are the resources Close releases.
	db         *sql.DB
	stopWorker context.CancelFunc
	workerCtx  context.Context // the worker's context, captured in New for background jobs (SDE import)

	// tokenMu serializes access-token refreshes: CCP rotates refresh
	// tokens on every refresh, so two concurrent refreshes on the same
	// stored token could invalidate each other.
	tokenMu sync.Mutex

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
}

// New opens the database (applying the embedded schemas on first
// boot), builds the application, and starts the background worker
// and the one-shot SSO reachability probe. Call Close to release
// the database and stop the worker.
func New(cfg Config) (*Application, error) {
	installResolver()

	dbConn, err := openDB(cfg.dbPath)
	if err != nil {
		return nil, err
	}

	sessionManager := scs.New()
	sessionManager.Store = sqlite3store.New(dbConn)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"
	// TODO(https): set Cookie.Secure = true once served over TLS.
	sessionManager.Cookie.Secure = false

	app := &Application{
		cfg:           cfg,
		sessions:      sessionManager,
		queries:       db.New(dbConn),
		jwks:          &jwksCache{},
		db:            dbConn,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
	app.esi = esi.New(loginHTTPClient, app.queries, app.validAccessToken)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	app.stopWorker = stopWorker
	app.workerCtx = workerCtx
	go app.runWorker(workerCtx)

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

// Close stops the background worker and closes the database.
func (app *Application) Close() {
	if app.stopWorker != nil {
		app.stopWorker()
	}
	if app.db != nil {
		app.db.Close()
	}
}

// installResolver pins public DNS servers: Android/Termux has no
// usable /etc/resolv.conf, so Go's resolver falls back to a dead
// [::1]:53. (netdns.go's init installs a second, fallback-aware
// resolver; this one runs later and wins, exactly as when both
// lived in package main.)
func installResolver() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			for _, dns := range []string{"8.8.8.8:53", "1.1.1.1:53"} {
				if conn, err := d.DialContext(ctx, network, dns); err == nil {
					return conn, nil
				}
			}
			return d.DialContext(ctx, network, "9.9.9.9:53")
		},
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

	r.Get("/", app.handleHome)
	r.Route("/home", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Post("/layout", app.handleHomeLayout)
	})
	r.Get("/healthz", handleHealthz)
	r.Get("/favicon.ico", handleFavicon)

	// Embedded static assets (2013 wallpaper, stylesheet).
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		r.Handle("/static/*", http.StripPrefix("/static", http.FileServer(http.FS(sub))))
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

	r.Route("/market", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleMarket)
		r.Get("/suggest", app.handleMarketSuggest)
	})

	r.Route("/items", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleItems)
		r.Get("/category/{categoryID}/", app.handleItemsCategory)
		r.Get("/group/{groupID}/", app.handleItemsGroup)
	})

	r.Route("/skills", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSkills)
	})

	r.Route("/character", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCharacter)
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
	})

	r.Route("/killmails", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleKillmails)
	})

	r.Route("/wallet", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleWallet)
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

	r.Route("/intel", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/wars/", app.handleIntelWars)
		r.Get("/incursions/", app.handleIntelIncursions)
		r.Get("/fw/", app.handleIntelFW)
	})

	r.Route("/sync", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSync)
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
