package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// blockedScraperSignatures names tokens in User-Agent or Sec-Ch-Ua headers
// from known automated AI training crawlers, commercial scrapers, and
// headless scraping tools.
var blockedScraperSignatures = []string{
	// Automated AI training and search scrapers
	"amazonbot",
	"amzn-searchbot",
	"reflectionbot",
	"bytespider",
	"petalbot",
	"claudebot",
	"gptbot",
	"ccbot",
	"perplexitybot",
	"diffbot",
	"facebookbot",
	"facebookexternalhit",
	"imagesiftbot",
	"omgilibot",
	"cohere-ai",
	"anthropic-ai",
	"semrushbot",

	// Headless scraping libraries and automation frameworks
	"headlesschrome",
	"phantomjs",
	"selenium",
	"puppeteer",
	"playwright",
	"scrapy",
	"python-requests",
	"aiohttp",
	"httpx",
	"axios",
	"colly",
	"node-fetch",

	// Malformed brand generator artifact observed in synthetic traffic
	"not a(brand",
}

// hasScraperSignature reports whether the user agent or client hints contain
// a known scraper or headless automation signature.
func hasScraperSignature(ua, secChUa string) bool {
	if ua == "" && secChUa == "" {
		return false
	}
	lowerUA := strings.ToLower(ua)
	lowerSec := strings.ToLower(secChUa)
	for _, sig := range blockedScraperSignatures {
		if (lowerUA != "" && strings.Contains(lowerUA, sig)) ||
			(lowerSec != "" && strings.Contains(lowerSec, sig)) {
			return true
		}
	}
	return false
}

// isSameOriginReferer reports whether the referer matches the request host.
func isSameOriginReferer(referer, host string) bool {
	if referer == "" || host == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}
	refHost := u.Host
	if i := strings.IndexByte(refHost, ':'); i != -1 {
		refHost = refHost[:i]
	}
	reqHost := host
	if i := strings.IndexByte(reqHost, ':'); i != -1 {
		reqHost = reqHost[:i]
	}
	return strings.EqualFold(refHost, reqHost)
}

// hasFetchMetadataAnomaly reports whether the request headers exhibit impossible
// W3C Fetch Metadata contradictions characteristic of forged crawler traffic.
func hasFetchMetadataAnomaly(r *http.Request) bool {
	// A genuine browser navigating via user interaction (address bar, bookmark)
	// sends Sec-Fetch-Site: none without a same-origin Referer.
	// Forged requests often send both Sec-Fetch-Site: none and a same-origin Referer.
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "none" {
		ref := r.Referer()
		if isSameOriginReferer(ref, r.Host) || isSameOriginReferer(ref, r.Header.Get("X-Forwarded-Host")) {
			return true
		}
	}
	return false
}

// hasClientHintAnomaly reports whether Chromium Client Hints contradict
// legitimate browser behavior or the reported User-Agent.
func hasClientHintAnomaly(ua, secChUa, secChUaPlatform string) bool {
	// Real Chromium GREASE brands never contain opening parentheses.
	// Synthetic crawlers were observed generating malformed brands with "(".
	if strings.Contains(secChUa, "(") {
		return true
	}

	lowerUA := strings.ToLower(ua)

	// Non-Chromium browsers (Firefox, Safari) do not send Sec-Ch-Ua.
	if secChUa != "" {
		if strings.Contains(lowerUA, "firefox/") {
			return true
		}
		if strings.Contains(lowerUA, "safari/") && !strings.Contains(lowerUA, "chrome/") && !strings.Contains(lowerUA, "chromium/") {
			return true
		}
	}

	// Platform contradictions between Client Hints and User-Agent.
	if secChUaPlatform != "" && lowerUA != "" {
		p := strings.Trim(strings.ToLower(secChUaPlatform), "\"")
		switch p {
		case "windows":
			if strings.Contains(lowerUA, "cros") ||
				strings.Contains(lowerUA, "macintosh") ||
				strings.Contains(lowerUA, "iphone") ||
				strings.Contains(lowerUA, "ipad") ||
				strings.Contains(lowerUA, "android") {
				return true
			}
		case "macos":
			if strings.Contains(lowerUA, "windows") ||
				strings.Contains(lowerUA, "android") ||
				strings.Contains(lowerUA, "cros") {
				return true
			}
		case "linux", "chrome os", "cros":
			if strings.Contains(lowerUA, "windows") ||
				strings.Contains(lowerUA, "macintosh") ||
				strings.Contains(lowerUA, "iphone") ||
				strings.Contains(lowerUA, "ipad") {
				return true
			}
		}
	}

	return false
}

// hasMissingBrowserHeaders reports whether a request claims a
// Chromium browser but sends neither of the header sets Chromium
// sends on every request: Client Hints (Sec-Ch-Ua) and Fetch
// Metadata (Sec-Fetch-*). A copied User-Agent on a plain HTTP
// client — the compromised-agent pattern: browser UA, script
// transport — sends neither.
func hasMissingBrowserHeaders(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if !strings.Contains(ua, "chrome/") && !strings.Contains(ua, "chromium/") && !strings.Contains(ua, "edg/") {
		return false
	}
	return r.Header.Get("Sec-Ch-Ua") == "" && r.Header.Get("Sec-Fetch-Site") == ""
}

// isBlockedBot reports whether an incoming HTTP request exhibits known
// botnet fingerprints, crawler signatures, or protocol metadata anomalies.
func isBlockedBot(r *http.Request) bool {
	ua := r.UserAgent()
	secChUa := r.Header.Get("Sec-Ch-Ua")
	secChUaPlatform := r.Header.Get("Sec-Ch-Ua-Platform")

	if hasScraperSignature(ua, secChUa) {
		return true
	}
	if hasClientHintAnomaly(ua, secChUa, secChUaPlatform) {
		return true
	}
	if hasFetchMetadataAnomaly(r) {
		return true
	}
	if hasMissingBrowserHeaders(r) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Behavioral watches: what one request cannot show.
//
// The botnet pattern behind these (compromised agent accounts
// fetching through residential proxies, per DataDome's Galileo
// writeup): browser User-Agents on script transports, only HTML —
// never CSS, JS or fonts — and IDs enumerated in sequence, 500+
// requests a minute spread over thousands of IPs. Per-address
// throttles cannot see it (each address asks once), so the
// watches key on what the botnet cannot cheaply diversify:
//
//   - the header fingerprint (UA + client hints + language):
//     dozens of UAs served the whole 24,000-request sample, each
//     fingerprint asking for hundreds of pages and no assets;
//   - the aggregate request stream: IDs arriving in ascending
//     order at a rate no readership produces.
//
// Both watches are small, bounded and idle-expiring; verified
// signed-in requests are never counted or turned away by them
// (a merely present cookie is verified, never trusted). The
// answers are 429, not 403: this is rate, not identity.
// ---------------------------------------------------------------------------

const (
	// botAssetFreePages is how many HTML pages one fingerprint
	// may take with zero asset fetches before the watch answers.
	botAssetFreePages = 25
	botWatchMaxPrints = 8192
	botPrintIdle      = 15 * time.Minute
	assetCookieName   = "es_assets"

	// The enumeration valve: at least this many ID requests in
	// a minute, mostly ascending, is a sweep.
	enumMinSamples = 120
	enumSeqRatio   = 0.6
	enumFloodFor   = time.Minute
)

// fingerprintOf hashes the headers bot tooling varies least.
// IPs rotate per request; this is what stays constant.
func fingerprintOf(r *http.Request) string {
	h := fnv.New64a()
	for _, s := range []string{r.UserAgent(), r.Header.Get("Sec-Ch-Ua"),
		r.Header.Get("Sec-Ch-Ua-Platform"), r.Header.Get("Accept-Language")} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

type botPrint struct {
	html, assets int
	last         time.Time
}

type botFingerprintWatch struct {
	mu        sync.Mutex
	prints    map[string]*botPrint
	lastSweep time.Time
	now       func() time.Time
	// key signs the asset cookie. A cookie is client-asserted,
	// so presence alone must exempt nobody: the value is an
	// HMAC of the fingerprint under a per-process random key,
	// issued only by an actual /static/ response.
	key []byte
}

// assetCookieValue is the signed cookie value for a fingerprint.
func (w *botFingerprintWatch) assetCookieValue(fp string) string {
	mac := hmac.New(sha256.New, w.key)
	_, _ = mac.Write([]byte(fp))
	return fp + "." + hex.EncodeToString(mac.Sum(nil))
}

// validAssetCookie reports whether the request carries an asset
// cookie this process issued for its own fingerprint.
func (w *botFingerprintWatch) validAssetCookie(r *http.Request, fp string) bool {
	cookie, err := r.Cookie(assetCookieName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(w.assetCookieValue(fp)))
}

// hasVerifiedSession reports whether the request's session
// cookie names a real, signed-in session. The guard runs before
// the session middleware, and a cookie header is client-asserted
// (anyone can send one), so the token is checked against the
// session store and the decoded data must carry the signed-in
// mark. Costs one store read, only for requests claiming this
// exemption.
func (app *Application) hasVerifiedSession(r *http.Request) bool {
	sm := app.sessions
	if sm == nil || sm.Store == nil {
		return false
	}
	cookie, err := r.Cookie(sm.Cookie.Name)
	if err != nil || cookie.Value == "" {
		return false
	}
	// pgxstore's plain Store methods panic by design ("missing
	// context arg"); its real API is the Ctx variants. Prefer
	// those, fall back to the plain Store for simple stores.
	var data []byte
	var found bool
	if cs, ok := sm.Store.(interface {
		FindCtx(context.Context, string) ([]byte, bool, error)
	}); ok {
		data, found, err = cs.FindCtx(r.Context(), cookie.Value)
	} else {
		data, found, err = sm.Store.Find(cookie.Value)
	}
	if err != nil || !found {
		return false
	}
	_, values, err := sm.Codec.Decode(data)
	if err != nil {
		return false
	}
	authed, _ := values[sessionAuthenticated].(bool)
	return authed
}

func (w *botFingerprintWatch) print(fp string, now time.Time) *botPrint {
	p, ok := w.prints[fp]
	if !ok {
		if len(w.prints) >= botWatchMaxPrints {
			w.sweep(now)
			if len(w.prints) >= botWatchMaxPrints {
				for old := range w.prints { // still full: drop any one
					delete(w.prints, old)
					break
				}
			}
		}
		p = &botPrint{}
		w.prints[fp] = p
	}
	p.last = now
	return p
}

func (w *botFingerprintWatch) sweep(now time.Time) {
	if now.Sub(w.lastSweep) < time.Minute {
		return
	}
	w.lastSweep = now
	for fp, p := range w.prints {
		if now.Sub(p.last) >= botPrintIdle {
			delete(w.prints, fp)
		}
	}
}

// recordAsset notes a static-asset fetch by this fingerprint.
func (w *botFingerprintWatch) recordAsset(fp string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.sweep(now)
	w.print(fp, now).assets++
}

// recordPage notes an HTML page fetch and reports whether this
// fingerprint has taken botAssetFreePages pages with no assets.
func (w *botFingerprintWatch) recordPage(fp string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.sweep(now)
	p := w.print(fp, now)
	p.html++
	return p.assets == 0 && p.html >= botAssetFreePages
}

type enumSample struct {
	id int64
	at time.Time
}

type enumFloodWatch struct {
	mu        sync.Mutex
	ids       []enumSample
	floodTill time.Time
	now       func() time.Time
}

// observe records one enumerated ID and reports whether the
// stream has tipped into a sweep: enumMinSamples within a
// minute, enumSeqRatio of them ascending from the previous one.
func (w *enumFloodWatch) observe(id int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if now.Before(w.floodTill) {
		return true
	}
	cut := 0
	for cut < len(w.ids) && now.Sub(w.ids[cut].at) > time.Minute {
		cut++
	}
	w.ids = append(w.ids[cut:], enumSample{id: id, at: now})
	if len(w.ids) < enumMinSamples {
		return false
	}
	seq := 0
	for i := 1; i < len(w.ids); i++ {
		if d := w.ids[i].id - w.ids[i-1].id; d > 0 && d <= 4096 {
			seq++
		}
	}
	if float64(seq)/float64(len(w.ids)-1) >= enumSeqRatio {
		w.floodTill = now.Add(enumFloodFor)
		w.ids = nil
		return true
	}
	return false
}

// botWatches makes the behavioral watches on first use (the test
// rig builds an Application without New).
func (app *Application) botWatches() (*botFingerprintWatch, *enumFloodWatch) {
	app.botWatchOnce.Do(func() {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		app.botWatch = &botFingerprintWatch{prints: make(map[string]*botPrint), now: time.Now, key: key}
		app.enumWatch = &enumFloodWatch{now: time.Now}
	})
	return app.botWatch, app.enumWatch
}

// isHTMLPageRequest reports whether this is a browser document
// fetch (as opposed to an asset, API or form request).
func isHTMLPageRequest(r *http.Request) bool {
	if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/static/") ||
		strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	return r.Header.Get("Sec-Fetch-Dest") == "document" ||
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// enumTargetID extracts the enumerated ID from the surfaces
// the botnet sweeps, when the request carries one: a kill ID in
// the path, or an entity ID in the query of the intel, pilot and
// killmail pages (the origin captures show all of them walked
// with ?character= in sequence).
func enumTargetID(r *http.Request) (int64, bool) {
	parse := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n > 0
	}
	if strings.HasPrefix(r.URL.Path, "/intel/kill/") {
		return parse(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/intel/kill/"), "/"))
	}
	if strings.HasPrefix(r.URL.Path, "/intel/") || r.URL.Path == "/pilot/" || r.URL.Path == "/killmails/" {
		q := r.URL.Query()
		for _, key := range []string{"character", "corporation", "alliance", "system", "ship"} {
			if v := q.Get(key); v != "" {
				return parse(v)
			}
		}
	}
	return 0, false
}

func tooManyBotRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "Too many requests. Try again in a minute, or sign in.", http.StatusTooManyRequests)
}

// DisableBotGuard turns off the scraper block and the behavioral
// watches. Only the dev entrypoint calls it (internal/devtools), so
// browser automation such as Playwright can drive a local server.
// It runs during route setup, before the server starts serving.
func DisableBotGuard(app *Application) {
	app.botGuardOff = true
}

// blockScrapers turns away automated AI scrapers, headless tools, and botnet
// crawlers at the perimeter with 403 Forbidden before running database reads.
func (app *Application) blockScrapers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.botGuardOff {
			next.ServeHTTP(w, r)
			return
		}
		if isBlockedBot(r) {
			http.Error(w, "Automated scraping is not permitted.", http.StatusForbidden)
			return
		}
		prints, floods := app.botWatches()
		fp := fingerprintOf(r)
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/static/") {
			prints.recordAsset(fp)
			// A client that fetches assets is a browser (or a
			// browser-driven tool); remember it across visits —
			// caches and service workers mean later visits may
			// fetch no assets at all. The value is signed (see
			// assetCookieValue): a self-asserted cookie exempts
			// nobody.
			http.SetCookie(w, &http.Cookie{Name: assetCookieName, Value: prints.assetCookieValue(fp),
				Path: "/", MaxAge: 30 * 24 * 60 * 60, SameSite: http.SameSiteLaxMode})
			next.ServeHTTP(w, r)
			return
		}
		// Cookie presence alone exempts nobody — both cookies are
		// verified: the session against its store and signed-in
		// mark, the asset cookie against its signature.
		if isHTMLPageRequest(r) && !app.hasVerifiedSession(r) {
			if id, ok := enumTargetID(r); ok && floods.observe(id) {
				tooManyBotRequests(w)
				return
			}
			if !prints.validAssetCookie(r, fp) && prints.recordPage(fp) {
				tooManyBotRequests(w)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
