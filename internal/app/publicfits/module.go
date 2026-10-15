// Package publicfits is the Public Fits module: the browser, the stable
// fit pages, ratings and suggestions for the fits anyone here has made
// public, and the worker jobs that keep those pages stored-only for
// anonymous readers (pages built ahead of time, views counted in memory
// and written in batches).
//
// The is_public flag and the fit storage stay core's; this module owns
// every surface that acts on the flag. What is derived from the fit
// itself (pricing, the module groups, the read-only simulation, EFT) is
// the fitting side's, reached through module.Deps.
package publicfits

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Public Fits module's registration.
var Module = module.Module{
	ID:       "publicfits",
	Title:    "Public Fits",
	Requires: []string{"fitting"},
	// Everything here is public data: no scope, signed in or not.
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Nav: []module.NavSection{{
		Section: "fitting",
		Entries: []module.NavEntry{
			{Href: module.PublicFitsPath, Label: "Public Loadouts Browser", Pages: []string{"fittings_public.html", "fit_public.html"}, After: "/fittings/saved/"},
		},
	}},
	Sitemap: []module.SitemapEntry{{Path: module.PublicFitsPath, ChangeFreq: "daily", Priority: "0.9"}},
	SitemapRows: func(ctx context.Context, deps module.Deps) ([]module.SitemapRow, error) {
		fits, err := deps.DB().SitemapTopFits(ctx)
		if err != nil {
			return nil, err
		}
		rows := make([]module.SitemapRow, 0, len(fits))
		for _, f := range fits {
			rows = append(rows, module.SitemapRow{
				Path:       module.PublicFitsPath + "fit/" + idString(f.ID) + "/",
				LastMod:    f.UpdatedAt.Format("2006-01-02"),
				ChangeFreq: "weekly",
				Priority:   "0.7",
			})
		}
		return rows, nil
	},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		anon := deps.AnonPages()
		r.With(anon).Get("/fittings/public/", h((*page).handlePublicFits))
		r.With(anon).Get("/fittings/public/suggest", h((*page).handlePublicFitSuggest))
		r.With(anon).Get("/fittings/public/fit/{fitID}/", h((*page).handlePublicFitPage))
		r.With(deps.RequireAuth()).Post("/fittings/public/fit/{fitID}/rate", h((*page).handlePublicFitRate))
	},
	// Pages built ahead of any visit, and the views counted since the
	// last cycle written in one batch: the same two chores, in the same
	// place in the cycle, as before the module existed.
	WorkerJobs: []module.WorkerJob{
		{Name: "public fit pages", After: module.AfterGuidePrices, Start: func(ctx context.Context, deps module.Deps) { deps.RefreshPublicFitPages(ctx) }},
		{Name: "public fit views", After: module.AfterGuidePrices, Start: func(ctx context.Context, deps module.Deps) { deps.FlushFitViews(ctx) }},
	},
	Pages: []module.Page{
		{Template: "fittings_public.html", Section: "fitting"},
		{Template: "fit_public.html", Section: "fitting"},
	},
}

// PageData is the view model passed to fittings_public.html and
// fit_public.html.
type PageData struct {
	module.ShellData
	PublicFits *publicFitsView
	PublicFit  *publicFitView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// page is one request's view of the foundation.
type page struct {
	deps    module.Deps
	queries *db.Queries
}

func newPage(deps module.Deps) *page { return &page{deps: deps, queries: deps.DB()} }
