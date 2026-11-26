package module

import (
	"context"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/skillplan"
)

// SDESkillGraph is the skill plan engine's skillplan.Graph over the
// SDE tables: skill meta and requirements read from the database,
// memoized for one render (plan expansion revisits the same skills).
// It is not safe for concurrent use.
type SDESkillGraph struct {
	q    *db.Queries
	ctx  context.Context
	what string
	meta map[int64]skillplan.SkillMeta
	miss map[int64]bool
	reqs map[int64][]skillplan.Requirement
}

// NewSDESkillGraph returns a graph for one render. what prefixes the
// log line when a requirements read fails.
func NewSDESkillGraph(ctx context.Context, q *db.Queries, what string) *SDESkillGraph {
	return &SDESkillGraph{
		q:    q,
		ctx:  ctx,
		what: what,
		meta: make(map[int64]skillplan.SkillMeta),
		miss: make(map[int64]bool),
		reqs: make(map[int64][]skillplan.Requirement),
	}
}

// Meta returns a skill's rank and training attributes.
func (g *SDESkillGraph) Meta(skillID int64) (skillplan.SkillMeta, bool) {
	if m, ok := g.meta[skillID]; ok {
		return m, true
	}
	if g.miss[skillID] {
		return skillplan.SkillMeta{}, false
	}
	row, err := g.q.GetSDESkillMeta(g.ctx, skillID)
	if err != nil {
		g.miss[skillID] = true
		return skillplan.SkillMeta{}, false
	}
	m := skillplan.SkillMeta{Rank: row.Rank, Primary: row.PrimaryAttr, Secondary: row.SecondaryAttr}
	g.meta[skillID] = m
	return m, true
}

// Requirements returns the skills a type needs. A failed read is
// logged and reads as no requirements.
func (g *SDESkillGraph) Requirements(typeID int64) []skillplan.Requirement {
	if rows, ok := g.reqs[typeID]; ok {
		return rows
	}
	dbRows, err := g.q.ListSDERequirementsByType(g.ctx, typeID)
	if err != nil {
		logging.Errorf("%s: requirements for type %d: %v", g.what, typeID, err)
		g.reqs[typeID] = nil
		return nil
	}
	rows := make([]skillplan.Requirement, 0, len(dbRows))
	for _, r := range dbRows {
		rows = append(rows, skillplan.Requirement{SkillID: r.SkillTypeID, Level: int(r.Level)})
	}
	g.reqs[typeID] = rows
	return rows
}

// SkillAttrs converts the attributes snapshot to the plan engine's
// set. An absent (loaded false) or empty snapshot falls back to the
// flat set, reported by the second return.
func SkillAttrs(raw esi.Attributes, loaded bool) (skillplan.AttrSet, bool) {
	if !loaded || (raw.Charisma == 0 && raw.Intelligence == 0 && raw.Memory == 0 && raw.Perception == 0 && raw.Willpower == 0) {
		return skillplan.FlatAttrSet, false
	}
	return skillplan.AttrSet{
		Charisma:     raw.Charisma,
		Intelligence: raw.Intelligence,
		Memory:       raw.Memory,
		Perception:   raw.Perception,
		Willpower:    raw.Willpower,
	}, true
}
