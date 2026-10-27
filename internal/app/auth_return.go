package app

import (
	"context"
	"net/http"
	"strings"
)

// sessionReturnTo is the page a signed-out visitor asked for, kept
// while they sign in: requireAuth stores it, and the sign-in's end
// sends them there instead of the home page. Axon's site panel opens
// the intel workbench this way, and lands on it after EVE sign-in.
const sessionReturnTo = "return_to"

// rememberReturnTo keeps a signed-out GET's own page for after
// sign-in. Only a page a person navigated to counts (a stale tab's
// poll of a .json endpoint must not become the landing page), and
// only a local path is kept, never another site.
func (app *Application) rememberReturnTo(r *http.Request) {
	if r.Method != http.MethodGet || wantsJSON(r) || strings.HasSuffix(r.URL.Path, ".json") ||
		!strings.Contains(r.Header.Get("Accept"), "text/html") {
		return
	}
	if p := r.URL.RequestURI(); safeReturnPath(p) {
		app.sessions.Put(r.Context(), sessionReturnTo, p)
	}
}

// takeReturnTo is where a finished sign-in goes: the page kept for
// it, once, or the home page.
func (app *Application) takeReturnTo(ctx context.Context) string {
	p := app.sessions.PopString(ctx, sessionReturnTo)
	if !safeReturnPath(p) {
		return "/"
	}
	return p
}

// safeReturnPath is a path on this site: rooted, and not a
// protocol-relative or backslashed address a browser would take
// elsewhere.
func safeReturnPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\")
}
