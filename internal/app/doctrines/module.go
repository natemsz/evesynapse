// Package doctrines is the Doctrines module: a corporation's doctrines,
// the fits that make them up, and the corporate loadouts browser. Which
// doctrine a keeper is choosing fits for, and reading a doctrine fit as a
// fit document, are access questions core answers (corp_doctrine_access.go)
// for the public fits browser and the fitting tool.
package doctrines

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Doctrines module's registration.
var Module = module.Module{
	ID:       "doctrines",
	Title:    "Doctrines",
	Requires: []string{"fitting"},
	// Anyone in a corporation can read its doctrines; who keeps them is the
	// corporation's own permission rule.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-characters.read_corporation_roles.v1", Unlocks: "Directors and role holders can keep their corporation's doctrines."},
		{Scope: "esi-fittings.read_fittings.v1", Unlocks: "A keeper can add a fit from their in-game fittings to a doctrine."},
	},
	Activities: []string{"corporation", "fitting"},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/doctrines/", Label: "Doctrines", Pages: []string{"doctrines.html", "doctrine.html"}, After: "/corporations/killmails/"},
			{Href: "/doctrines/fits/", Label: "Corporate Loadouts", Pages: []string{"corp_fits.html"}, After: "/doctrines/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/doctrines/", h((*mod).handleDoctrines))
		auth.Get("/doctrines/fits/", h((*mod).handleCorpFits))
		auth.Post("/doctrines/create", h((*mod).handleDoctrineCreate))
		auth.Get("/doctrines/suggest", h((*mod).handleDoctrineSuggest))
		auth.Get("/doctrines/{doctrineID}", h((*mod).handleDoctrine))
		auth.Post("/doctrines/{doctrineID}/save", h((*mod).handleDoctrineSave))
		auth.Post("/doctrines/{doctrineID}/delete", h((*mod).handleDoctrineDelete))
		auth.Post("/doctrines/{doctrineID}/fits/add", h((*mod).handleDoctrineFitAdd))
		auth.Post("/doctrines/{doctrineID}/fits/{fitID}/remove", h((*mod).handleDoctrineFitRemove))
	},
	Pages: []module.Page{
		{Template: "doctrines.html", Section: "corporation"},
		{Template: "doctrine.html", Section: "corporation"},
		{Template: "corp_fits.html", Section: "corporation"},
	},
}

// PageData is the view model passed to the doctrine templates.
type PageData struct {
	module.ShellData
	Doctrines *doctrinesView
	Doctrine  *doctrineView
	CorpFits  *corpFitsView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
