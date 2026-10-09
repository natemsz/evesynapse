package app

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/skillplan"
)

// ---------------------------------------------------------------------------
// Skill plans (Phase 4): the /skills/plans pages, plan CRUD,
// plan-from-fit, and the data loaders the skill
// pages share. Everything renders from local state — the schema-012
// SDE graph and the worker-warmed skills/skillqueue/attributes
// snapshots — so no handler ever calls ESI (loadCorpSnapshot reads
// the snapshot table only).
// ---------------------------------------------------------------------------

// sdeSkillGraph is the handler-side skillplan.Graph: SDE table reads,
// memoized per render (plan expansion revisits the same skills).
type sdeSkillGraph struct {
	app  *Application
	ctx  context.Context
	meta map[int64]skillplan.SkillMeta
	miss map[int64]bool
	reqs map[int64][]skillplan.Requirement
}

func newSDESkillGraph(app *Application, ctx context.Context) *sdeSkillGraph {
	return &sdeSkillGraph{
		app:  app,
		ctx:  ctx,
		meta: make(map[int64]skillplan.SkillMeta),
		miss: make(map[int64]bool),
		reqs: make(map[int64][]skillplan.Requirement),
	}
}

func (g *sdeSkillGraph) Meta(skillID int64) (skillplan.SkillMeta, bool) {
	if m, ok := g.meta[skillID]; ok {
		return m, true
	}
	if g.miss[skillID] {
		return skillplan.SkillMeta{}, false
	}
	row, err := g.app.queries.GetSDESkillMeta(g.ctx, skillID)
	if err != nil {
		g.miss[skillID] = true
		return skillplan.SkillMeta{}, false
	}
	m := skillplan.SkillMeta{Rank: row.Rank, Primary: row.PrimaryAttr, Secondary: row.SecondaryAttr}
	g.meta[skillID] = m
	return m, true
}

func (g *sdeSkillGraph) Requirements(typeID int64) []skillplan.Requirement {
	if rows, ok := g.reqs[typeID]; ok {
		return rows
	}
	dbRows, err := g.app.queries.ListSDERequirementsByType(g.ctx, typeID)
	if err != nil {
		logging.Errorf("skill plans: requirements for type %d: %v", typeID, err)
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

// loadCharTraining assembles the engine's character input from the
// snapshots. loaded=false when the skills snapshot isn't there yet
// (the worker is still warming this character).
func (app *Application) loadCharTraining(ctx context.Context, characterID int64) (skillplan.CharTraining, esi.Attributes, bool) {
	ct := skillplan.CharTraining{
		SP:       make(map[int64]int64),
		QueuedTo: make(map[int64]int),
	}
	var skills esi.Skills
	if !app.loadCorpSnapshot(ctx, characterID, esi.SnapSkills, &skills) {
		return ct, esi.Attributes{}, false
	}
	for _, s := range skills.Skills {
		ct.SP[s.SkillID] = s.SkillpointsInSkill
	}
	ct.Unallocated = skills.UnallocatedSP

	var queue esi.Skillqueue
	if app.loadCorpSnapshot(ctx, characterID, esi.SnapSkillqueue, &queue) {
		for _, e := range queue {
			if e.FinishedLevel > ct.QueuedTo[e.SkillID] {
				ct.QueuedTo[e.SkillID] = e.FinishedLevel
			}
			if e.FinishDate != "" {
				if t, err := time.Parse(time.RFC3339, e.FinishDate); err == nil && t.After(ct.QueueEnd) {
					ct.QueueEnd = t
				}
			}
		}
	}

	var attrs esi.Attributes
	app.loadCorpSnapshot(ctx, characterID, esi.SnapAttributes, &attrs)
	return ct, attrs, true
}

// attrsOrFlat converts the ESI attributes payload into a skillplan.AttrSet,
// reporting whether a real snapshot backed it.
func attrsOrFlat(raw esi.Attributes, loaded bool) (skillplan.AttrSet, bool) {
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

// characterAttrsLoaded reports whether the attributes snapshot is
// readable for the character (separate from skills loading: the
// times fall back to a flat 20 spread with a notice either way).
func (app *Application) characterAttrsLoaded(ctx context.Context, characterID int64) bool {
	var raw esi.Attributes
	return app.loadCorpSnapshot(ctx, characterID, esi.SnapAttributes, &raw)
}

// typeNames resolves names for IDs from local caches only.
func (app *Application) typeNames(ctx context.Context, ids []int64) map[int64]string {
	return app.esi.CachedTypeNames(ctx, ids)
}

func nameOrID(names map[int64]string, id int64) string {
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	return fmt.Sprintf("Type #%d", id)
}

// ---------------------------------------------------------------------------
// Views.
// ---------------------------------------------------------------------------

// skillPlanSummary is one plan in the list.
type skillPlanSummary struct {
	ID        int64
	Name      string
	Items     int
	Character int64
	Selected  bool   // the plan open in the editor
	Left      string // "19h 8m left", "all trained" or "empty": what reviewing the list needs
}

// planListTimedMax is how many plans the picker times: each one is a full
// computation, so a very long list shows counts only past this.
const planListTimedMax = 25

// skillPlanRow is one row of the unified planner: one skill, in the order
// it will train. The skills the user put in the plan carry the move and
// remove controls; the prerequisites the plan pulls in are shown in their
// place in the order, without controls, with what needs them.
type skillPlanRow struct {
	SkillID  int64
	Skill    string
	Direct   bool          // a skill in the plan (movable, removable); false = pulled in as a prerequisite
	NeededBy string        // for a prerequisite: "for Beta Skill"
	Covered  bool          // already trained to the target (left out of the timing)
	Squares  template.HTML // the five level boxes: trained, planned, empty
	FromTo   string        // "III → V"
	SP       string
	Time     string
	Finish   string // UTC
	Note     string // "in queue", "already trained"
	CanUp    bool   // a move up is allowed (and would change the order)
	CanDown  bool
	UpWhy    string // when CanUp is false: why (a prerequisite blocks it, or it is first)
	DownWhy  string
}

// skillPlansView is the /skills/plans page body.
type skillPlansView struct {
	CharacterID   int64
	CharacterName string
	GraphWarming  bool // SDE skill graph not imported yet
	Plans         []skillPlanSummary
	PlanQuery     string           // the picker's search, when the page was loaded with one
	PlanTotal     int              // plans the character has, before the search narrows the list
	Plan          *skillPlanDetail // selected plan, nil when none selected
	FitPreview    *fitPreview      // plan-from-fit preview state
	SearchQuery   string
	SearchResults []skillSearchRow
	Message       string // set on a refused move, shown inside the editor
}

// skillPlanDetail is the selected plan, computed.
type skillPlanDetail struct {
	ID              int64
	Name            string
	Rows            []skillPlanRow // the plan in training order, prerequisites included
	Covered         []skillPlanRow // plan skills already trained or queued to their target
	Count           int            // skills in the plan
	Unknown         []string
	TotalSP         string
	TotalTime       string
	StartsNote      string
	AttrsNote       string
	AttrsLine       string
	UnallocatedNote string
	Remap           *remapView
	ConfirmDelete   bool

	order []int64 // the plan's own skills in training order (what a move trades places with)
}

// remapView is the remap advisor panel.
type remapView struct {
	CurrentLine string // "Intelligence 27 · Memory 21 · …"
	BestLine    string
	CurrentTime string
	BestTime    string
	Saving      string // "" when the current spread is already best
	BonusNote   string // "2 bonus remaps available"
}

// skillSearchRow is one editor skill-search hit.
type skillSearchRow struct {
	SkillID int64
	Name    string
	Rank    string
	Squares template.HTML // the character's trained level
	Levels  []levelOption // the levels still worth planning; empty when trained to V
}

// levelOption is one entry of a target-level picker.
type levelOption struct {
	Value    int
	Roman    string
	Selected bool
}

// fitPreview is the plan-from-fit preview: the closure checklist
// before the user commits it to a saved plan.
type fitPreview struct {
	FittingID   int64
	FitName     string
	ShipName    string
	ShipTypeID  int64
	Rows        []fitPreviewRow
	MissingData int // closure skills without SDE meta (left out on create)
	AlreadyMet  int // skills the character already satisfies
	ToTrain     int
}

type fitPreviewRow struct {
	Skill   string
	SkillID int64
	Level   string // roman required level
	Status  string // "trained" | "in queue" | "to train" | "no data"
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func (app *Application) handleSkillPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	_, active, links, err := app.pickCharacter(ctx, r, "/skills/plans")
	if err != nil {
		logging.Errorf("skill plans: list characters: %v", err)
		data.Error = "Could not load skill plans; check the server log."
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	data.SkillsChars = links

	view := &skillPlansView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.SkillPlans = view

	if n, err := app.queries.CountSDESkillMeta(ctx); err != nil || n == 0 {
		if err != nil {
			logging.Errorf("skill plans: count skill meta: %v", err)
		}
		view.GraphWarming = true
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}

	plans, err := app.queries.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: userID, CharacterID: active.CharacterID})
	if err != nil {
		logging.Errorf("skill plans: list for character %d: %v", active.CharacterID, err)
		data.Error = "Could not load skill plans; check the server log."
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	all := make([]skillPlanSummary, 0, len(plans))
	for _, p := range plans {
		items, err := app.queries.ListSkillPlanItems(ctx, p.ID)
		if err != nil {
			logging.Errorf("skill plans: items for plan %d: %v", p.ID, err)
		}
		all = append(all, skillPlanSummary{
			ID: p.ID, Name: p.Name, Items: len(items), Character: p.CharacterID,
		})
	}

	// Selected plan: ?plan= wins, else the first plan.
	var selected *db.SkillPlan
	if want, _ := strconv.ParseInt(r.URL.Query().Get("plan"), 10, 64); want > 0 {
		if p, err := app.queries.GetSkillPlan(ctx, db.GetSkillPlanParams{ID: want, UserID: userID}); err == nil && p.CharacterID == active.CharacterID {
			pp := p
			selected = &pp
		}
	}
	if selected == nil && len(plans) > 0 {
		p := plans[0]
		selected = &p
	}
	if selected != nil {
		view.Plan = app.buildPlanDetail(ctx, active, *selected, r.URL.Query().Get("confirm") == "delete")
	}

	// The picker: every plan with what is left to train in it, narrowed by
	// its search (?pq=) when the page is loaded without JavaScript; with
	// it the same search filters the list as it is typed.
	for i := range all {
		p := &all[i]
		p.Selected = selected != nil && p.ID == selected.ID
		switch {
		case p.Items == 0:
			p.Left = "empty"
		case p.Selected:
			p.Left = planLeft(view.Plan)
		case i < planListTimedMax:
			p.Left = planLeft(app.buildPlanDetail(ctx, active, plans[i], false))
		}
	}
	view.PlanTotal = len(all)
	view.PlanQuery = strings.TrimSpace(r.URL.Query().Get("pq"))
	for _, p := range all {
		if view.PlanQuery == "" || strings.Contains(strings.ToLower(p.Name), strings.ToLower(view.PlanQuery)) {
			view.Plans = append(view.Plans, p)
		}
	}

	// Editor skill search (only meaningful with a plan selected).
	view.SearchQuery = strings.TrimSpace(r.URL.Query().Get("q"))
	if view.SearchQuery != "" && selected != nil {
		hits, err := app.queries.SearchSDESkills(ctx, db.SearchSDESkillsParams{Lower: view.SearchQuery, Lower_2: view.SearchQuery})
		if err != nil {
			logging.Errorf("skill plans: search %q: %v", view.SearchQuery, err)
		}
		searchGraph := newSDESkillGraph(app, ctx)
		ct, _, trainingLoaded := app.loadCharTraining(ctx, active.CharacterID)
		for _, hit := range hits {
			trained := 0
			if trainingLoaded {
				trained = trainedLevel(searchGraph, ct, hit.TypeID)
			}
			row := skillSearchRow{
				SkillID: hit.TypeID, Name: hit.Name, Rank: formatRank(hit.Rank),
				Squares: planLevels(trained, trained),
			}
			// Only the levels above what is trained: those are what a plan
			// is for. V is the usual goal, so it starts selected.
			for l := trained + 1; l <= 5; l++ {
				row.Levels = append(row.Levels, levelOption{Value: l, Roman: esi.RomanLevel(l), Selected: l == 5})
			}
			view.SearchResults = append(view.SearchResults, row)
		}
	}

	app.render(ctx, w, http.StatusOK, "skillplans.html", data)
}

// planLeft says what is left in a computed plan, for the picker's list.
func planLeft(d *skillPlanDetail) string {
	switch {
	case d == nil:
		return ""
	case d.TotalTime != "":
		return d.TotalTime + " left"
	case d.Count > 0 && len(d.Rows) == 0:
		return "all trained"
	}
	return ""
}

// buildPlanDetail computes one plan against the character's
// current training state and shapes it for the editor.
func (app *Application) buildPlanDetail(ctx context.Context, ch db.Character, plan db.SkillPlan, confirmDelete bool) *skillPlanDetail {
	detail := &skillPlanDetail{ID: plan.ID, Name: plan.Name, ConfirmDelete: confirmDelete}

	items, err := app.queries.ListSkillPlanItems(ctx, plan.ID)
	if err != nil {
		logging.Errorf("skill plans: items for plan %d: %v", plan.ID, err)
		return detail
	}

	graph := newSDESkillGraph(app, ctx)
	ct, rawAttrs, skillsLoaded := app.loadCharTraining(ctx, ch.CharacterID)
	attrs, attrsKnown := attrsOrFlat(rawAttrs, skillsLoaded && app.characterAttrsLoaded(ctx, ch.CharacterID))

	targets := make([]skillplan.Target, 0, len(items))
	for _, item := range items {
		targets = append(targets, skillplan.Target{
			SkillID: item.SkillTypeID,
			Level:   int(item.TargetLevel),
			Intent:  int(item.Position),
		})
	}

	outcome := skillplan.Compute(graph, targets, ct, attrs, time.Now())
	if !skillsLoaded {
		// Without the skills snapshot every step would read as
		// from-scratch; say so instead of presenting fiction.
		detail.AttrsNote = "Skill data is still warming up for this character — times will settle once the first sync lands."
	}

	// Names for items, steps, drops, unknowns.
	ids := make([]int64, 0, len(items)+len(outcome.Steps))
	for _, item := range items {
		ids = append(ids, item.SkillTypeID)
	}
	for _, s := range outcome.Steps {
		ids = append(ids, s.SkillID)
	}
	for _, d := range outcome.Dropped {
		ids = append(ids, d.SkillID)
	}
	ids = append(ids, outcome.Unknown...)
	names := app.typeNames(ctx, ids)

	detail.Count = len(items)

	// What each plan skill needs (transitively): which prerequisite row is
	// for which skill, and which moves a prerequisite rules out.
	needs := make(map[int64]map[int64]bool, len(items))
	neededBy := make(map[int64][]string)
	for _, item := range items {
		needs[item.SkillTypeID] = prerequisiteClosure(graph, item.SkillTypeID)
	}
	for _, item := range items {
		for pre := range needs[item.SkillTypeID] {
			neededBy[pre] = append(neededBy[pre], nameOrID(names, item.SkillTypeID))
		}
	}

	// The plan's own skills, in the order they train: each one's
	// neighbours are what a move up or down trades places with. A plan
	// skill is the plan's even when an earlier skill also needs it (the
	// computation files that one under prerequisites), so membership of
	// the plan, not that flag, decides which rows can be moved.
	inPlan := make(map[int64]bool, len(items))
	for _, item := range items {
		inPlan[item.SkillTypeID] = true
	}
	var order []int64
	for _, s := range outcome.Steps {
		if inPlan[s.SkillID] {
			order = append(order, s.SkillID)
		}
	}
	detail.order = order
	slot := make(map[int64]int, len(order))
	for i, id := range order {
		slot[id] = i
	}

	for _, s := range outcome.Steps {
		row := skillPlanRow{
			SkillID: s.SkillID,
			Skill:   nameOrID(names, s.SkillID),
			Direct:  inPlan[s.SkillID],
			Squares: planLevels(s.FromLevel, s.ToLevel),
			FromTo:  fmt.Sprintf("%s → %s", esi.RomanLevel(s.FromLevel), esi.RomanLevel(s.ToLevel)),
			SP:      esi.FormatInt(s.SPRemaining),
			Time:    humanDuration(time.Duration(s.Seconds * float64(time.Second))),
			Finish:  s.Finish.UTC().Format("2006-01-02 15:04 UTC"),
		}
		if !row.Direct {
			row.NeededBy = neededByText(neededBy[s.SkillID])
		} else {
			i := slot[s.SkillID]
			switch {
			case i == 0:
				row.UpWhy = "it is already first"
			case needs[s.SkillID][order[i-1]]:
				row.UpWhy = fmt.Sprintf("%s has to be trained before it", nameOrID(names, order[i-1]))
			default:
				row.CanUp = true
			}
			switch {
			case i == len(order)-1:
				row.DownWhy = "it is already last"
			case needs[order[i+1]][s.SkillID]:
				row.DownWhy = fmt.Sprintf("%s needs it first", nameOrID(names, order[i+1]))
			default:
				row.CanDown = true
			}
		}
		detail.Rows = append(detail.Rows, row)
	}
	// Plan skills the character already has to their target: no timing,
	// full boxes, and still removable. Found from the plan's own items,
	// not the computation's dropped list, which leaves out a plan skill
	// that another skill also needs.
	stepped := make(map[int64]bool, len(outcome.Steps))
	for _, s := range outcome.Steps {
		stepped[s.SkillID] = true
	}
	unknown := make(map[int64]bool, len(outcome.Unknown))
	for _, id := range outcome.Unknown {
		unknown[id] = true
		detail.Unknown = append(detail.Unknown, nameOrID(names, id))
	}
	dropped := make(map[int64]skillplan.DroppedTarget, len(outcome.Dropped))
	for _, d := range outcome.Dropped {
		dropped[d.SkillID] = d
	}
	for _, item := range items {
		if stepped[item.SkillTypeID] || unknown[item.SkillTypeID] {
			continue
		}
		level, note := int(item.TargetLevel), "already trained"
		if d, ok := dropped[item.SkillTypeID]; ok {
			level, note = d.Level, d.Reason
		}
		detail.Covered = append(detail.Covered, skillPlanRow{
			SkillID: item.SkillTypeID,
			Skill:   nameOrID(names, item.SkillTypeID),
			Direct:  true,
			Covered: true,
			Squares: planLevels(level, level),
			FromTo:  esi.RomanLevel(level),
			Note:    note,
		})
	}

	if len(outcome.Steps) > 0 {
		detail.TotalSP = esi.FormatInt(outcome.TotalSP)
		detail.TotalTime = humanDuration(time.Duration(outcome.TotalSecond * float64(time.Second)))
		if ct.QueueEnd.After(time.Now()) {
			detail.StartsNote = fmt.Sprintf("Plan timing starts when your current queue finishes (%s).",
				ct.QueueEnd.UTC().Format("2006-01-02 15:04 UTC"))
		} else {
			detail.StartsNote = "Plan timing starts now — your queue is empty."
		}
	}
	if attrsKnown {
		detail.AttrsLine = fmt.Sprintf("Your attributes — Charisma %d · Intelligence %d · Memory %d · Perception %d · Willpower %d",
			attrs.Charisma, attrs.Intelligence, attrs.Memory, attrs.Perception, attrs.Willpower)
		advice := skillplan.AdviseRemap(graph, outcome.Steps, attrs)
		rv := &remapView{
			CurrentLine: spreadLine(advice.Current),
			BestLine:    spreadLine(advice.Best),
			CurrentTime: humanDuration(time.Duration(advice.CurSeconds * float64(time.Second))),
			BestTime:    humanDuration(time.Duration(advice.BestSeconds * float64(time.Second))),
		}
		if advice.Best != advice.Current && advice.BestSeconds < advice.CurSeconds {
			saving := advice.CurSeconds - advice.BestSeconds
			rv.Saving = humanDuration(time.Duration(saving * float64(time.Second)))
		}
		if rawAttrs.BonusRemaps != nil {
			rv.BonusNote = fmt.Sprintf("%d bonus remap(s) available", *rawAttrs.BonusRemaps)
		}
		detail.Remap = rv
	} else if detail.AttrsNote == "" {
		detail.AttrsNote = "Attribute snapshot is still warming up — training times assume a flat 20-point spread until it lands."
	}
	if ct.Unallocated > 0 {
		detail.UnallocatedNote = fmt.Sprintf("You have %s unallocated SP that could cover part of this plan.", esi.FormatInt(ct.Unallocated))
	}
	return detail
}

// spreadLine renders one attribute spread compactly.
func spreadLine(a skillplan.AttrSet) string {
	return fmt.Sprintf("Cha %d · Int %d · Mem %d · Per %d · Wil %d",
		a.Charisma, a.Intelligence, a.Memory, a.Perception, a.Willpower)
}

// formatRank renders an SDE rank (always integral in practice).
func formatRank(rank float64) string {
	if rank == float64(int64(rank)) {
		return fmt.Sprintf("%dx", int64(rank))
	}
	return fmt.Sprintf("%.1fx", rank)
}

// ---------------------------------------------------------------------------
// POST handlers. All verify plan ownership against the session
// user and redirect back to the relevant page.
// ---------------------------------------------------------------------------

func skillPlansRedirect(w http.ResponseWriter, r *http.Request, characterID, planID int64) {
	url := fmt.Sprintf("/skills/plans?character=%d", characterID)
	if planID > 0 {
		url += fmt.Sprintf("&plan=%d", planID)
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// ownedPlan loads a plan the session user owns for the character.
func (app *Application) ownedPlan(ctx context.Context, userID, characterID, planID int64) (db.SkillPlan, bool) {
	plan, err := app.queries.GetSkillPlan(ctx, db.GetSkillPlanParams{ID: planID, UserID: userID})
	if err != nil || plan.CharacterID != characterID {
		return db.SkillPlan{}, false
	}
	return plan, true
}

func (app *Application) handleSkillPlanCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "Untitled plan"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	if !app.userOwnsCharacter(ctx, userID, characterID) {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	plan, err := app.queries.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
		UserID:      userID,
		CharacterID: characterID,
		Name:        name,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		logging.Errorf("skill plans: create for character %d: %v", characterID, err)
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	skillPlansRedirect(w, r, characterID, plan.ID)
}

// userOwnsCharacter reports whether the character belongs to the
// user (plans are only ever made for one's own characters).
func (app *Application) userOwnsCharacter(ctx context.Context, userID, characterID int64) bool {
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return false
	}
	for _, ch := range characters {
		if ch.CharacterID == characterID {
			return true
		}
	}
	return false
}

func (app *Application) handleSkillPlanItemAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	planID, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	skillID, _ := strconv.ParseInt(r.FormValue("skill"), 10, 64)
	level, _ := strconv.Atoi(r.FormValue("level"))
	if _, ok := app.ownedPlan(ctx, userID, characterID, planID); !ok || skillID <= 0 {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	if level < 1 {
		level = 1
	}
	if level > 5 {
		level = 5
	}
	// Only real skills can enter a plan (the browser/editor search
	// only offers them; a crafted POST gets the same answer).
	addGraph := newSDESkillGraph(app, ctx)
	if _, ok := addGraph.Meta(skillID); !ok {
		skillPlansRedirect(w, r, characterID, planID)
		return
	}
	// A skill the character has already trained to this level is done:
	// it does not go in a plan, and the lower levels it would need are
	// done too.
	trained := 0
	if ct, _, loaded := app.loadCharTraining(ctx, characterID); loaded {
		trained = trainedLevel(addGraph, ct, skillID)
	}
	if level <= trained {
		names := app.typeNames(ctx, []int64{skillID})
		app.flash(ctx, fmt.Sprintf("%s is already trained to %s on this character, so it is not added. Pick a higher level to plan.",
			nameOrID(names, skillID), esi.RomanLevel(trained)))
		skillPlansRedirect(w, r, characterID, planID)
		return
	}
	// A plan holds one target per skill, and the levels below it are
	// implied (the computation trains them on the way), so adding a skill
	// that is already planned to this level or higher changes nothing:
	// say so rather than quietly lowering the target.
	existingItems, _ := app.queries.ListSkillPlanItems(ctx, planID)
	for _, item := range existingItems {
		if item.SkillTypeID == skillID && int(item.TargetLevel) >= level {
			names := app.typeNames(ctx, []int64{skillID})
			app.flash(ctx, fmt.Sprintf("%s is already in this plan to %s.",
				nameOrID(names, skillID), esi.RomanLevel(int(item.TargetLevel))))
			skillPlansRedirect(w, r, characterID, planID)
			return
		}
	}

	pos, err := app.queries.NextSkillPlanPosition(ctx, planID)
	if err != nil {
		logging.Errorf("skill plans: next position for plan %d: %v", planID, err)
		skillPlansRedirect(w, r, characterID, planID)
		return
	}
	if err := app.queries.UpsertSkillPlanItem(ctx, db.UpsertSkillPlanItemParams{
		PlanID: planID, SkillTypeID: skillID, TargetLevel: int64(level), Position: pos,
	}); err != nil {
		logging.Errorf("skill plans: add skill %d to plan %d: %v", skillID, planID, err)
	}
	if r.FormValue("return") == "character" {
		http.Redirect(w, r, fmt.Sprintf("/character/?character=%d#browse", characterID), http.StatusSeeOther)
		return
	}
	skillPlansRedirect(w, r, characterID, planID)
}

// planEditRequest is what the in-place edit endpoints (move, remove) have
// in common: the signed-in user, the character and a plan they own.
type planEditRequest struct {
	userID      int64
	characterID int64
	plan        db.SkillPlan
	skillID     int64
}

// planEditFromRequest reads and checks an edit request. ok is false when
// the response has been written (nothing to edit: a redirect, or a 404
// for the in-place caller).
func (app *Application) planEditFromRequest(w http.ResponseWriter, r *http.Request) (req planEditRequest, ok bool) {
	ctx := r.Context()
	req.userID = int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || req.userID == 0 {
		if wantsFragment(r) {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return req, false
		}
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return req, false
	}
	req.characterID, _ = strconv.ParseInt(r.FormValue("character"), 10, 64)
	planID, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	req.skillID, _ = strconv.ParseInt(r.FormValue("skill"), 10, 64)
	plan, owned := app.ownedPlan(ctx, req.userID, req.characterID, planID)
	if !owned || req.skillID <= 0 {
		if wantsFragment(r) {
			http.Error(w, "no such plan", http.StatusNotFound)
			return req, false
		}
		skillPlansRedirect(w, r, req.characterID, planID)
		return req, false
	}
	req.plan = plan
	return req, true
}

// wantsFragment is true for the planner's own script, which sends the
// edit with fetch and swaps the returned editor into the page; a plain
// form post (no JavaScript) gets a redirect back to the page instead.
func wantsFragment(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

// planEdited answers an edit: the refreshed editor for the in-place
// caller (409 with the reason shown inside it when refused), or a
// redirect back to the page with the reason as a flash message.
func (app *Application) planEdited(w http.ResponseWriter, r *http.Request, req planEditRequest, refusal string) {
	ctx := r.Context()
	if !wantsFragment(r) {
		if refusal != "" {
			app.flash(ctx, refusal)
		}
		skillPlansRedirect(w, r, req.characterID, req.plan.ID)
		return
	}
	view := &skillPlansView{
		CharacterID: req.characterID,
		Plan:        app.buildPlanDetail(ctx, db.Character{CharacterID: req.characterID}, req.plan, false),
		Message:     refusal,
	}
	status := http.StatusOK
	if refusal != "" {
		status = http.StatusConflict
	}
	app.renderFragmentStatus(w, status, "skillplans.html", "plan-body", view)
}

func (app *Application) handleSkillPlanItemRemove(w http.ResponseWriter, r *http.Request) {
	req, ok := app.planEditFromRequest(w, r)
	if !ok {
		return
	}
	if err := app.queries.DeleteSkillPlanItem(r.Context(), db.DeleteSkillPlanItemParams{PlanID: req.plan.ID, SkillTypeID: req.skillID}); err != nil {
		logging.Errorf("skill plans: remove skill %d from plan %d: %v", req.skillID, req.plan.ID, err)
	}
	app.planEdited(w, r, req, "")
}

func (app *Application) handleSkillPlanItemMove(w http.ResponseWriter, r *http.Request) {
	req, ok := app.planEditFromRequest(w, r)
	if !ok {
		return
	}
	app.planEdited(w, r, req, app.movePlanSkill(r.Context(), req.plan, req.skillID, r.FormValue("dir")))
}

// movePlanSkill moves one of the plan's own skills a place earlier
// (dir "up") or later ("down") in the order it trains. It returns why it
// could not, or "" once the plan is reordered.
//
// The order a plan trains in is computed (prerequisites first, then by
// each skill's position), so a skill cannot pass one it depends on or
// that depends on it, and trading positions with a neighbour is not
// enough when prerequisites sit between them. The move is made on the
// computed order and the positions are renumbered to match, which the
// computation then reproduces.
func (app *Application) movePlanSkill(ctx context.Context, plan db.SkillPlan, skillID int64, dir string) string {
	if dir != "up" && dir != "down" {
		return "Pick a direction to move it."
	}
	detail := app.buildPlanDetail(ctx, db.Character{CharacterID: plan.CharacterID}, plan, false)
	var row *skillPlanRow
	for i := range detail.Rows {
		if detail.Rows[i].SkillID == skillID && detail.Rows[i].Direct {
			row = &detail.Rows[i]
			break
		}
	}
	if row == nil {
		return "That skill is not waiting to be trained, so there is nothing to move."
	}
	at := slices.Index(detail.order, skillID)
	if at < 0 {
		return "That skill is not in this plan."
	}
	to := at - 1
	if dir == "down" {
		to = at + 1
	}
	if (dir == "up" && !row.CanUp) || (dir == "down" && !row.CanDown) {
		why := row.UpWhy
		if dir == "down" {
			why = row.DownWhy
		}
		return fmt.Sprintf("%s can't move %s: %s.", row.Skill, map[string]string{"up": "earlier", "down": "later"}[dir], why)
	}

	items, err := app.queries.ListSkillPlanItems(ctx, plan.ID)
	if err != nil {
		logging.Errorf("skill plans: items for plan %d: %v", plan.ID, err)
		return "Could not reorder the plan; check the server log."
	}
	desired := slices.Clone(detail.order)
	desired[at], desired[to] = desired[to], desired[at]
	// Skills with nothing left to train are not in the order: they keep
	// their places after the rest.
	for _, item := range items {
		if !slices.Contains(desired, item.SkillTypeID) {
			desired = append(desired, item.SkillTypeID)
		}
	}
	current := make(map[int64]int64, len(items))
	for _, item := range items {
		current[item.SkillTypeID] = item.Position
	}
	for pos, id := range desired {
		if current[id] == int64(pos) {
			continue
		}
		if err := app.queries.UpdateSkillPlanItemPosition(ctx, db.UpdateSkillPlanItemPositionParams{
			Position: int64(pos), PlanID: plan.ID, SkillTypeID: id,
		}); err != nil {
			logging.Errorf("skill plans: reposition skill %d in plan %d: %v", id, plan.ID, err)
			return "Could not reorder the plan; check the server log."
		}
	}
	return ""
}

func (app *Application) handleSkillPlanDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	planID, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	if _, ok := app.ownedPlan(ctx, userID, characterID, planID); ok {
		if err := app.queries.DeleteSkillPlan(ctx, db.DeleteSkillPlanParams{ID: planID, UserID: userID}); err != nil {
			logging.Errorf("skill plans: delete plan %d: %v", planID, err)
		}
	}
	skillPlansRedirect(w, r, characterID, 0)
}

// createPlanWithItems makes a plan and fills it, returning its ID.
func (app *Application) createPlanWithItems(ctx context.Context, userID, characterID int64, name string, targets []skillplan.Target) (int64, error) {
	plan, err := app.queries.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
		UserID:      userID,
		CharacterID: characterID,
		Name:        name,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		return 0, err
	}
	for i, t := range targets {
		if err := app.queries.UpsertSkillPlanItem(ctx, db.UpsertSkillPlanItemParams{
			PlanID: plan.ID, SkillTypeID: t.SkillID, TargetLevel: int64(t.Level), Position: int64(i + 1),
		}); err != nil {
			return plan.ID, err
		}
	}
	return plan.ID, nil
}

// uniquePlanName finds a free plan name for the character by
// numbering duplicates ("Loki", "Loki (2)", …).
func (app *Application) uniquePlanName(ctx context.Context, userID, characterID int64, base string) string {
	plans, err := app.queries.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: userID, CharacterID: characterID})
	if err != nil {
		return base
	}
	taken := make(map[string]bool, len(plans))
	for _, p := range plans {
		taken[p.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s (%d)", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// ---------------------------------------------------------------------------
// Plan from fit: preview the skill closure of a saved fitting,
// then create on confirm. The closure always recomputes from the
// fitting snapshot server-side — the posted form only names the
// fitting, never the skill list.
// ---------------------------------------------------------------------------

// loadFitting finds one saved fitting of the character from its
// snapshot.
func (app *Application) loadFitting(ctx context.Context, characterID, fittingID int64) (esi.Fitting, bool) {
	var fittings esi.Fittings
	if !app.loadCorpSnapshot(ctx, characterID, esi.SnapFittings, &fittings) {
		return esi.Fitting{}, false
	}
	for _, f := range fittings {
		if f.FittingID == fittingID {
			return f, true
		}
	}
	return esi.Fitting{}, false
}

// fitClosureTargets computes a fitting's skill closure plus the
// per-skill status against the character (for the preview).
func (app *Application) fitClosureTargets(ctx context.Context, ch db.Character, fit esi.Fitting) (targets []skillplan.Target, preview *fitPreview) {
	graph := newSDESkillGraph(app, ctx)
	typeIDs := []int64{fit.ShipTypeID}
	seen := map[int64]bool{fit.ShipTypeID: true}
	for _, item := range fit.Items {
		if !seen[item.TypeID] {
			seen[item.TypeID] = true
			typeIDs = append(typeIDs, item.TypeID)
		}
	}
	targets = skillplan.FitSkillClosure(graph, typeIDs)

	ct, _, _ := app.loadCharTraining(ctx, ch.CharacterID)
	names := app.typeNames(ctx, append(typeIDs, func() []int64 {
		ids := make([]int64, 0, len(targets))
		for _, t := range targets {
			ids = append(ids, t.SkillID)
		}
		return ids
	}()...))

	preview = &fitPreview{
		FittingID:  fit.FittingID,
		FitName:    fit.Name,
		ShipName:   nameOrID(names, fit.ShipTypeID),
		ShipTypeID: fit.ShipTypeID,
	}
	for _, t := range targets {
		row := fitPreviewRow{Skill: nameOrID(names, t.SkillID), SkillID: t.SkillID, Level: esi.RomanLevel(t.Level)}
		meta, hasMeta := graph.Meta(t.SkillID)
		switch {
		case !hasMeta:
			row.Status = "no data"
			preview.MissingData++
		default:
			trained := skillplan.LevelForSP(meta.Rank, ct.SP[t.SkillID])
			switch {
			case trained >= t.Level:
				row.Status = "trained"
				preview.AlreadyMet++
			case ct.QueuedTo[t.SkillID] >= t.Level:
				row.Status = "in queue"
				preview.AlreadyMet++
			default:
				row.Status = "to train"
				preview.ToTrain++
			}
		}
		preview.Rows = append(preview.Rows, row)
	}
	return targets, preview
}

func (app *Application) handleSkillPlanFitPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	_, active, links, err := app.pickCharacter(ctx, r, "/skills/plans/fit")
	if err != nil || links == nil {
		if err != nil {
			logging.Errorf("skill plans: fit preview characters: %v", err)
			data.Error = "Could not load fittings; check the server log."
		}
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	data.SkillsChars = links
	view := &skillPlansView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.SkillPlans = view

	fittingID, _ := strconv.ParseInt(r.URL.Query().Get("fitting"), 10, 64)
	fit, ok := app.loadFitting(ctx, active.CharacterID, fittingID)
	if !ok {
		data.Error = "That fitting isn't in this character's saved fittings (or they haven't synced yet)."
		app.render(ctx, w, http.StatusOK, "skillplans.html", data)
		return
	}
	_, preview := app.fitClosureTargets(ctx, active, fit)
	view.FitPreview = preview
	app.render(ctx, w, http.StatusOK, "skillplans.html", data)
}

func (app *Application) handleSkillPlanFromFit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	fittingID, _ := strconv.ParseInt(r.FormValue("fitting"), 10, 64)
	if !app.userOwnsCharacter(ctx, userID, characterID) {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	var active *db.Character
	for i, ch := range characters {
		if ch.CharacterID == characterID {
			active = &characters[i]
			break
		}
	}
	if active == nil {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	fit, ok := app.loadFitting(ctx, characterID, fittingID)
	if !ok {
		http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
		return
	}
	targets, preview := app.fitClosureTargets(ctx, *active, fit)

	// Skills with no SDE meta can't be trained against the graph;
	// leave them out (the preview said so openly).
	graph := newSDESkillGraph(app, ctx)
	kept := targets[:0]
	for _, t := range targets {
		if _, ok := graph.Meta(t.SkillID); ok {
			kept = append(kept, t)
		}
	}
	// Skills the character has already trained to the level the fit
	// needs are done; the plan holds only what is left.
	fitTotal := len(kept)
	kept, skipped, _ := app.untrainedTargets(ctx, characterID, graph, kept)
	if len(kept) == 0 && fitTotal > 0 {
		app.flash(ctx, "This character already has every skill this fit needs, so there is nothing to plan.")
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	if len(skipped) > 0 {
		app.flash(ctx, fmt.Sprintf("Plan created without the %d skill(s) already trained: %s.", len(skipped), strings.Join(skipped, ", ")))
	}
	base := strings.TrimSpace(fit.Name)
	if base == "" {
		base = preview.ShipName
	}
	name := app.uniquePlanName(ctx, userID, characterID, "Fit: "+base)
	planID, err := app.createPlanWithItems(ctx, userID, characterID, name, kept)
	if err != nil {
		logging.Errorf("skill plans: create from fit %d for character %d: %v", fittingID, characterID, err)
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	skillPlansRedirect(w, r, characterID, planID)
}

// trainedLevel is the level the character has trained a skill to: from
// its trained skill points and rank, not counting queued levels. 0 when
// it is untrained or the skill has no data.
func trainedLevel(graph skillplan.Graph, ct skillplan.CharTraining, skillID int64) int {
	meta, ok := graph.Meta(skillID)
	if !ok {
		return 0
	}
	return skillplan.LevelForSP(meta.Rank, ct.SP[skillID])
}

// untrainedTargets drops the targets the character has already trained
// to (or past): a plan is for what is left to learn, and a skill that is
// done does not belong in one. skipped says what was left out, as
// "Name level" strings. With no skills snapshot yet there is nothing to
// compare against, so every target stays and known is false.
func (app *Application) untrainedTargets(ctx context.Context, characterID int64, graph skillplan.Graph, targets []skillplan.Target) (kept []skillplan.Target, skipped []string, known bool) {
	ct, _, loaded := app.loadCharTraining(ctx, characterID)
	if !loaded {
		return targets, nil, false
	}
	ids := make([]int64, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.SkillID)
	}
	names := app.typeNames(ctx, ids)
	for _, t := range targets {
		if trainedLevel(graph, ct, t.SkillID) >= t.Level {
			skipped = append(skipped, fmt.Sprintf("%s %s", nameOrID(names, t.SkillID), esi.RomanLevel(t.Level)))
			continue
		}
		kept = append(kept, t)
	}
	return kept, skipped, true
}

// prerequisiteClosure is every skill a skill needs, directly or through
// other skills (not the skill itself). A requirement cycle in the data
// ends the walk instead of looping.
func prerequisiteClosure(graph skillplan.Graph, skillID int64) map[int64]bool {
	out := make(map[int64]bool)
	var walk func(id int64)
	walk = func(id int64) {
		for _, req := range graph.Requirements(id) {
			if req.SkillID == skillID || out[req.SkillID] {
				continue
			}
			out[req.SkillID] = true
			walk(req.SkillID)
		}
	}
	walk(skillID)
	return out
}

// neededByText says what a prerequisite row is for: "for Beta Skill",
// or "for Beta Skill, Gamma Skill +2".
func neededByText(names []string) string {
	switch {
	case len(names) == 0:
		return ""
	case len(names) <= 2:
		return "for " + strings.Join(names, ", ")
	default:
		return fmt.Sprintf("for %s +%d", strings.Join(names[:2], ", "), len(names)-2)
	}
}

// planLevels draws the five level boxes of a plan row: filled for the
// levels already trained (or in the queue), shaded in the ember accent
// for the levels this plan trains, empty beyond. from and to are the
// plan step's first untrained level minus one and its target.
func planLevels(from, to int) template.HTML {
	if from < 0 {
		from = 0
	}
	if to > 5 {
		to = 5
	}
	label := fmt.Sprintf("Trained to level %d", from)
	if to > from {
		label += fmt.Sprintf(", plan trains it to level %d", to)
	}
	var b strings.Builder
	b.WriteString(`<span class="lvl lvl-plan" role="img" aria-label="`)
	b.WriteString(label)
	b.WriteString(`">`)
	for i := 1; i <= 5; i++ {
		switch {
		case i <= from && from >= 5:
			b.WriteString(`<i class="on max"></i>`)
		case i <= from:
			b.WriteString(`<i class="on"></i>`)
		case i <= to:
			b.WriteString(`<i class="plan"></i>`)
		default:
			b.WriteString(`<i></i>`)
		}
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}
