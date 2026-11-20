package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"ranvia.io/go"

	"evesynapse/internal/logging"
)

// The bot guard. With RANVIA_KEY set, Ranvia (ranvia.io) guards the
// site through its public Go loader, ranvia.io/go: the loader
// fetches the engine for the site key and decides every request on
// this server. Without a key, and while Ranvia has no engine yet (a
// first start with ranvia.io out of reach), the built-in guard in
// core_bot_guard.go runs. Either way the guard sits at the same
// place in the middleware chain and sees the same requests.

// botGuardMiddleware is the guard for this run.
func (app *Application) botGuardMiddleware() func(http.Handler) http.Handler {
	if app.cfg.ranviaKey == "" {
		return app.blockScrapers
	}
	g, err := ranvia.New(ranvia.Config{
		Key:             app.cfg.ranviaKey,
		ServiceURL:      app.cfg.ranviaURL,
		CacheDir:        app.cfg.ranviaCacheDir,
		Network:         app.networkOf,
		SignedIn:        app.hasVerifiedSession,
		VerifiedCrawler: app.verifiedCrawler,
		ClientIP:        clientAddr,
		RulesFile:       app.cfg.botGuardSettingsPath,
		LogRefusals:     app.cfg.ranviaLogRefusals,
		Logf:            logging.Infof,
		Fallback:        app.blockScrapers,
	})
	if err != nil {
		logging.Errorf("evesynapse: Ranvia did not start (%v); using the built-in guard", err)
		return app.blockScrapers
	}
	logging.Infof("evesynapse: bot guard: Ranvia")
	return func(next http.Handler) http.Handler {
		guarded := g.Middleware(next)
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

// verifiedCrawler reports a request from the search crawler its
// User-Agent names. Both guards share the one check: the Ranvia
// loader's reverse-and-forward DNS test, made on first use, so its
// cached verdicts carry over when Ranvia takes over from the built-in
// guard.
func (app *Application) verifiedCrawler(r *http.Request) bool {
	app.crawlerCheckOnce.Do(func() {
		if app.crawlerCheck == nil {
			app.crawlerCheck = ranvia.CrawlerCheck(clientAddr)
		}
	})
	return app.crawlerCheck(r)
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
