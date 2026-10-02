// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
package main

import (
	"context"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers as "sqlite"

	db "evesynapse/internal/db/sqlc"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed schema/001_init.sql
var initSchema string

type application struct {
	cfg      config
	sessions *scs.SessionManager
	queries  *db.Queries
	jwks     *jwksCache
}

func main() {
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
		cfg:      cfg,
		sessions: sessionManager,
		queries:  db.New(dbConn),
		jwks:     &jwksCache{},
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go runWorker(workerCtx)

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
