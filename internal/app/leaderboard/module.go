// Package leaderboard is the Top Stations module (/market/leaderboard/): stations ranked by their stored order-book activity. Public; reads stored rows only.
package leaderboard

import (
	"context"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is this module's registration.
var Module = module.Module{
	ID:            "leaderboard",
	Title:         "Top Stations",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/leaderboard/", Label: "Top Stations", Pages: []string{"market_leaderboard.html"}, After: "/market/tradefinder/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*page, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPage(deps), w, req) }
		}
		r.With(deps.AnonPages()).Get("/market/leaderboard/", h((*page).handleMarketLeaderboard))
	},
	Pages: []module.Page{
		{Template: "market_leaderboard.html", Section: "market"},
	},
}

// PageData is the view model passed to market_leaderboard.html.
type PageData struct {
	module.ShellData
	MarketLeaderboard *leaderboardView
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
type View = leaderboardView

// BuildView builds the page body from the stored rows.
func BuildView(ctx context.Context, deps module.Deps, q url.Values) *View {
	return newPage(deps).buildLeaderboardView(ctx, q)
}
