// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
//
// This is the modernization skeleton: chi router + scs sessions (SQLite
// store) + html/template rendering, with the ESI/SSO plumbing stubbed.
package main

import (
	"context"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers as "sqlite"
)

//go:embed templates/*.html
var templatesFS embed.FS

type config struct {
	addr            string // listen address
	dbPath          string // SQLite database file
	eveClientID     string // EVE SSO application client ID
	eveClientSecret string // EVE SSO application client secret
	sessionKey      string // reserved for cookie signing when SSO lands
}

type application struct {
	cfg      config
	sessions *scs.SessionManager
}

func main() {
	cfg := config{
		addr:            getenvDefault("ADDR", ":8080"),
		dbPath:          getenvDefault("DB_PATH", "evesynapse.db"),
		eveClientID:     os.Getenv("EVE_CLIENT_ID"),
		eveClientSecret: os.Getenv("EVE_CLIENT_SECRET"),
		sessionKey:      os.Getenv("SESSION_KEY"),
	}

	db, err := openDB(cfg.dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sessionManager := scs.New()
	sessionManager.Store = sqlite3store.New(db)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"
	// TODO(https): set Cookie.Secure = true once served over TLS.
	sessionManager.Cookie.Secure = false

	app := &application{cfg: cfg, sessions: sessionManager}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go runWorker(workerCtx)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("evesynapse: listening on %s (db: %s, EVE SSO configured: %t)",
		cfg.addr, cfg.dbPath, cfg.eveClientID != "")
	log.Fatal(srv.ListenAndServe())
}

func (app *application) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(app.sessions.LoadAndSave)

	r.Get("/", app.handleHome)
	r.Get("/healthz", handleHealthz)
	r.Get("/auth/eve", app.handleEVELoginStub)

	// Dev-only convenience: flips the session "authenticated" flag so the
	// admin placeholder can be exercised before EVE SSO lands. Remove
	// before this ever faces the internet.
	r.Get("/dev-login", app.handleDevLogin)

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

// requireAuth is a stub middleware: it only checks the "authenticated"
// session flag. It will be replaced by real EVE SSO session checks.
func (app *application) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !app.sessions.GetBool(r.Context(), "authenticated") {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	// The scs sqlite3store expects a sessions table; create it if needed.
	// (users/characters live in schema/001_init.sql and are managed via sqlc.)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		expiry REAL NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expiry)`)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
