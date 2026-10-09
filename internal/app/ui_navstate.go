package app

import (
	"context"
	"net/http"
)

// navCookie holds the reader's navigation layout (expanded, rail or
// hidden). The choice lives in localStorage, applied by a script in the
// page head before first paint; a late script would paint expanded and
// then snap to the saved layout. The cookie carries the same choice, so
// the server renders <html data-nav="rail"> itself. app.js and boot.js
// write it whenever the layout changes.
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
