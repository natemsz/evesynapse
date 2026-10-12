package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/xml"
	"net/http"
	"sync"
	"time"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public discovery: /robots.txt and /sitemap.xml. Both sit outside
// the auth middleware: crawlers carry no session cookie.
// ---------------------------------------------------------------------------

//go:embed seo/robots.txt
var robotsTXT string

// sitemapTTL bounds how stale the generated sitemap may be; the
// fit, item and ship lists underneath are all LIMIT-capped reads.
const sitemapTTL = time.Hour

// sitemapCache is the last rendered sitemap and when it was built,
// guarded by sitemapMu. Every public fit, item and ship URL in it
// is anonymous-safe by construction: the routes below serve them
// with no session.
var (
	sitemapMu  sync.Mutex
	sitemapAt  time.Time
	sitemapXML []byte
)

// handleRobots serves the static crawler rules as text/plain.
func (app *Application) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(robotsTXT))
}

// siteOrigin is the absolute base for public URLs: the
// configured public origin, or the request host when the operator
// has not set one (tests, first boot before SSO is configured).
func (app *Application) siteOrigin(r *http.Request) string {
	if origin := app.cfg.publicOrigin(); origin != "" {
		return origin
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// handleSitemap serves the generated sitemap, rebuilt at most
// hourly: the landing page, the public browser and simulator, the
// top public fits by weighted rating, and bounded top item and
// ship lists.
func (app *Application) handleSitemap(w http.ResponseWriter, r *http.Request) {
	sitemapMu.Lock()
	fresh := time.Since(sitemapAt) < sitemapTTL && sitemapXML != nil
	var out []byte
	if fresh {
		out = sitemapXML
	}
	sitemapMu.Unlock()
	if !fresh {
		var err error
		out, err = app.buildSitemap(r.Context(), app.siteOrigin(r))
		if err != nil {
			logging.Errorf("sitemap: build: %v", err)
			http.Error(w, "Sitemap unavailable", http.StatusInternalServerError)
			return
		}
		sitemapMu.Lock()
		sitemapXML, sitemapAt = out, time.Now()
		sitemapMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(out)
}

type sitemapURL struct {
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
}

type sitemapSet struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// buildSitemap renders the sitemap from the database: core public
// pages, the top public fits by weighted rating (LIMIT 1000), the
// top items by swept volume (LIMIT 1000), and the ships with the
// most public fits (LIMIT 200). Authed paths never appear.
func (app *Application) buildSitemap(ctx context.Context, origin string) ([]byte, error) {
	urls := []sitemapURL{
		{Loc: origin + "/", ChangeFreq: "weekly", Priority: "1.0"},
		{Loc: origin + "/fittings/public/", ChangeFreq: "daily", Priority: "0.9"},
		{Loc: origin + "/fittings/", ChangeFreq: "monthly", Priority: "0.8"},
	}
	fits, err := app.queries.SitemapTopFits(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range fits {
		urls = append(urls, sitemapURL{
			Loc:        origin + "/fittings/public/fit/" + idString(f.ID) + "/",
			LastMod:    f.UpdatedAt.Format("2006-01-02"),
			ChangeFreq: "weekly",
			Priority:   "0.7",
		})
	}
	items, err := app.queries.SitemapTopItems(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		urls = append(urls, sitemapURL{
			Loc:        origin + "/item/" + idString(it.TypeID) + "/",
			ChangeFreq: "weekly",
			Priority:   "0.6",
		})
	}
	ships, err := app.queries.SitemapTopShips(ctx)
	if err != nil {
		return nil, err
	}
	for _, sh := range ships {
		urls = append(urls, sitemapURL{
			Loc:        origin + "/ships/" + idString(sh.ShipTypeID) + "/",
			ChangeFreq: "weekly",
			Priority:   "0.6",
		})
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.Encode(sitemapSet{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: urls}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
