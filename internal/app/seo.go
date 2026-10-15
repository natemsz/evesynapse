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

// handleRobots serves the crawler rules as text/plain: the static
// rules, and the sitemap's address at this instance's own origin
// (the line is left out when the instance has none configured).
func (app *Application) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(robotsTXT))
	if origin := app.canonicalOrigin(); origin != "" {
		_, _ = w.Write([]byte("Sitemap: " + origin + "/sitemap.xml\n"))
	}
}

// canonicalOrigin is the absolute base for every crawler- or
// share-facing URL: this instance's own configured public origin
// (Config.publicOrigin), and nothing else. Every instance is
// somebody's own site, so no other site's address is ever a
// fallback: an instance with no origin configured gets "", and
// its callers then publish no absolute URL at all. It never
// derives from the triggering request either: a rebuild kicked
// off through an alias host must not bake that alias into the
// sitemap or the shared links.
func (app *Application) canonicalOrigin() string {
	return app.cfg.publicOrigin()
}

// publicPageURL is the absolute address of one of this instance's
// public pages, or "" when the instance has no origin configured.
func (app *Application) publicPageURL(path string) string {
	origin := app.canonicalOrigin()
	if origin == "" {
		return ""
	}
	return origin + path
}

// handleSitemap serves the generated sitemap, rebuilt at most
// hourly: the core public pages (landing, public fits, simulator,
// item browser, the public planners, the Intel pages), the top
// public fits by weighted rating, and bounded top item and ship
// lists.
func (app *Application) handleSitemap(w http.ResponseWriter, r *http.Request) {
	// A sitemap is a list of absolute URLs: without an origin of
	// its own this instance has none to give.
	origin := app.canonicalOrigin()
	if origin == "" {
		http.NotFound(w, r)
		return
	}
	sitemapMu.Lock()
	fresh := time.Since(sitemapAt) < sitemapTTL && sitemapXML != nil
	var out []byte
	if fresh {
		out = sitemapXML
	}
	sitemapMu.Unlock()
	if !fresh {
		var err error
		out, err = app.buildSitemap(r.Context(), origin)
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
// pages (the landing page, the public fits browser and simulator,
// the item browser, the public build and PI planners, and the
// Intel pages — the same surfaces robots.txt names), the top
// public fits by weighted rating (LIMIT 1000), the top items by
// swept volume (LIMIT 1000), and the ships with the most public
// fits (LIMIT 200). Authed paths never appear.
func (app *Application) buildSitemap(ctx context.Context, origin string) ([]byte, error) {
	urls := []sitemapURL{
		{Loc: origin + "/", ChangeFreq: "weekly", Priority: "1.0"},
		{Loc: origin + "/fittings/public/", ChangeFreq: "daily", Priority: "0.9"},
		{Loc: origin + "/fittings/", ChangeFreq: "monthly", Priority: "0.8"},
		{Loc: origin + "/items/", ChangeFreq: "weekly", Priority: "0.8"},
		{Loc: origin + "/planner/", ChangeFreq: "monthly", Priority: "0.8"},
		{Loc: origin + "/industry/pi/", ChangeFreq: "monthly", Priority: "0.8"},
		{Loc: origin + "/intel/wars/", ChangeFreq: "weekly", Priority: "0.7"},
		{Loc: origin + "/intel/incursions/", ChangeFreq: "weekly", Priority: "0.7"},
		{Loc: origin + "/intel/fw/", ChangeFreq: "weekly", Priority: "0.7"},
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
			Loc:        origin + "/items/type/" + idString(it.TypeID) + "/",
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
