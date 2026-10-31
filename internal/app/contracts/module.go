// Package contracts is the Contracts module: the character contracts page: courier, item and auction contracts. It reads the
// snapshots core keeps fetched; the fetch itself is core's. It also
// carries the public contract search and detail pages over the
// stored public corpus core sweeps and appraises (search.go).
package contracts

import (
	"context"
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Contracts module's registration.
var Module = module.Module{
	ID:       "contracts",
	Title:    "Contracts",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-contracts.read_character_contracts.v1", Unlocks: "Courier, item and auction contracts."},
	},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-ui.open_window.v1", Unlocks: "Open a contract in the game client from its page."},
	},
	Snapshots:      []string{esi.SnapContracts},
	BriefingTopics: []module.BriefingTopicDecl{{ID: "contracts", Title: "Contract deadlines"}},
	Activities:     []string{"market"},
	Nav: []module.NavSection{
		{
			Section: "finances",
			Entries: []module.NavEntry{
				{Href: "/contracts/", Label: "My Contracts", Pages: []string{"contracts.html"}, After: "/orders/"},
			},
		},
		{
			Section: "market",
			Entries: []module.NavEntry{
				{Href: "/contracts/search/", Label: "Contract Search", Pages: []string{"contract_search.html", "contract_detail.html"}, After: "/market/"},
			},
		},
	},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/contracts/", h((*mod).handleContracts))
		// The public search and detail pages read the stored
		// corpus only, so they ride the anonymous gate like the
		// other public market surfaces.
		anon := r.With(deps.AnonPages())
		anon.Get("/contracts/search/", h((*mod).handleContractSearch))
		anon.Get("/contracts/search/{contractID}/", h((*mod).handleContractDetail))
		anon.Get("/contracts/search/{contractID}/panel", h((*mod).handleContractPanel))
		// View in game acts as one of the reader's characters,
		// so it is signed-in only.
		auth.Post("/ui/openwindow/contract/{contractID}", h((*mod).handleOpenContractInGame))
	},
	Pages: []module.Page{
		{Template: "contracts.html", Section: "finances"},
		{Template: "contract_search.html", Section: "market"},
		{Template: "contract_detail.html", Section: "market"},
	},
}

// PageData is the view model passed to the Contracts templates.
type PageData struct {
	module.ShellData
	Contracts      *contractsView
	ContractsChars []module.CharLink
	ContractSearch *searchView
	ContractDetail *detailView
	ContractPanel  *panelView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }

// LoadItems reads one contract's stored item list; ok is false until its
// detail was fetched. Exported for the economy tests.
func LoadItems(ctx context.Context, deps module.Deps, contractID int64) (esi.ContractItems, bool, error) {
	return newMod(deps).loadContractItems(ctx, contractID)
}
