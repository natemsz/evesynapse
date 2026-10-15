// Package wallet is the Wallet module: the character wallet page: balance, journal, transactions, the balance history and the wallet-flow cells. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
package wallet

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Wallet module's registration.
var Module = module.Module{
	ID:       "wallet",
	Title:    "Wallet",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-wallet.read_character_wallet.v1", Unlocks: "Balance, journal and transactions."},
	},
	EnhanceScopes: []module.Scope{},
	Snapshots:     []string{esi.SnapWallet, esi.SnapWalletJournal, esi.SnapWalletTxns},
	Widgets:       []module.WidgetDecl{{ID: "walletflow", Title: "Wallet Flow", Description: "Income vs spend over 7 and 30 days with top sources, sinks, and a per-character breakdown.", W: 2, H: 2, After: "pulse"}},
	Activities:    []string{"pilots"},
	Nav: []module.NavSection{{
		Section: "pilot",
		Entries: []module.NavEntry{
			{Href: "/wallet/", Label: "Wallet", Pages: []string{"wallet.html"}, After: "/assets/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/wallet/", h((*mod).handleWallet))
		auth.Get("/wallet/graph-fragment", h((*mod).handleWalletGraphFragment))
	},
	Pages: []module.Page{
		{Template: "wallet.html", Section: "pilot"},
	},
}

// PageData is the view model passed to the Wallet templates.
type PageData struct {
	module.ShellData
	Wallet      *walletView
	WalletChars []module.CharLink
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
