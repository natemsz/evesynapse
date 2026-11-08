package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBlockScrapers pins that automated AI crawlers and data scrapers
// receive an immediate 403 Forbidden without reaching inner handlers, while
// standard browsers and legitimate search engines pass through.
func TestBlockScrapers(t *testing.T) {
	app := &Application{}
	called := false
	handler := app.blockScrapers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	blocked := []string{
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot) Chrome/119.0.6045.214 Safari/537.36",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Reflectionbot/1.0; +https://reflection.ai/bot) Chrome/151.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
		"Mozilla/5.0 (compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
		"PetalBot",
		"CCBot/2.0 (https://commoncrawl.org/faq/)",
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"cohere-ai",
	}

	for _, ua := range blocked {
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

	allowed := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"curl/8.5.0",
		"",
	}

	for _, ua := range allowed {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/intel/kills/", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("UA %q: got status %d, want %d", ua, rec.Code, http.StatusOK)
		}
		if !called {
			t.Errorf("UA %q: inner handler was not reached", ua)
		}
	}
}
