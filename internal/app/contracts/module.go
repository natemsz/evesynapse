// Package contracts is the Contracts module: the character contracts page: courier, item and auction contracts. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
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
	EnhanceScopes:  []module.Scope{},
	Snapshots:      []string{esi.SnapContracts},
	BriefingTopics: []module.BriefingTopicDecl{{ID: "contracts", Title: "Contract deadlines"}},
	Activities:     []string{"market"},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/contracts/", Label: "Contracts", Pages: []string{"contracts.html"}, After: "/orders/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/contracts/", h((*mod).handleContracts))
	},
	Pages: []module.Page{
		{Template: "contracts.html", Section: "market"},
	},
}

// PageData is the view model passed to the Contracts templates.
type PageData struct {
	module.ShellData
	Contracts      *contractsView
	ContractsChars []module.CharLink
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
