package app

import (
	"context"
	"net/http"
	"strconv"
)

// navCookie holds the reader's navigation layout (rail, hidden or
// expanded). The choice lives in localStorage, applied by a script in the
// page head before first paint; a late script would paint the default and
// then snap to the saved layout. The cookie carries the same choice, so
// the server renders <html data-nav="rail"> itself. app.js and boot.js
// write it whenever the layout changes.
const navCookie = "evesynapse-nav"

type navStateKey struct{}

// navStateMiddleware reads the cookie into the request context.
// The desktop default is the icon rail: an absent or unknown
// cookie renders the rail, while a saved rail, hidden or
// expanded choice always wins.
func navStateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "rail"
		if c, err := r.Cookie(navCookie); err == nil {
			switch c.Value {
			case "rail", "hidden", "expanded":
				state = c.Value
			}
		}
		ctx := context.WithValue(r.Context(), navStateKey{}, state)
		ctx = withHiddenNotices(ctx, r)
		if id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); err == nil && id > 0 {
			ctx = context.WithValue(ctx, navCharacterKey{}, id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// navCharacterKey carries the character a page was asked for
// (?character=), so the links to the other pages of its section keep
// it: switching tabs keeps the pilot, or the corporation, in view.
type navCharacterKey struct{}

// navCharacterFrom is the request's ?character=, 0 when none.
func navCharacterFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(navCharacterKey{}).(int64)
	return id
}

// navStateFrom returns the layout the request carries: the saved
// choice, or the rail default.
func navStateFrom(ctx context.Context) string {
	state, _ := ctx.Value(navStateKey{}).(string)
	if state == "" {
		return "rail"
	}
	return state
}
