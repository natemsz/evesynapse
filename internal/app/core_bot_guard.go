package app

import (
	"net/http"
	"strings"
)

// blockedScraperSignatures names User-Agent tokens of known automated AI
// training crawlers and commercial scrapers that harvest site data without
// providing search referral traffic.
var blockedScraperSignatures = []string{
	"amazonbot",
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
}

// isBlockedScraper reports whether the user agent belongs to a known
// scraper that should be turned away before running expensive renders.
func isBlockedScraper(ua string) bool {
	if ua == "" {
		return false
	}
	lower := strings.ToLower(ua)
	for _, sig := range blockedScraperSignatures {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// blockScrapers turns away known AI training crawlers and scrapers at the
// perimeter with 403 Forbidden before they reach session or database reads.
func (app *Application) blockScrapers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBlockedScraper(r.UserAgent()) {
			http.Error(w, "Automated scraping is not permitted.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
