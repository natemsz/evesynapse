// Package contacts is the Contacts module: the character contacts page and its name fragment. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
package contacts

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Contacts module's registration.
var Module = module.Module{
	ID:       "contacts",
	Title:    "Contacts",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-characters.read_contacts.v1", Unlocks: "Personal contacts and standings."},
	},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-alliances.read_contacts.v1", Unlocks: "Alliance contacts."},
	},
	Snapshots:  []string{esi.SnapContacts},
	Activities: []string{"pilots"},
	Nav: []module.NavSection{{
		Section: "pilot",
		Entries: []module.NavEntry{
			{Href: "/contacts/", Label: "Contacts", Pages: []string{"contacts.html"}, After: "/calendar/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/contacts/", h((*mod).handleContacts))
		auth.Get("/contacts/name-fragment", h((*mod).handleContactNameFragment))
	},
	Pages: []module.Page{
		{Template: "contacts.html", Section: "pilot"},
	},
}

// PageData is the view model passed to the Contacts templates.
type PageData struct {
	module.ShellData
	Contacts      *contactsView
	ContactsChars []module.CharLink
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
