// Package appraise is the Appraisal module (/market/appraise/):
// paste a list of items — a fit, a cargo scan, an asset window,
// a loot haul — and get every line priced by the appraisal core
// (core's house price order over stored data only), with the
// tier that spoke and its as-of on each line, and coverage next
// to every total. Appraisals are saved as snapshots (schema
// 058) and can be shared read-only: signed-in ones under their
// owner's account, anonymous ones under a shared anonymous
// owner (schema 064) remembered by a signed history cookie.
// Anonymous appraisals fall off 180 days after their last view;
// signed-in ones do not expire. Either creator can delete
// theirs.
package appraise

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Module is the Appraisal module's registration.
var Module = module.Module{
	ID:            "appraise",
	Title:         "Appraisal",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Nav: []module.NavSection{{
		Section: "tools",
		Entries: []module.NavEntry{
			{Href: "/market/appraise/", Label: "Appraisal", Pages: []string{"market_appraise.html"}, After: "/items/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		// The page and the shared-snapshot view read stored
		// data only, so they ride the anonymous page gate; the
		// compute itself rides the tighter simulator allowance,
		// like the fitting tool's POSTs.
		r.With(deps.AnonPages()).Get("/market/appraise/", h((*page).handleAppraisePage))
		r.With(deps.AnonPages()).Get("/market/appraise/s/{token}/", h((*page).handleAppraiseShare))
		r.With(deps.AnonSim()).Post("/market/appraise/", h((*page).handleAppraiseSubmit))
		r.With(deps.AnonSim()).Post("/market/appraise/s/{token}/delete", h((*page).handleAppraiseDeleteAnon))
		r.With(deps.AnonSim()).Post("/market/appraise/s/{token}/reprice", h((*page).handleAppraiseRepriceToken))
		auth := r.With(deps.RequireAuth())
		auth.Post("/market/appraise/{id}/reprice", h((*page).handleAppraiseReprice))
		auth.Post("/market/appraise/{id}/delete", h((*page).handleAppraiseDelete))
	},
	WorkerJobs: []module.WorkerJob{
		{Name: "appraisal expiry", After: module.AfterMarket, Start: func(ctx context.Context, deps module.Deps) {
			p := newPage(deps)
			if n, err := p.pruneExpiredAnonymousAppraisals(ctx, time.Now()); err != nil {
				logging.Errorf("appraisal: expiry sweep: %v", err)
			} else if n > 0 {
				logging.Infof("appraisal: expired %d unviewed anonymous appraisal(s)", n)
			}
		}},
	},
	Pages: []module.Page{
		{Template: "market_appraise.html", Section: "tools"},
	},
}

// PageData is the view model passed to market_appraise.html.
type PageData struct {
	module.ShellData
	Appraise *appraiseView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// page is one request's view of the foundation.
type page struct {
	deps    module.Deps
	queries *db.Queries
}

func newPage(deps module.Deps) *page { return &page{deps: deps, queries: deps.DB()} }
