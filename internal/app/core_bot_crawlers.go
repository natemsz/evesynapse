package app

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Search engine crawlers fetch pages with no browser headers and no
// assets, which is exactly what the bot guard counts. A request that
// names a search crawler in its User-Agent is checked the way the
// search engines document: the address's reverse DNS name must sit
// under the engine's domain, and that name must resolve back to the
// address. A verified crawler is never counted; a request that only
// claims to be one goes through the guard like any other.

// searchCrawlers maps a User-Agent token to the domains its
// crawler's addresses resolve under.
var searchCrawlers = []struct {
	token    string
	suffixes []string
}{
	{"googlebot", []string{".googlebot.com", ".google.com"}},
	{"google-inspectiontool", []string{".googlebot.com", ".google.com"}},
	{"bingbot", []string{".search.msn.com"}},
	{"applebot", []string{".applebot.apple.com"}},
}

const (
	crawlerCacheMax  = 4096
	crawlerOKFor     = 24 * time.Hour
	crawlerFailFor   = time.Hour
	crawlerLookupMax = 4 // concurrent DNS checks; past it a claim waits for the next request
)

type crawlerVerdict struct {
	ok bool
	at time.Time
}

type crawlerVerifier struct {
	mu         sync.Mutex
	seen       map[string]crawlerVerdict // address -> verdict
	sem        chan struct{}
	lookupAddr func(ctx context.Context, addr string) ([]string, error)
	lookupHost func(ctx context.Context, host string) ([]string, error)
	now        func() time.Time
}

func newCrawlerVerifier() *crawlerVerifier {
	return &crawlerVerifier{
		seen:       make(map[string]crawlerVerdict),
		sem:        make(chan struct{}, crawlerLookupMax),
		lookupAddr: net.DefaultResolver.LookupAddr,
		lookupHost: net.DefaultResolver.LookupHost,
		now:        time.Now,
	}
}

// crawlerVerifier is the application's verifier, made on first use.
func (app *Application) crawlerVerifier() *crawlerVerifier {
	app.crawlersOnce.Do(func() {
		if app.crawlers == nil {
			app.crawlers = newCrawlerVerifier()
		}
	})
	return app.crawlers
}

// claimedCrawler returns the token and domains of the search
// crawler a User-Agent names, empty when it names none.
func claimedCrawler(ua string) (string, []string) {
	ua = strings.ToLower(ua)
	for _, c := range searchCrawlers {
		if strings.Contains(ua, c.token) {
			return c.token, c.suffixes
		}
	}
	return "", nil
}

// verified reports whether the request comes from the search
// crawler its User-Agent names.
func (v *crawlerVerifier) verified(r *http.Request) bool {
	token, suffixes := claimedCrawler(r.UserAgent())
	if suffixes == nil {
		return false
	}
	addr := clientAddr(r)
	// The verdict is cached per claimed crawler as well as address:
	// one verified crawler's address must not vouch for every name.
	cacheKey := addr + "|" + token
	now := v.now()
	v.mu.Lock()
	if got, ok := v.seen[cacheKey]; ok {
		ttl := crawlerFailFor
		if got.ok {
			ttl = crawlerOKFor
		}
		if now.Sub(got.at) < ttl {
			v.mu.Unlock()
			return got.ok
		}
	}
	v.mu.Unlock()

	select {
	case v.sem <- struct{}{}:
		defer func() { <-v.sem }()
	default:
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	ok := v.check(ctx, addr, suffixes)

	v.mu.Lock()
	if len(v.seen) >= crawlerCacheMax {
		for k := range v.seen {
			delete(v.seen, k)
			break
		}
	}
	v.seen[cacheKey] = crawlerVerdict{ok: ok, at: now}
	v.mu.Unlock()
	return ok
}

// check runs the reverse-then-forward DNS check for one address.
func (v *crawlerVerifier) check(ctx context.Context, addr string, suffixes []string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	names, err := v.lookupAddr(ctx, addr)
	if err != nil {
		return false
	}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		under := false
		for _, s := range suffixes {
			if strings.HasSuffix(name, s) {
				under = true
				break
			}
		}
		if !under {
			continue
		}
		addrs, err := v.lookupHost(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if back := net.ParseIP(a); back != nil && back.Equal(ip) {
				return true
			}
		}
	}
	return false
}
