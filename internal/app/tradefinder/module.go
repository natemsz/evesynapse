// Package tradefinder is the Tradefinder module (/market/tradefinder/): cross-region routes ranked from the stored region statistics. Public; reads stored rows only and queues pending routes for signed-in visitors.
package tradefinder

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is this module's registration.
var Module = module.Module{
	ID:            "tradefinder",
	Title:         "Tradefinder",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/tradefinder/", Label: "Tradefinder", Pages: []string{"market_tradefinder.html"}, After: "/market/scanner/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		r.With(deps.AnonPages()).Get("/market/tradefinder/", h((*page).handleMarketTradefinder))
	},
	Pages: []module.Page{
		{Template: "market_tradefinder.html", Section: "market"},
	},
}

// PageData is the view model passed to market_tradefinder.html.
type PageData struct {
	module.ShellData
	MarketTradefinder *tradefinderView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// page is one request's view of the foundation.
type page struct {
	deps    module.Deps
	queries *db.Queries
}

func newPage(deps module.Deps) *page { return &page{deps: deps, queries: deps.DB()} }

// View is the page body, exported with BuildView so the contract tests can
// build and inspect exactly what the page renders.
type View = tradefinderView

// BuildView builds the page body from the stored rows.
func BuildView(ctx context.Context, deps module.Deps, q map[string][]string) *View {
	return newPage(deps).buildTradefinderView(ctx, q)
}

// RowCap is how many routes one render lists at most.
const RowCap = tradefinderRowCap
