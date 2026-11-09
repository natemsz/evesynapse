package app

import (
	"net/http"
	"net/url"
	"strings"
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
	return false
}

// blockScrapers turns away automated AI scrapers, headless tools, and botnet
// crawlers at the perimeter with 403 Forbidden before running database reads.
func (app *Application) blockScrapers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBlockedBot(r) {
			http.Error(w, "Automated scraping is not permitted.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
