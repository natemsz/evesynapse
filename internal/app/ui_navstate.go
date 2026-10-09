package app

import (
	"context"
	"net/http"
)

// navCookie holds the reader's navigation layout (expanded, rail or
// hidden). The choice has always lived in localStorage, applied by a
// script in the page head before first paint. A script can be late
// (a proxy that defers scripts, a slow connection), and then the page
// paints expanded and snaps to the saved layout. The same choice is
// now also kept in this cookie, so the server renders
// <html data-nav="rail"> itself and the page arrives in its saved
// layout with no script involved. app.js and boot.js write the
// cookie whenever the layout changes (and once for readers whose
// choice predates it).
const navCookie = "evesynapse-nav"

type navStateKey struct{}

// navStateMiddleware reads the cookie into the request context. Only
// the two non-default layouts count: "expanded" is the page's own
// default, and anything else in the cookie is ignored.
func navStateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(navCookie); err == nil && (c.Value == "rail" || c.Value == "hidden") {
			r = r.WithContext(context.WithValue(r.Context(), navStateKey{}, c.Value))
		}
		next.ServeHTTP(w, r)
	})
}

// navStateFrom returns the saved layout the request carried, or "".
func navStateFrom(ctx context.Context) string {
	state, _ := ctx.Value(navStateKey{}).(string)
	return state
}
