package app

import (
	"context"
	"net/http"
)

// A hover or focus prefetch (swap.js) is not a view. It sends
// X-Swap-Prefetch: 1, and a request carrying it changes nothing a
// view would: no view count, no typing-guess hit, no foresight open,
// no account or character activity, and the fetches its page asks for
// queue below viewed priority. When the pilot does open a prefetched
// page, swap.js sends the same address once more without the header,
// and that request counts as the view.

type prefetchCtxKey struct{}

// markPrefetch tags a prefetch request's context.
func markPrefetch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Swap-Prefetch") == "1" {
			r = r.WithContext(context.WithValue(r.Context(), prefetchCtxKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// isPrefetch reports whether the request is a prefetch, not a view.
func isPrefetch(ctx context.Context) bool {
	on, _ := ctx.Value(prefetchCtxKey{}).(bool)
	return on
}

// viewedPriority is the closeness for a fetch a page asks for: viewed
// for a page somebody opened, the speculative typing tier for one
// that was only prefetched.
func viewedPriority(ctx context.Context) int64 {
	if isPrefetch(ctx) {
		return wantTyping
	}
	return wantViewed
}
