package app

// ---------------------------------------------------------------------------
// Browser-facing protections applied to every response and request:
// the session cookie's attributes, the security headers, and the
// guard against cross-site state-changing requests.
// ---------------------------------------------------------------------------

import (
	"net/http"

	"github.com/alexedwards/scs/v2"

	"evesynapse/internal/logging"
)

// newSessionManager builds the session manager the app runs on.
// The cookie is HTTP-only and SameSite=Lax, and marked Secure
// whenever the site's public address is https — so a session
// cookie issued over TLS is never sent in the clear.
func newSessionManager(store scs.Store, cfg Config) *scs.SessionManager {
	sm := scs.New()
	sm.Store = store
	sm.Lifetime = sessionLifetime
	sm.Cookie.Name = "evesynapse_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Secure = cfg.servedOverTLS()
	return sm
}

// warnIfServedInTheClear logs a startup warning when the site's
// public address is plain http on something other than this
// machine: sign-in cookies, and every page of character data,
// would cross the network unencrypted.
func warnIfServedInTheClear(cfg Config) {
	if cfg.servedOverTLS() || cfg.publicHostIsLocal() {
		return
	}
	if origin := cfg.publicOrigin(); origin != "" {
		logging.Warnf("evesynapse: %s is plain http: sign-in cookies and character data travel unencrypted. Serve the app over HTTPS (see \"HTTPS\" in the README) and set EVE_CALLBACK_URL to the https address", origin)
	}
}

// contentSecurityPolicy limits what a page may load and where it
// may send data: everything comes from the app itself (fonts
// included), plus images from CCP's image server (portraits, logos,
// item icons). Pages can not be framed, cannot post forms elsewhere,
// and cannot have their base address moved.
//
// No inline scripts, styles, or event handlers anywhere: every
// script is a versioned file under /static/ (boot.js, app.js,
// fit.js), and styling is the stylesheet plus CSSOM writes from
// those files — so neither script-src nor style-src needs
// 'unsafe-inline'. Keep it that way: an inline <script> or style
// attribute added to a template will silently not run.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data: https://images.evetech.net; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders sets the response headers that harden every
// page: no MIME sniffing, no framing, no referrer leaking to other
// sites (same-origin referrers are kept — the character switcher
// returns to the page it came from), the content security policy,
// and HSTS when the site is served over https.
func (app *Application) securityHeaders(next http.Handler) http.Handler {
	hsts := app.cfg.servedOverTLS()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// crossOriginGuard rejects state-changing requests (POST and
// friends) that a browser sends from another site: the classic
// cross-site request forgery, where a page elsewhere submits a form
// to this app using the visitor's session. It is the standard
// library's check — the browser's own Sec-Fetch-Site header, or
// Origin compared with Host — and needs no tokens in forms.
// Requests that carry neither header (scripts, curl, tests) are not
// browser requests and pass.
//
// Until now the only thing standing between another site and the
// app's POST handlers was the session cookie's SameSite default.
func (app *Application) crossOriginGuard() func(http.Handler) http.Handler {
	guard := http.NewCrossOriginProtection()
	// Behind a reverse proxy that rewrites the Host header, the
	// browser's Origin no longer matches it. The site's own public
	// address is same-origin by definition, so it is always allowed.
	if origin := app.cfg.publicOrigin(); origin != "" {
		if err := guard.AddTrustedOrigin(origin); err != nil {
			logging.Errorf("evesynapse: cross-origin guard: cannot trust %q: %v", origin, err)
		}
	}
	return guard.Handler
}
