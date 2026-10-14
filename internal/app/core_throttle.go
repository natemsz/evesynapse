package app

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Anonymous throttle: how often one client address may call the
// routes that serve visitors without a session and cost real work
// (the public fit browser and pages, the simulator, ship and item
// pages). Signed-in requests are never counted.
//
// What it buys: one host (a scraper, a loop, a broken client) can no
// longer keep the simulator and the public pages busy for everyone.
// What it does not: many hosts, each under the limit, still get
// through, and visitors behind one shared address (a carrier NAT, an
// office) share one allowance. Each address costs a few dozen bytes
// of memory while it is active.
// ---------------------------------------------------------------------------

// The allowances. Pages: a burst of 60, then two a second. The
// simulator, which runs the fitting engine on every edit: a burst of
// 30, then one a second.
const (
	anonPagesPerMinute = 120
	anonPagesBurst     = 60
	anonSimPerMinute   = 60
	anonSimBurst       = 30
)

// anonThrottle is a token bucket per client address.
type anonThrottle struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	clients   map[string]*throttleBucket
	lastSweep time.Time
	now       func() time.Time // tests set the clock
}

type throttleBucket struct {
	tokens float64
	at     time.Time
}

func newAnonThrottle(perMinute, burst int) *anonThrottle {
	return &anonThrottle{
		perSecond: float64(perMinute) / 60,
		burst:     float64(burst),
		clients:   make(map[string]*throttleBucket),
		now:       time.Now,
	}
}

// allow spends one token of key's bucket. When none is left it
// reports how long until one is.
func (t *anonThrottle) allow(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	b, ok := t.clients[key]
	if !ok {
		b = &throttleBucket{tokens: t.burst, at: now}
		t.clients[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * t.perSecond
	if b.tokens > t.burst {
		b.tokens = t.burst
	}
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / t.perSecond * float64(time.Second))
	return false, wait
}

// sweep forgets addresses whose bucket has filled up again: they are
// back where a new address starts. At most once a minute.
func (t *anonThrottle) sweep(now time.Time) {
	if now.Sub(t.lastSweep) < time.Minute {
		return
	}
	t.lastSweep = now
	full := time.Duration(t.burst / t.perSecond * float64(time.Second))
	for key, b := range t.clients {
		if now.Sub(b.at) >= full {
			delete(t.clients, key)
		}
	}
}

// clientAddr is the address a request came from. The app is served
// behind a reverse proxy on the same machine (README: Caddy), which
// connects from loopback and appends the client it saw to
// X-Forwarded-For: so for a loopback peer the last entry of that
// header is the client. From any other peer the header is the
// client's own say-so and is ignored.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !peer.IsLoopback() {
		return host
	}
	fwd := r.Header.Values("X-Forwarded-For")
	if len(fwd) == 0 {
		return host
	}
	parts := strings.Split(fwd[len(fwd)-1], ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if ip := net.ParseIP(last); ip != nil {
		return ip.String()
	}
	return host
}

// throttleAnon wraps routes in t: requests without a signed-in
// session spend from their address's bucket and get 429 Too Many
// Requests, with Retry-After, once it is empty.
func (app *Application) throttleAnon(t *anonThrottle) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if app.sessions.GetBool(r.Context(), sessionAuthenticated) {
				next.ServeHTTP(w, r)
				return
			}
			if ok, wait := t.allow(clientAddr(r)); !ok {
				secs := int(wait/time.Second) + 1
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				http.Error(w, "Too many requests from your address. Try again in a minute, or sign in.", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// anonThrottles returns the page and simulator throttles, made on
// first use (the test rig builds an Application without New).
func (app *Application) anonThrottles() (pages, sim *anonThrottle) {
	app.anonThrottleOnce.Do(func() {
		app.anonPageThrottle = newAnonThrottle(anonPagesPerMinute, anonPagesBurst)
		app.anonSimThrottle = newAnonThrottle(anonSimPerMinute, anonSimBurst)
	})
	return app.anonPageThrottle, app.anonSimThrottle
}
