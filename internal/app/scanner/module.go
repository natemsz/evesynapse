// Package scanner is the Spread Scanner module (/market/scanner/): the station-trading screen over the stored per-station book statistics. Public; reads stored rows only.
package scanner

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is this module's registration.
var Module = module.Module{
	ID:            "scanner",
	Title:         "Spread Scanner",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Nav: []module.NavSection{{
		Section: "toolsanalysis",
		Entries: []module.NavEntry{
			{Href: "/market/scanner/", Label: "Spread Scanner", Pages: []string{"market_scanner.html"}, After: "/market/baskets/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		r.With(deps.AnonPages()).Get("/market/scanner/", h((*page).handleMarketScanner))
	},
	Pages: []module.Page{
		{Template: "market_scanner.html", Section: "toolsanalysis"},
	},
}

// PageData is the view model passed to market_scanner.html.
type PageData struct {
	module.ShellData
	MarketScanner *scannerView
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
type View = scannerView

// BuildView builds the page body from the stored rows.
func BuildView(ctx context.Context, deps module.Deps, q map[string][]string) *View {
	return newPage(deps).buildScannerView(ctx, q)
}
