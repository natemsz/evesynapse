// Package appraise is the Appraisal module (/market/appraise/):
// paste a list of items — a fit, a cargo scan, an asset window,
// a loot haul — and get every line priced by the appraisal core
// (core's house price order over stored data only), with the
// tier that spoke and its as-of on each line, and coverage next
// to every total. Signed-in users' appraisals are saved as
// snapshots (schema 058) and can be shared read-only; anonymous
// appraisals are computed and shown, never stored.
package appraise

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
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
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/appraise/", Label: "Appraisal", Pages: []string{"market_appraise.html"}, After: "/contracts/search/"},
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
		auth := r.With(deps.RequireAuth())
		auth.Post("/market/appraise/{id}/reprice", h((*page).handleAppraiseReprice))
	},
	Pages: []module.Page{
		{Template: "market_appraise.html", Section: "market"},
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
