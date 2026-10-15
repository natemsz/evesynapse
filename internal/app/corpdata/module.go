// Package corpdata is the Corporation Data module: the members, wallets,
// orders, assets and structures pages of a corporation, read from the
// corporation snapshots core keeps fetched. The access model (who may see
// a corporation, in-game roles) and the snapshot feed are core's.
package corpdata

import (
	"evesynapse/internal/balancechart"
	"evesynapse/internal/esi"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Corporation Data module's registration.
var Module = module.Module{
	ID:       "corpdata",
	Title:    "Corporation Data",
	Requires: []string{},
	// Each page shows what its scope grants and says plainly when the
	// character lacks the role or the scope, so none is required to load
	// the module.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-corporations.read_corporation_membership.v1", Unlocks: "The Members page lists the corporation's members."},
		{Scope: "esi-corporations.track_members.v1", Unlocks: "Members' locations, ships and last login."},
		{Scope: "esi-wallet.read_corporation_wallets.v1", Unlocks: "The Wallets page: division balances, journals and transactions."},
		{Scope: "esi-markets.read_corporation_orders.v1", Unlocks: "The Orders page: the corporation's market orders."},
		{Scope: "esi-assets.read_corporation_assets.v1", Unlocks: "The Assets page: corporation hangars and assets."},
		{Scope: "esi-corporations.read_structures.v1", Unlocks: "The Structures page: structures and their fuel and state."},
	},
	Widgets: []module.WidgetDecl{
		{ID: "pulse", Title: "Corp Pulse", Description: "What changed in the corporation: members in and out, wallet movements, structure arrivals and timers. Needs a qualifying in-game role.", W: 1, H: 1, Feature: "corp_members", After: "training"},
		{ID: "upkeep", Title: "Upkeep & Fuel", Description: "Structures low on fuel, unanchoring, or running a state timer. Station Managers and Directors only.", W: 1, H: 1, Feature: "corp_structures", After: "walletflow"},
	},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/corporations/members/", Label: "Members", Pages: []string{"corp_members.html"}, After: "/corporations/"},
			{Href: "/corporations/wallets/", Label: "Wallets", Pages: []string{"corp_wallets.html"}, After: "/corporations/members/"},
			{Href: "/corporations/orders/", Label: "Orders", Pages: []string{"corp_orders.html"}, After: "/corporations/wallets/"},
			{Href: "/corporations/assets/", Label: "Assets", Pages: []string{"corp_assets.html"}, After: "/corporations/orders/"},
			{Href: "/corporations/structures/", Label: "Structures", Pages: []string{"corp_structures.html"}, After: "/corporations/assets/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/corporations/members/", h((*mod).handleCorpMembers))
		auth.Get("/corporations/wallets/", h((*mod).handleCorpWallets))
		auth.Get("/corporations/orders/", h((*mod).handleCorpOrders))
		auth.Get("/corporations/assets/", h((*mod).handleCorpAssets))
		auth.Get("/corporations/structures/", h((*mod).handleCorpStructures))
	},
	Pages: []module.Page{
		{Template: "corp_members.html", Section: "corporation"},
		{Template: "corp_wallets.html", Section: "corporation"},
		{Template: "corp_orders.html", Section: "corporation"},
		{Template: "corp_assets.html", Section: "corporation"},
		{Template: "corp_structures.html", Section: "corporation"},
	},
}

// PageData is the view model passed to the corporation data templates.
type PageData struct {
	module.ShellData
	CorpChars   []module.CharLink
	CorpMembers *corpMembersView
	CorpWallets *corpWalletsView
	CorpOrders  *corpOrdersView
	CorpAssets  *corpAssetsView
	CorpStructs *corpStructuresView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }

// WalletGraph builds a division's balance chart from its journal and
// current balance, exported so the contract tests pin what the page draws.
func WalletGraph(journal esi.CorpJournal, balance float64, balanceKnown bool, balanceAt time.Time) (*balancechart.BalanceChart, string) {
	return corpWalletGraph(journal, balance, balanceKnown, balanceAt)
}
