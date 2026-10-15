// Package fitting is the Fitting module: the ship workbench (the
// simulator over the All-V or a pilot's own skills and implants), saved
// loadouts, import and export, saving to the game, and the shopping list
// of a fit. The fit document, EFT, slot families and the simulation view
// are foundation (internal/fitview), which core and the other modules
// read; this package owns the pages and the writes.
package fitting

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/fitview"
)

// Module is the Fitting module's registration.
var Module = module.Module{
	ID:       "fitting",
	Title:    "Fitting",
	Requires: []string{},
	// The simulator works for anyone, signed in or not (All V).
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-skills.read_skills.v1", Unlocks: "Simulate with a pilot's own trained skills instead of All V."},
		{Scope: "esi-clones.read_implants.v1", Unlocks: "Simulate with the pilot's implants, and each jump clone's."},
		{Scope: "esi-fittings.read_fittings.v1", Unlocks: "Your in-game saved fittings appear beside your loadouts."},
		{Scope: "esi-fittings.write_fittings.v1", Unlocks: "Save a fit to the game's own fittings."},
	},
	Sitemap:    []module.SitemapEntry{{Path: "/fittings/", ChangeFreq: "monthly", Priority: "0.8"}},
	Activities: []string{"fitting"},
	Nav: []module.NavSection{{
		Section: "fitting",
		Entries: []module.NavEntry{
			{Href: "/fittings/", Label: "Ship Workbench", Pages: []string{"fittings.html"}},
			{Href: "/fittings/saved/", Label: "My Loadouts", Pages: []string{"fittings_saved.html"}, After: "/fittings/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*tool, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newTool(deps), w, req) }
		}
		// Anonymous-safe surfaces: the simulator (All-V without a
		// session) and the SDE-only picker. Handlers branch on the session.
		r.With(deps.AnonPages()).Get("/fittings/", h((*tool).handleFittings))
		r.With(deps.AnonSim()).Post("/fittings/simulate/", h((*tool).handleFitSimulate))
		r.With(deps.AnonPages()).Get("/fittings/picker.json", h((*tool).handleFitPickerJSON))
		auth := r.With(deps.RequireAuth())
		auth.Get("/fittings/saved/", h((*tool).handleFittingsSaved))
		auth.Get("/fittings/clones.json", h((*tool).handleFitClonesJSON))
		auth.Post("/fittings/save/", h((*tool).handleFitSave))
		auth.Post("/fittings/save-to-eve/", h((*tool).handleFitSaveToEVE))
		auth.Post("/fittings/delete/", h((*tool).handleFitDelete))
		auth.Post("/fittings/import/", h((*tool).handleFitImport))
		auth.Post("/fittings/export/", h((*tool).handleFitExport))
		auth.Get("/fittings/mine.json", h((*tool).handleFitMineJSON))
		auth.Post("/fittings/fork/", h((*tool).handleFitFork))
		auth.Get("/fittings/shopping/{id}/", h((*tool).handleFitShopping))
	},
	Pages: []module.Page{
		{Template: "fittings.html", Section: "fitting"},
		{Template: "fittings_saved.html", Section: "fitting"},
		{Template: "fit_shopping.html", Section: "fitting"},
	},
}

// PageData is the view model passed to the fitting templates.
type PageData struct {
	module.ShellData
	Fittings      *fittingsView
	FittingsChars []module.CharLink
	FitEditor     *fitEditorView
	Shopping      *shoppingView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// tool is one request's view of the foundation.
type tool struct {
	deps    module.Deps
	queries *db.Queries
	fv      *fitview.Builder
}

func newTool(deps module.Deps) *tool {
	return &tool{deps: deps, queries: deps.DB(), fv: deps.FitBuilder()}
}
