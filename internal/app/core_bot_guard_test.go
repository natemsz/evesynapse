package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
