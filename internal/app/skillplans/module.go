package skillplans

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/skillplan"
)

// Module is the Skill Plans module's registration.
var Module = module.Module{
	ID:       "skillplans",
	Title:    "Skill Plans",
	Requires: []string{},
	Scopes:   []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-skills.read_skills.v1", Unlocks: "Plans show what is already trained, the skills still to train, and the time they take."},
		{Scope: "esi-skills.read_skillqueue.v1", Unlocks: "Skills already in the training queue count toward a plan, and times start when the queue ends."},
	},
	Nav: []module.NavSection{{
		Section: "pilot",
		Entries: []module.NavEntry{
			{Href: "/skills/plans", Label: "Skill Plans", Pages: []string{"skillplans.html"}, After: "/characters/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*plans, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newPlans(deps), w, req) }
		}
		r = r.With(deps.RequireAuth())
		r.Get("/skills/plans", h((*plans).handleSkillPlans))
		r.Get("/skills/plans/", h((*plans).handleSkillPlans)) // old links and bookmarks carry the slash
		r.Get("/skills/plans/fit", h((*plans).handleSkillPlanFitPreview))
		r.Post("/skills/plans/create", h((*plans).handleSkillPlanCreate))
		r.Post("/skills/plans/item-add", h((*plans).handleSkillPlanItemAdd))
		r.Post("/skills/plans/item-remove", h((*plans).handleSkillPlanItemRemove))
		r.Post("/skills/plans/item-move", h((*plans).handleSkillPlanItemMove))
		r.Post("/skills/plans/delete", h((*plans).handleSkillPlanDelete))
		r.Post("/skills/plans/from-fit", h((*plans).handleSkillPlanFromFit))
	},
	Pages: []module.Page{
		{Template: "skillplans.html", Section: "pilot"},
	},
}

// PageData is the view model passed to skillplans.html.
type PageData struct {
	module.ShellData
	SkillsChars []module.CharLink
	SkillPlans  *skillPlansView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// plans is one request's view of the foundation.
type plans struct {
	deps    module.Deps
	queries *db.Queries
}

func newPlans(deps module.Deps) *plans {
	return &plans{deps: deps, queries: deps.DB()}
}

// LoadCharTraining assembles the engine's character input from the
// stored snapshots (loaded=false until the skills snapshot exists).
func LoadCharTraining(ctx context.Context, deps module.Deps, characterID int64) (skillplan.CharTraining, esi.Attributes, bool) {
	return newPlans(deps).loadCharTraining(ctx, characterID)
}
