// Package buildplanner is the Build Planner module (/planner/): pick a
// manufacturable product and get its full build chain, with owned
// blueprints' ME/TE filled in, the stockpile netted against the
// requirements and a priced shopping list. The engine is
// internal/buildplan (pure); this package reads the static data and the
// stored snapshots through module.Deps and shapes the page and its JSON
// payload (the contract plan.js reads, documented in FRONTEND.md).
package buildplanner

import (
	"context"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Build Planner's registration.
var Module = module.Module{
	ID:       "buildplanner",
	Title:    "Build Planner",
	Requires: []string{},
	// The planner works for anyone, signed in or not.
	Scopes: []module.Scope{
		{Scope: "esi-characters.read_blueprints.v1", Unlocks: "Your owned blueprints fill in each build step's ME and TE."},
	},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-assets.read_assets.v1", Unlocks: "Your stockpile is netted against what the build needs."},
		{Scope: "esi-skills.read_skills.v1", Unlocks: "Build times use your trained industry skills, and a pilot can be judged on the skills the chain needs."},
	},
	Sitemap:    []module.SitemapEntry{{Path: "/planner/", ChangeFreq: "monthly", Priority: "0.8"}},
	Activities: []string{"industry"},
	Nav: []module.NavSection{{
		Section: "industry",
		Entries: []module.NavEntry{
			{Href: "/planner/", Label: "Build Planner", Pages: []string{"planner.html"}, After: "/industry/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		r.Route("/planner", func(r chi.Router) {
			r.With(deps.AnonSim()).Get("/", handlePlanner(deps))
			r.With(deps.AnonPages()).Get("/suggest", handlePlannerSuggest(deps))
		})
	},
	Pages: []module.Page{
		{Template: "planner.html", Section: "industry"},
	},
}

// PageData is the view model passed to planner.html.
type PageData struct {
	module.ShellData
	Planner *plannerView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// planner is one request's view of the foundation.
type planner struct {
	deps    module.Deps
	queries *db.Queries
}

func newPlanner(deps module.Deps) *planner {
	return &planner{deps: deps, queries: deps.DB()}
}

// The JSON the page embeds (and /planner/?format=json serves) is the
// contract plan.js reads; its types are exported so the contract tests
// decode exactly what the page ships.
type (
	Payload = planPayload
	NodeDTO = planNodeDTO
)

// SDESource is the planner's recipe source over the local SDE tables,
// memoized per render.
type SDESource = sdePlannerSource

// NewSDESource opens a recipe source for one render.
func NewSDESource(deps module.Deps, ctx context.Context) *SDESource {
	return &sdePlannerSource{app: newPlanner(deps), ctx: ctx, memo: make(map[int64]*buildplan.Blueprint), none: make(map[int64]bool)}
}

// Prefetch warms the product's whole blueprint closure in batched reads.
func (s *sdePlannerSource) Prefetch(root int64) { s.prefetch(root) }

// Memoized is how many blueprints the source has read so far.
func (s *sdePlannerSource) Memoized() int { return len(s.memo) }

type PresetTotals = planPresetTotals
