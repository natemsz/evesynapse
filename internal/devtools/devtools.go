// Package devtools holds the development-only HTTP surface of
// EveSynapse. It is imported solely by the dev entrypoint
// (cmd/evesynapse-dev); the release entrypoint (cmd/evesynapse)
// never imports it, so the release binary contains none of this
// code and /dev-login does not exist there.
package devtools

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app"
)

// Register mounts the dev-only routes on the application's router.
// It matches app.RouteHook, so the dev entrypoint passes it to
// Application.Handler. Today it registers:
//
//	GET /dev-login — flip the session "authenticated" flag without
//	EVE SSO, so the admin can be exercised locally.
//
// The route is registered ONLY when the config has DEV_LOGIN=1;
// otherwise Register is a no-op. The sign-in itself is delegated
// to the application (DevSignIn) so this package depends only on
// exported application surface.
//
// !!! /dev-login hands a signed-in session to anyone who asks.
// NEVER enable it on a deployment anyone else can reach. !!!
func Register(r chi.Router, a *app.Application) {
	if !app.DevLoginEnabled(a) {
		return
	}
	// LOUD ON PURPOSE: same warning the old inline route printed.
	log.Printf("WARNING: DEV_LOGIN=1 — /dev-login is ENABLED. Never run like this in production.")
	r.Get("/dev-login", func(w http.ResponseWriter, r *http.Request) {
		app.DevSignIn(r.Context(), a)
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
	})
}
