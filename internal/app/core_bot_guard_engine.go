package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"evesynapse/internal/logging"
)

// A bot guard engine can replace the guard in core_bot_guard.go. An
// engine is linked into a build from outside this package and
// registers itself from an init function; a build without one runs
// the built-in guard. Either way the guard sits at the same place in
// the middleware chain and sees the same requests.

// BotGuardHost is what the application tells an engine.
type BotGuardHost struct {
	// SignedIn reports whether the request carries a verified,
	// signed-in session (checked against the session store; a
	// demo session does not count).
	SignedIn func(*http.Request) bool
	// Network names the visitor's network as the edge reports it,
	// "" when nothing trustworthy names one (see networkOf).
	Network func(*http.Request) string
	// VerifiedCrawler reports a search engine crawler whose address
	// checks out (core_bot_crawlers.go).
	VerifiedCrawler func(*http.Request) bool
	// Secret is the install's stable 32-byte signing secret for
	// the guard, stored in the database, so what the guard signs
	// survives a restart.
	Secret []byte
	// SettingsPath is BOT_GUARD_SETTINGS: a file of the engine's
	// own settings, kept on the server.
	SettingsPath string
	// Logf logs one line.
	Logf func(format string, args ...any)
}

// BotGuardEngine builds an engine's middleware for a host.
type BotGuardEngine func(BotGuardHost) (func(http.Handler) http.Handler, error)

var botGuardEngine BotGuardEngine

// RegisterBotGuardEngine installs an engine. A linked engine calls
// it from an init function, before the application starts.
func RegisterBotGuardEngine(e BotGuardEngine) {
	botGuardEngine = e
}

// botGuardMiddleware is the guard for this build: the registered
// engine when there is one and it starts, else blockScrapers. An
// engine that fails to start is logged and replaced by the
// built-in guard: the site keeps serving, guarded.
func (app *Application) botGuardMiddleware() func(http.Handler) http.Handler {
	if botGuardEngine == nil {
		return app.blockScrapers
	}
	mw, err := botGuardEngine(app.botGuardHost())
	if err != nil {
		logging.Errorf("evesynapse: bot guard engine did not start (%v); using the built-in guard", err)
		return app.blockScrapers
	}
	logging.Infof("evesynapse: bot guard: linked engine")
	return func(next http.Handler) http.Handler {
		guarded := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The dev entrypoint turns the guard off during route
			// setup (DisableBotGuard), after this was built.
			if app.botGuardOff {
				next.ServeHTTP(w, r)
				return
			}
			guarded.ServeHTTP(w, r)
		})
	}
}

func (app *Application) botGuardHost() BotGuardHost {
	return BotGuardHost{
		SignedIn:        app.hasVerifiedSession,
		Network:         app.networkOf,
		VerifiedCrawler: app.crawlerVerifier().verified,
		Secret:          app.botGuardSecret(),
		SettingsPath:    app.cfg.botGuardSettingsPath,
		Logf:            logging.Infof,
	}
}

// botGuardSecret is the secret New loaded from the database, or a
// random one for this process when there is none (the test rig, or
// a database that refused).
func (app *Application) botGuardSecret() []byte {
	app.botGuardKeyOnce.Do(func() {
		if len(app.botGuardKey) == 0 {
			key := make([]byte, 32)
			_, _ = rand.Read(key)
			app.botGuardKey = key
		}
	})
	return app.botGuardKey
}

// loadBotGuardSecret reads the install's guard secret, creating and
// storing one on first use.
func (app *Application) loadBotGuardSecret(ctx context.Context) {
	stored, err := app.queries.GetBotGuardSecret(ctx)
	if err != nil || stored == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			logging.Errorf("evesynapse: bot guard secret: %v", err)
			return
		}
		if err := app.queries.InsertBotGuardSecret(ctx, hex.EncodeToString(buf)); err != nil {
			logging.Errorf("evesynapse: bot guard secret: %v; signing with a key for this run only", err)
			return
		}
		if stored, err = app.queries.GetBotGuardSecret(ctx); err != nil {
			logging.Errorf("evesynapse: bot guard secret: %v; signing with a key for this run only", err)
			return
		}
	}
	key, err := hex.DecodeString(stored)
	if err != nil || len(key) < 32 {
		logging.Errorf("evesynapse: bot guard secret is malformed; signing with a key for this run only")
		return
	}
	app.botGuardKey = key
}

// networkOf names the visitor's network as the edge reports it:
// the ASN header an admin configures (BOT_GUARD_ASN_HEADER, filled
// by a Cloudflare request header rule from ip.src.asnum), else
// Cloudflare's CF-IPCountry. Both are read under the trust
// clientAddr gives CF-Connecting-IP: a loopback peer while
// TRUST_CF_CONNECTING_IP holds. A value of the wrong shape counts
// as no network. Empty when nothing names one.
func (app *Application) networkOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if peer := net.ParseIP(host); peer == nil || !peer.IsLoopback() || !trustCFConnectingIP.Load() {
		return ""
	}
	if name := app.cfg.botGuardASNHeader; name != "" {
		if asn := strings.TrimSpace(r.Header.Get(name)); asn != "" && len(asn) <= 10 && allDigits(asn) {
			return "as" + asn
		}
	}
	if cc := strings.TrimSpace(r.Header.Get("CF-IPCountry")); len(cc) == 2 && isAlnum(cc) {
		return strings.ToUpper(cc)
	}
	return ""
}

func isAlnum(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
