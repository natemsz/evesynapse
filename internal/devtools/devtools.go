// Package devtools holds the development-only HTTP surface of
// EveSynapse. It is imported solely by the dev entrypoint
// (cmd/evesynapse-dev); the release entrypoint (cmd/evesynapse)
// never imports it, so the release binary contains none of this
// code and /dev-login does not exist there.
package devtools

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app"
	"evesynapse/internal/logging"
)

const devAssetsPrefix = "/__dev-cdn"

// Prepare turns off the bot guard, Ranvia included, which otherwise
// refuses headless browsers such as Playwright. When DEV_ASSETS_DIR
// points at a local mirror of the CDN static tree, it also points the
// app's CDN stylesheet at the dev server's /__dev-cdn/static/ mount so
// every per-scheme planet and nebula wallpaper can be previewed locally.
func Prepare(a *app.Application) {
	app.DisableBotGuard(a)
	logging.Warnf("dev server: the bot guard is OFF. Never run this binary in production.")
	if dir := strings.TrimSpace(os.Getenv("DEV_ASSETS_DIR")); dir != "" {
		app.SetDevAssetsBase(devAssetsPrefix)
		logging.Warnf("DEV_ASSETS_DIR: serving local CDN mirror from %s at %s/static/", dir, devAssetsPrefix)
	}
}

// Register mounts the dev-only routes on the application's router.
// It matches app.RouteHook, so the dev entrypoint passes it to
// Application.Handler. It registers:
//
//	GET /dev-login, flip the session "authenticated" flag without
//	EVE SSO, so the signed-in pages can be exercised locally. It
//	lands on the home page: the dev session has no account, so it
//	is never an admin.
//
//	GET /__dev-cdn/static/*, serve a local CDN asset directory when
//	DEV_ASSETS_DIR is set, falling back to /static/* for files not
//	present in the mirror.
//
// !!! /dev-login hands a signed-in session to anyone who asks.
// NEVER enable it on a deployment anyone else can reach. !!!
func Register(r chi.Router, a *app.Application) {
	if dir := strings.TrimSpace(os.Getenv("DEV_ASSETS_DIR")); dir != "" {
		staticRoot := filepath.Join(dir, "static")
		dirFS := os.DirFS(staticRoot)
		r.Get(devAssetsPrefix+"/static/*", func(w http.ResponseWriter, req *http.Request) {
			rel := strings.TrimPrefix(path.Clean("/"+chi.URLParam(req, "*")), "/")
			if rel != "" && fs.ValidPath(rel) {
				if info, err := fs.Stat(dirFS, rel); err == nil && !info.IsDir() {
					switch strings.ToLower(path.Ext(rel)) {
					case ".avif":
						w.Header().Set("Content-Type", "image/avif")
					case ".jxl":
						w.Header().Set("Content-Type", "image/jxl")
					case ".webp":
						w.Header().Set("Content-Type", "image/webp")
					case ".jpg", ".jpeg":
						w.Header().Set("Content-Type", "image/jpeg")
					}
					w.Header().Set("Cache-Control", "no-cache")
					http.ServeFileFS(w, req, dirFS, rel)
					return
				}
			}
			http.Redirect(w, req, "/static/"+rel, http.StatusTemporaryRedirect)
		})
	}
	if !app.DevLoginEnabled(a) {
		return
	}
	// Loud on purpose.
	logging.Warnf("DEV_LOGIN=1: /dev-login is ENABLED. Never run like this in production.")
	r.Get("/dev-login", func(w http.ResponseWriter, r *http.Request) {
		app.DevSignIn(r.Context(), a)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}
