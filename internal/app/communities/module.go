// Package communities is the Communities module (/fleets/communities/):
// groups of players outside the game's corporations. Anyone can read a
// community's public page. A pilot signed in through EVE with no account
// can join and leave. Running a community takes an account.
package communities

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Communities module's registration. It asks for no ESI
// scope.
var Module = module.Module{
	ID:            "communities",
	Title:         "Communities",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"corporation"},
	Nav: []module.NavSection{{
		Section: "fleets",
		Entries: []module.NavEntry{
			{Href: basePath, Label: "Communities", Pages: []string{"communities.html", "community.html"}},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		anon := r.With(deps.AnonPages())
		anon.Get(basePath, h((*mod).handleDirectory))
		anon.Get(basePath+"{slug}/", h((*mod).handleCommunity))
		// Joining and leaving are open to a pilot with no account.
		anon.Post(basePath+"{slug}/join", h((*mod).handleJoin))
		anon.Post(basePath+"{slug}/leave", h((*mod).handleLeave))

		auth := r.With(deps.RequireAuth())
		auth.Post(basePath+"create", h((*mod).handleCreate))
		auth.Post(basePath+"{slug}/settings", h((*mod).handleSettings))
		auth.Post(basePath+"{slug}/invite", h((*mod).handleNewInvite))
		auth.Post(basePath+"{slug}/close", h((*mod).handleClose))
		auth.Post(basePath+"{slug}/reopen", h((*mod).handleReopen))
		auth.Post(basePath+"{slug}/members/{characterID}", h((*mod).handleMemberAction))
	},
	Pages: []module.Page{
		{Template: "communities.html", Section: "fleets"},
		{Template: "community.html", Section: "fleets"},
	},
}

// PageData is the view model passed to communities.html and
// community.html.
type PageData struct {
	module.ShellData
	Directory *directoryView
	Community *communityView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
