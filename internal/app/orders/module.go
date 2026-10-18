// Package orders is the Orders module: the character market orders page: open orders, history and order lifecycle. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
package orders

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Orders module's registration.
var Module = module.Module{
	ID:       "market",
	Title:    "Market orders",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-markets.read_character_orders.v1", Unlocks: "Open orders and order history."},
		{Scope: "esi-markets.structure_markets.v1", Unlocks: "Prices at player-owned structures."},
	},
	EnhanceScopes:  []module.Scope{},
	Snapshots:      []string{esi.SnapOrders, esi.SnapOrdersHistory},
	Widgets:        []module.WidgetDecl{{ID: "market", Title: "Market", Description: "Open orders across characters: counts, sell/buy value, orders expiring soonest.", W: 1, H: 1, After: "pi"}},
	BriefingTopics: []module.BriefingTopicDecl{{ID: "orders", Title: "Order expiries and undercuts"}},
	Activities:     []string{"market"},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/orders/", Label: "My Orders", Pages: []string{"orders.html"}, After: "/market/restock/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/orders/", h((*mod).handleOrders))
	},
	Pages: []module.Page{
		{Template: "orders.html", Section: "market"},
	},
}

// PageData is the view model passed to the Orders templates.
type PageData struct {
	module.ShellData
	Orders      *ordersView
	OrdersChars []module.CharLink
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
