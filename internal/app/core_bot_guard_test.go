package app

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestBlockScrapers pins that automated AI crawlers, headless scraping tools,
// and synthetic botnet fingerprints receive an immediate 403 Forbidden without
// reaching inner handlers, while standard browsers and legitimate search engines pass through.
func TestBlockScrapers(t *testing.T) {
	app := &Application{}
	called := false
	handler := app.blockScrapers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	blockedUserAgents := []string{
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot) Chrome/119.0.6045.214 Safari/537.36",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amzn-SearchBot/0.1) Chrome/119.0.6045.214 Safari/537.36",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Reflectionbot/1.0; +https://reflection.ai/bot) Chrome/151.0.0.0 Safari/537.36",
		"Mozilla/5.0 (compatible; SemrushBot/7~bl; +http://www.semrush.com/bot.html)",
		"Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
		"Mozilla/5.0 (compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
		"PetalBot",
		"CCBot/2.0 (https://commoncrawl.org/faq/)",
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"cohere-ai",
		"python-requests/2.31.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/120.0.0.0 Safari/537.36",
		"Playwright/1.40.0",
		"Puppeteer/21.0.0",
		"Scrapy/2.11.0 (+https://scrapy.org)",
	}

	for _, ua := range blockedUserAgents {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kills/", nil)
		req.Header.Set("User-Agent", ua)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("UA %q: got status %d, want %d", ua, rec.Code, http.StatusForbidden)
		}
		if called {
			t.Errorf("UA %q: inner handler was reached, expected early block", ua)
		}
	}

	// Synthetic and malformed Client Hints.
	malformedClientHints := []struct {
		name    string
		ua      string
		secChUa string
	}{
		{
			name:    "Malformed GREASE brand with parenthesis",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36",
			secChUa: `"Chromium";v="154", "Google Chrome";v="154", "Not A(Brand";v="99"`,
		},
		{
			name:    "Sec-Ch-Ua present on Firefox",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
			secChUa: `"Chromium";v="121", "Google Chrome";v="121"`,
		},
		{
			name:    "Sec-Ch-Ua present on Safari",
			ua:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
			secChUa: `"Chromium";v="120"`,
		},
	}

	for _, tc := range malformedClientHints {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kills/", nil)
		req.Header.Set("User-Agent", tc.ua)
		req.Header.Set("Sec-Ch-Ua", tc.secChUa)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got status %d, want %d", tc.name, rec.Code, http.StatusForbidden)
		}
		if called {
			t.Errorf("%s: inner handler was reached", tc.name)
		}
	}

	// Platform contradictions between Client Hints and User-Agent.
	platformContradictions := []struct {
		name     string
		ua       string
		platform string
	}{
		{
			name:     "Windows platform hint with ChromeOS User-Agent",
			ua:       "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36",
			platform: `"Windows"`,
		},
		{
			name:     "Windows platform hint with Macintosh User-Agent",
			ua:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			platform: `"Windows"`,
		},
		{
			name:     "macOS platform hint with Windows User-Agent",
			ua:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			platform: `"macOS"`,
		},
	}

	for _, tc := range platformContradictions {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kills/", nil)
		req.Header.Set("User-Agent", tc.ua)
		req.Header.Set("Sec-Ch-Ua-Platform", tc.platform)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got status %d, want %d", tc.name, rec.Code, http.StatusForbidden)
		}
		if called {
			t.Errorf("%s: inner handler was reached", tc.name)
		}
	}

	// Protocol and Fetch Metadata contradictions (Sec-Fetch-Site: none with same-origin Referer).
	protocolContradictions := []struct {
		name    string
		host    string
		site    string
		referer string
	}{
		{
			name:    "Sec-Fetch-Site none with same-origin Referer on default host",
			host:    "evesynapse.app",
			site:    "none",
			referer: "https://evesynapse.app",
		},
		{
			name:    "Sec-Fetch-Site none with path Referer on default host",
			host:    "evesynapse.app",
			site:    "none",
			referer: "https://evesynapse.app/intel/kills/",
		},
		{
			name:    "Sec-Fetch-Site none with port-qualified local host",
			host:    "localhost:8080",
			site:    "none",
			referer: "http://localhost:8080/",
		},
	}

	for _, tc := range protocolContradictions {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kill/138182496/", nil)
		req.Host = tc.host
		req.Header.Set("Sec-Fetch-Site", tc.site)
		req.Header.Set("Referer", tc.referer)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got status %d, want %d", tc.name, rec.Code, http.StatusForbidden)
		}
		if called {
			t.Errorf("%s: inner handler was reached", tc.name)
		}
	}

	// Legitimate requests that must pass through.
	allowed := []struct {
		name    string
		ua      string
		secChUa string
		site    string
		referer string
	}{
		{
			name:    "Standard Chrome navigation from within site",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			secChUa: `"Chromium";v="120", "Google Chrome";v="120", "Not)A;Brand";v="99"`,
			site:    "same-origin",
			referer: "https://evesynapse.app/intel/kills/",
		},
		{
			name:    "Direct bookmark or address bar navigation in Chrome",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			secChUa: `"Chromium";v="120", "Google Chrome";v="120", "Not_A Brand";v="24"`,
			site:    "none",
			referer: "",
		},
		{
			name:    "Search referral from external site",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			secChUa: `"Chromium";v="120", "Google Chrome";v="120", "Not=A?Brand";v="8"`,
			site:    "cross-site",
			referer: "https://www.google.com/",
		},
		{
			name:    "Standard Firefox request",
			ua:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
			site:    "same-origin",
			referer: "https://evesynapse.app/",
		},
		{
			name:    "Standard Safari request",
			ua:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
			site:    "none",
			referer: "",
		},
		{
			name: "Googlebot crawler",
			ua:   "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		},
		{
			name: "Bingbot crawler",
			ua:   "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		},
		{
			name: "Local curl call",
			ua:   "curl/8.5.0",
		},
		{
			name: "Blank user agent",
			ua:   "",
		},
	}

	for _, tc := range allowed {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kills/", nil)
		req.Host = "evesynapse.app"
		if tc.ua != "" {
			req.Header.Set("User-Agent", tc.ua)
		}
		if tc.secChUa != "" {
			req.Header.Set("Sec-Ch-Ua", tc.secChUa)
		}
		if tc.site != "" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.referer != "" {
			req.Header.Set("Referer", tc.referer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: got status %d, want %d", tc.name, rec.Code, http.StatusOK)
		}
		if !called {
			t.Errorf("%s: inner handler was not reached", tc.name)
		}
	}
}

// attackChromeUA is the dominant User-Agent in a 24,000-request
// botnet sample (compromised agents on residential proxies): a
// real Chrome string, sent by a script transport.
const attackChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"

func botGuardTestApp() (*Application, http.Handler) {
	app := &Application{}
	return app, app.blockScrapers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func htmlPage(app *Application, h http.Handler, ua, url, remote string, cookies ...*http.Cookie) int {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = remote
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="153", "Google Chrome";v="153"`)
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Dest", "document")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestBlockScrapersForgedChrome: a Chromium UA with neither
// Client Hints nor Fetch Metadata is a copied UA on a script
// transport, however real the string looks.
func TestBlockScrapersForgedChrome(t *testing.T) {
	_, h := botGuardTestApp()
	req := httptest.NewRequest(http.MethodGet, "/intel/kills/?character=2121821954", nil)
	req.Header.Set("User-Agent", attackChromeUA)
	req.Header.Set("Referer", "https://evesynapse.app/intel/kills/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged Chrome: status %d, want 403", rec.Code)
	}
}

// TestBotWatchHTMLOnlyFingerprint: one fingerprint (rotating
// addresses, as residential proxies do) taking pages and never
// assets is throttled at the page budget; a fingerprint that
// fetches assets, and a client carrying the asset cookie, are not.
func TestBotWatchHTMLOnlyFingerprint(t *testing.T) {
	app, h := botGuardTestApp()
	for i := 1; i < botAssetFreePages; i++ {
		if code := htmlPage(app, h, attackChromeUA, "/intel/kills/?character=2121821954", "203.0.113.1:1000"); code != http.StatusOK {
			t.Fatalf("page %d: status %d, want 200", i, code)
		}
	}
	if code := htmlPage(app, h, attackChromeUA, "/intel/kills/?character=2121821954", "198.51.100.9:1000"); code != http.StatusTooManyRequests {
		t.Fatalf("page %d from a fresh address: status %d, want 429 (fingerprint, not address)", botAssetFreePages, code)
	}

	// A browser-shaped fingerprint: assets first, then pages.
	app2, h2 := botGuardTestApp()
	assetReq := httptest.NewRequest(http.MethodGet, "/static/site.css", nil)
	assetReq.Header.Set("User-Agent", attackChromeUA)
	assetReq.Header.Set("Sec-Ch-Ua", `"Chromium";v="153", "Google Chrome";v="153"`)
	assetReq.Header.Set("Sec-Fetch-Site", "same-origin")
	assetReq.Header.Set("Sec-Fetch-Dest", "style")
	assetRec := httptest.NewRecorder()
	h2.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK {
		t.Fatalf("asset: status %d, want 200", assetRec.Code)
	}
	var assetCookie *http.Cookie
	for _, c := range assetRec.Result().Cookies() {
		if c.Name == assetCookieName {
			assetCookie = c
		}
	}
	if assetCookie == nil {
		t.Fatal("asset response set no asset cookie")
	}
	for i := 0; i < botAssetFreePages+5; i++ {
		if code := htmlPage(app2, h2, attackChromeUA, "/market/", "203.0.113.1:1000", assetCookie); code != http.StatusOK {
			t.Fatalf("browser page %d: status %d, want 200", i, code)
		}
	}
}

// issuedAssetCookie performs one real /static/ fetch through h
// and returns the signed asset cookie it sets.
func issuedAssetCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/static/site.css", nil)
	req.Header.Set("User-Agent", attackChromeUA)
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="153", "Google Chrome";v="153"`)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == assetCookieName {
			return c
		}
	}
	t.Fatal("no asset cookie issued")
	return nil
}

// TestEnumFloodValve: IDs arriving in ascending order at volume
// trip the valve even with the asset cookie (fingerprints vary);
// the same volume in random order does not.
func TestEnumFloodValve(t *testing.T) {
	_, h := botGuardTestApp()
	cookie := issuedAssetCookie(t, h)
	code := 0
	for i := 0; i < enumMinSamples+10; i++ {
		code = htmlPage(nil, h, attackChromeUA, "/intel/kill/"+strconv.Itoa(139000000+i)+"/", "203.0.113.1:1000", cookie)
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("sequential sweep: last status %d, want 429", code)
	}

	_, h2 := botGuardTestApp()
	cookie2 := issuedAssetCookie(t, h2)
	for i := 0; i < enumMinSamples+10; i++ {
		id := 139000000 + (i*7919)%500000 // deterministic scatter
		if code := htmlPage(nil, h2, attackChromeUA, "/intel/kill/"+strconv.Itoa(id)+"/", "203.0.113.1:1000", cookie2); code != http.StatusOK {
			t.Fatalf("random reads %d: status %d, want 200", i, code)
		}
	}

	// The query form the origin captures show: character IDs
	// walked in sequence across the intel surfaces.
	_, h3 := botGuardTestApp()
	cookie3 := issuedAssetCookie(t, h3)
	code = 0
	for i := 0; i < enumMinSamples+10; i++ {
		code = htmlPage(nil, h3, attackChromeUA, "/intel/kills/?character="+strconv.Itoa(2120000000+i), "203.0.113.1:1000", cookie3)
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("sequential character sweep: last status %d, want 429", code)
	}
}

// memSessionStore is an in-memory scs.Store for session tests.
type memSessionStore struct{ data map[string][]byte }

func (s *memSessionStore) Find(token string) ([]byte, bool, error) {
	b, ok := s.data[token]
	return b, ok, nil
}
func (s *memSessionStore) Commit(token string, b []byte, _ time.Time) error {
	s.data[token] = b
	return nil
}
func (s *memSessionStore) Delete(token string) error {
	delete(s.data, token)
	return nil
}

// TestBotWatchSpoofedCookies: self-asserted cookies exempt
// nobody. A made-up session cookie and a made-up asset cookie
// are both ignored; a session that really exists in the store
// and carries the signed-in mark is exempt.
func TestBotWatchSpoofedCookies(t *testing.T) {
	fake := []*http.Cookie{
		{Name: "evesynapse_session", Value: "forged-token"},
		{Name: assetCookieName, Value: "1"},
	}
	app, h := botGuardTestApp()
	code := 0
	for i := 0; i < botAssetFreePages+5; i++ {
		code = htmlPage(app, h, attackChromeUA, "/intel/kills/?character=2121821954", "203.0.113.1:1000", fake...)
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("spoofed cookies: last status %d, want 429", code)
	}

	// A verified signed-in session walks the same IDs freely.
	store := &memSessionStore{data: map[string][]byte{}}
	sm := newSessionManager(store, Config{})
	deadline := time.Now().Add(time.Hour)
	raw, err := sm.Codec.Encode(deadline, map[string]interface{}{sessionAuthenticated: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit("real-token", raw, deadline); err != nil {
		t.Fatal(err)
	}
	authedApp := &Application{sessions: sm}
	authed := authedApp.blockScrapers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	sessionCookie := &http.Cookie{Name: "evesynapse_session", Value: "real-token"}
	for i := 0; i < enumMinSamples+10; i++ {
		if code := htmlPage(authedApp, authed, attackChromeUA,
			"/intel/kill/"+strconv.Itoa(139000000+i)+"/", "203.0.113.1:1000", sessionCookie); code != http.StatusOK {
			t.Fatalf("signed-in read %d: status %d, want 200", i, code)
		}
	}
	// The same token without the signed-in mark is not exempt.
	raw, err = sm.Codec.Encode(deadline, map[string]interface{}{"seen": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit("anon-token", raw, deadline); err != nil {
		t.Fatal(err)
	}
	anonCookie := &http.Cookie{Name: "evesynapse_session", Value: "anon-token"}
	if authedApp.hasVerifiedSession(mustReqWithCookies(t, anonCookie)) {
		t.Fatal("anonymous session verified as signed-in")
	}
}

func mustReqWithCookies(t *testing.T, cookies ...*http.Cookie) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}
