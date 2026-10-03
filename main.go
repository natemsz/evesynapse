// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
package main

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "golang.org/x/crypto/x509roots/fallback" // embedded Mozilla roots when no system cert store is visible (Android/Termux)
	_ "modernc.org/sqlite"                     // pure-Go SQLite driver, registers as "sqlite"

	db "evesynapse/internal/db/sqlc"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed schema/001_init.sql
var initSchema string

//go:embed schema/002_snapshots.sql
var snapshotsSchema string

//go:embed static
var staticFS embed.FS

type application struct {
	cfg      config
	sessions *scs.SessionManager
	queries  *db.Queries
	jwks     *jwksCache

	// tokenMu serializes access-token refreshes: CCP rotates refresh
	// tokens on every refresh, so two concurrent refreshes on the same
	// stored token could invalidate each other.
	tokenMu sync.Mutex

	// In-process cache of EVE type ID → name (backed by the
	// type_names table).
	typeNamesMu sync.RWMutex
	typeNames   map[int64]string

	// In-process cache of EVE type ID → group ID, populated as a
	// side effect of type fetches; feeds skill-sheet grouping
	// (skills.go).
	typeGroupsMu sync.RWMutex
	typeGroups   map[int64]int64

	// In-process cache of EVE group ID → name for skill-sheet
	// section headers; group names are stable public data.
	groupNamesMu sync.Mutex
	groupNames   map[int64]string

	// In-memory cache of built corporation views, keyed by
	// corporation ID; each entry expires with the ESI Expires
	// header of the response it was built from (corporation.go).
	corpMu    sync.Mutex
	corpCache map[int64]corpCacheEntry

	// In-process cache of station/system ID → name for asset
	// location titles; both are stable public data (assets.go).
	placeMu    sync.Mutex
	placeNames map[int64]string

	// In-memory cache of GET /markets/prices/ keyed by type ID,
	// refreshed once stale per the response Expires header
	// (market.go).
	pricesMu     sync.Mutex
	prices       map[int64]esiMarketPrice
	pricesExpiry time.Time

	// Character IDs flagged for first-in-line warm-up on the next
	// worker cycle (fresh SSO logins, Sync-page re-warm requests).
	priorityMu    sync.Mutex
	priorityChars map[int64]bool

	// The worker's user-visible status (Sync/Admin pages).
	workerMu sync.Mutex
	worker   workerStatus
}

func main() {
	// Android/Termux has no usable /etc/resolv.conf, so Go's resolver
	// falls back to a dead [::1]:53. Pin public DNS servers instead.
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

	cfg := loadConfig()

	dbConn, err := openDB(cfg.dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer dbConn.Close()

	sessionManager := scs.New()
	sessionManager.Store = sqlite3store.New(dbConn)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"
	// TODO(https): set Cookie.Secure = true once served over TLS.
	sessionManager.Cookie.Secure = false

	app := &application{
		cfg:           cfg,
		sessions:      sessionManager,
		queries:       db.New(dbConn),
		jwks:          &jwksCache{},
		typeNames:     make(map[int64]string),
		typeGroups:    make(map[int64]int64),
		groupNames:    make(map[int64]string),
		corpCache:     make(map[int64]corpCacheEntry),
		placeNames:    make(map[int64]string),
		prices:        make(map[int64]esiMarketPrice),
		priorityChars: make(map[int64]bool),
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go app.runWorker(workerCtx)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("evesynapse: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.addr, cfg.dbPath, cfg.ssoConfigured())
	if cfg.devLogin {
		// LOUD ON PURPOSE: /dev-login hands out a signed-in session to
		// anyone who asks. It exists for local development only.
		log.Printf("WARNING: DEV_LOGIN=1 — /dev-login is ENABLED. Never run like this in production.")
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
			log.Printf("evesynapse: WARNING EVE SSO discovery not reachable: %v", err)
			return
		}
		defer resp.Body.Close()
		log.Printf("evesynapse: EVE SSO discovery reachable (HTTP %d)", resp.StatusCode)
	}()
	log.Fatal(srv.ListenAndServe())
}

func (app *application) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(app.sessions.LoadAndSave)

	r.Get("/", app.handleHome)
	r.Get("/healthz", handleHealthz)

	// Embedded static assets (2013 wallpaper, stylesheet).
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		r.Handle("/static/*", http.StripPrefix("/static", http.FileServer(http.FS(sub))))
	}
	r.Get("/auth/eve", app.handleEVELogin)
	r.Get("/auth/callback", app.handleEVECallback)
	r.Get("/auth/logout", app.handleSignOut)

	// DEV-ONLY ROUTE — registered exclusively when DEV_LOGIN=1.
	// /dev-login flips the session "authenticated" flag without EVE SSO
	// so the admin can be exercised locally. It must stay off anywhere
	// near production; see handleDevLogin in auth.go.
	if app.cfg.devLogin {
		r.Get("/dev-login", app.handleDevLogin)
	}

	r.Route("/admin", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleAdmin)
	})

	r.Route("/corporations", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleCorporations)
	})

	r.Route("/assets", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleAssets)
	})

	r.Route("/market", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleMarket)
	})

	r.Route("/skills", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSkills)
	})

	r.Route("/sync", func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/", app.handleSync)
		r.Post("/warm", app.handleSyncWarm)
	})

	return r
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// requireAuth gates the admin on the session "authenticated" flag, which
// is only set by a completed EVE SSO login (or the dev-only /dev-login).
func (app *application) requireAuth(next http.Handler) http.Handler {
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

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		return nil, err
	}
	// The scs sqlite3store expects a sessions table; create it if needed.
	// (users/characters live in schema/001_init.sql and are managed via sqlc.)
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		expiry REAL NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(`CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expiry)`)
	if err != nil {
		return nil, err
	}
	// Apply the initial schema on first boot: a fresh DB_PATH has no
	// users/characters tables, and nothing else in the app creates them.
	// This is one-time creation of the sqlc-managed schema, not a
	// migration framework — later schema changes need new schema files.
	var usersTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&usersTables); err != nil {
		return nil, err
	}
	if usersTables == 0 {
		if err := applySchema(conn, initSchema); err != nil {
			return nil, err
		}
	}
	// Schema 002 (snapshot + type-name caches), applied the same
	// guarded way: only when its tables don't exist yet. On an
	// existing database this adds the new tables alongside the old
	// ones without disturbing them.
	var snapshotTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'character_snapshots'`).Scan(&snapshotTables); err != nil {
		return nil, err
	}
	if snapshotTables == 0 {
		if err := applySchema(conn, snapshotsSchema); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// applySchema executes a multi-statement DDL script statement by
// statement (the modernc driver Exec handles one statement at a time).
// Full-line -- comments are stripped first: they may contain ";" and
// would otherwise break the naive split.
func applySchema(conn *sql.DB, script string) error {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
