// Package restock is the Restock Planner module (/market/restock/): target stock levels per item against what the account has listed. Signed-in only.
package restock

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is this module's registration.
var Module = module.Module{
	ID:       "restock",
	Title:    "Restock Planner",
	Requires: []string{},
	Scopes:   []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-markets.read_character_orders.v1", Unlocks: "What you already have listed counts against each target."},
	},
	Activities: []string{"market"},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/restock/", Label: "Restock Planner", Pages: []string{"market_restock.html"}, After: "/market/tradefinder/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		auth := deps.RequireAuth()
		r.With(auth).Get("/market/restock/", h((*page).handleRestock))
		r.With(auth).Post("/market/restock/save/", h((*page).handleRestockSave))
		r.With(auth).Post("/market/restock/delete/", h((*page).handleRestockDelete))
	},
	Pages: []module.Page{
		{Template: "market_restock.html", Section: "market"},
	},
}

// PageData is the view model passed to market_restock.html.
type PageData struct {
	module.ShellData
	Restock *restockView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// page is one request's view of the foundation.
type page struct {
	deps    module.Deps
	queries *db.Queries
}

func newPage(deps module.Deps) *page { return &page{deps: deps, queries: deps.DB()} }
