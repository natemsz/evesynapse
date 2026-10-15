// Package groups is the Groups module (/groups): sets of characters the
// directors of a corporation or alliance keep by hand. A group is a list
// and nothing more; what it is used for is elsewhere (at present Discord
// roles). The group tables are core's; this module owns every surface
// that edits them, including adding ticked members from the corporation
// members list.
package groups

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Groups module's registration.
var Module = module.Module{
	ID:       "groups",
	Title:    "Groups",
	Requires: []string{},
	Scopes:   []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-characters.read_corporation_roles.v1", Unlocks: "Directors can manage their corporation's groups."},
	},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/groups/", Label: "Groups", Pages: []string{"groups.html"}, After: "/srp/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/groups/", h((*mod).handleGroups))
		auth.Post("/groups/create", h((*mod).handleGroupCreate))
		auth.Post("/groups/{groupID}/delete", h((*mod).handleGroupDelete))
		auth.Post("/groups/{groupID}/members/add", h((*mod).handleGroupMemberAdd))
		auth.Post("/groups/{groupID}/members/remove", h((*mod).handleGroupMemberRemove))
		auth.Post("/corporations/members/group", h((*mod).handleCorpMembersGroup))
	},
	Pages: []module.Page{
		{Template: "groups.html", Section: "corporation"},
	},
}

// PageData is the view model passed to groups.html.
type PageData struct {
	module.ShellData
	Groups *groupsView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
