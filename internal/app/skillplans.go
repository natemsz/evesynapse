package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/skillplan"
)

// ---------------------------------------------------------------------------
// Skill plans (Phase 4): the /skills/plans pages, plan CRUD, the
// Magic 14 template, plan-from-fit, and the data loaders the skill
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
}

// skillPlanStepRow is one computed step of the plan editor.
type skillPlanStepRow struct {
	Skill   string
	SkillID int64
	FromTo  string // "III → V"
	SP      string
	Time    string
	Finish  string // RFC3339 UTC
	Prereq  bool
}

// skillPlanItemRow is one editor row: the user's entries in intent
// order (computation re-sorts; this is what reorder edits).
type skillPlanItemRow struct {
	SkillID int64
	Skill   string
	Level   string // roman
	CanUp   bool
	CanDown bool
}

// skillPlansView is the /skills/plans page body.
type skillPlansView struct {
	CharacterID   int64
	CharacterName string
	GraphWarming  bool // SDE skill graph not imported yet
	Plans         []skillPlanSummary
	Plan          *skillPlanDetail // selected plan, nil when none selected
	FitPreview    *fitPreview      // plan-from-fit preview state
	SearchQuery   string
	SearchResults []skillSearchRow
}

// skillPlanDetail is the selected plan, computed.
type skillPlanDetail struct {
	ID              int64
	Name            string
	Items           []skillPlanItemRow
	Steps           []skillPlanStepRow
	Dropped         []string // "Gunnery V — already in queue"
	Unknown         []string
	TotalSP         string
	TotalTime       string
	StartsNote      string
	AttrsNote       string
	AttrsLine       string
	UnallocatedNote string
	Remap           *remapView
	ConfirmDelete   bool
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
	for _, p := range plans {
		items, err := app.queries.ListSkillPlanItems(ctx, p.ID)
		if err != nil {
			logging.Errorf("skill plans: items for plan %d: %v", p.ID, err)
			continue
		}
		view.Plans = append(view.Plans, skillPlanSummary{
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

	// Editor skill search (only meaningful with a plan selected).
	view.SearchQuery = strings.TrimSpace(r.URL.Query().Get("q"))
	if view.SearchQuery != "" && selected != nil {
		hits, err := app.queries.SearchSDESkills(ctx, db.SearchSDESkillsParams{Lower: view.SearchQuery, Lower_2: view.SearchQuery})
		if err != nil {
			logging.Errorf("skill plans: search %q: %v", view.SearchQuery, err)
		}
		for _, hit := range hits {
			view.SearchResults = append(view.SearchResults, skillSearchRow{
				SkillID: hit.TypeID, Name: hit.Name, Rank: formatRank(hit.Rank),
			})
		}
	}

	app.render(ctx, w, http.StatusOK, "skillplans.html", data)
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

	for i, item := range items {
		detail.Items = append(detail.Items, skillPlanItemRow{
			SkillID: item.SkillTypeID,
			Skill:   nameOrID(names, item.SkillTypeID),
			Level:   esi.RomanLevel(int(item.TargetLevel)),
			CanUp:   i > 0,
			CanDown: i < len(items)-1,
		})
	}
	for _, s := range outcome.Steps {
		detail.Steps = append(detail.Steps, skillPlanStepRow{
			Skill:   nameOrID(names, s.SkillID),
			SkillID: s.SkillID,
			FromTo:  fmt.Sprintf("%s → %s", esi.RomanLevel(s.FromLevel), esi.RomanLevel(s.ToLevel)),
			SP:      esi.FormatInt(s.SPRemaining),
			Time:    humanDuration(time.Duration(s.Seconds * float64(time.Second))),
			Finish:  s.Finish.UTC().Format("2006-01-02 15:04 UTC"),
			Prereq:  s.Prereq,
		})
	}
	for _, d := range outcome.Dropped {
		detail.Dropped = append(detail.Dropped,
			fmt.Sprintf("%s %s — %s", nameOrID(names, d.SkillID), esi.RomanLevel(d.Level), d.Reason))
	}
	for _, id := range outcome.Unknown {
		detail.Unknown = append(detail.Unknown, nameOrID(names, id))
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
	// Enforce level ordering (Issue 5): can't add level N without levels 1..N-1 in plan
	// Get existing plan items for this skill
	existingItems, _ := app.queries.ListSkillPlanItems(ctx, planID)
	maxPlannedLevel := trained // trained levels need no plan row
	for _, item := range existingItems {
		if item.SkillTypeID == skillID && int(item.TargetLevel) > maxPlannedLevel {
			maxPlannedLevel = int(item.TargetLevel)
		}
	}
	// If adding level N, ensure all lower levels are in the plan
	// (trained levels are handled by skillplan.Compute)
	if level > maxPlannedLevel+1 {
		// Auto-add the missing intermediate levels
		for l := maxPlannedLevel + 1; l < level; l++ {
			pos, err := app.queries.NextSkillPlanPosition(ctx, planID)
			if err != nil {
				break
			}
			app.queries.UpsertSkillPlanItem(ctx, db.UpsertSkillPlanItemParams{
				PlanID: planID, SkillTypeID: skillID, TargetLevel: int64(l), Position: pos,
			})
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

func (app *Application) handleSkillPlanItemRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	planID, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	skillID, _ := strconv.ParseInt(r.FormValue("skill"), 10, 64)
	if _, ok := app.ownedPlan(ctx, userID, characterID, planID); ok && skillID > 0 {
		if err := app.queries.DeleteSkillPlanItem(ctx, db.DeleteSkillPlanItemParams{PlanID: planID, SkillTypeID: skillID}); err != nil {
			logging.Errorf("skill plans: remove skill %d from plan %d: %v", skillID, planID, err)
		}
	}
	skillPlansRedirect(w, r, characterID, planID)
}

func (app *Application) handleSkillPlanItemMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	planID, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	skillID, _ := strconv.ParseInt(r.FormValue("skill"), 10, 64)
	dir := r.FormValue("dir")
	if _, ok := app.ownedPlan(ctx, userID, characterID, planID); !ok || skillID <= 0 {
		skillPlansRedirect(w, r, characterID, planID)
		return
	}
	items, err := app.queries.ListSkillPlanItems(ctx, planID)
	if err != nil {
		skillPlansRedirect(w, r, characterID, planID)
		return
	}
	idx := -1
	for i, item := range items {
		if item.SkillTypeID == skillID {
			idx = i
			break
		}
	}
	swap := idx
	if dir == "up" {
		swap = idx - 1
	} else if dir == "down" {
		swap = idx + 1
	}
	if idx >= 0 && swap >= 0 && swap < len(items) && swap != idx {
		a, b := items[idx], items[swap]
		_ = app.queries.UpdateSkillPlanItemPosition(ctx, db.UpdateSkillPlanItemPositionParams{
			Position: b.Position, PlanID: planID, SkillTypeID: a.SkillTypeID,
		})
		_ = app.queries.UpdateSkillPlanItemPosition(ctx, db.UpdateSkillPlanItemPositionParams{
			Position: a.Position, PlanID: planID, SkillTypeID: b.SkillTypeID,
		})
	}
	skillPlansRedirect(w, r, characterID, planID)
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

// ---------------------------------------------------------------------------
// Magic 14 template. The 14 fundamental skills every ship benefits
// from, all trained to level 5, as published by EVE University:
// https://wiki.eveuniversity.org/The_Magic_14 (fetched 2026-10-03).
// Names resolve to type IDs through sde_types at creation time —
// if the wiki ever renames one, the missing name is reported, not
// silently planned around.
// ---------------------------------------------------------------------------

var magic14Skills = []string{
	"CPU Management",
	"Power Grid Management",
	"Capacitor Management",
	"Capacitor Systems Operation",
	"Mechanics",
	"Hull Upgrades",
	"Shield Management",
	"Shield Operation",
	"Long Range Targeting",
	"Signature Analysis",
	"Navigation",
	"Evasive Maneuvering",
	"Warp Drive Operation",
	"Spaceship Command",
}

const magic14Source = "https://wiki.eveuniversity.org/The_Magic_14"

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
// numbering duplicates ("Magic 14", "Magic 14 (2)", …).
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

func (app *Application) handleSkillPlanFromTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/skills/plans", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	template := r.FormValue("template")
	if template != "magic14" || !app.userOwnsCharacter(ctx, userID, characterID) {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	rows, err := app.queries.ListSDETypesByNames(ctx, magic14Skills)
	if err != nil {
		logging.Errorf("skill plans: magic 14 name lookup: %v", err)
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	byName := make(map[string]int64, len(rows))
	for _, row := range rows {
		byName[row.Name] = row.TypeID
	}
	graph := newSDESkillGraph(app, ctx)
	targets := make([]skillplan.Target, 0, len(magic14Skills))
	for _, name := range magic14Skills {
		id, ok := byName[name]
		if !ok {
			logging.Warnf("skill plans: magic 14 skill %q not found in SDE types (wiki: %s)", name, magic14Source)
			continue
		}
		if _, isSkill := graph.Meta(id); !isSkill {
			logging.Warnf("skill plans: magic 14 skill %q (type %d) has no skill meta", name, id)
			continue
		}
		targets = append(targets, skillplan.Target{SkillID: id, Level: 5, Intent: len(targets)})
	}
	if len(targets) == 0 {
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	// A plan is what is left to learn: skills the character has already
	// trained to V stay out of it.
	total := len(targets)
	targets, skipped, _ := app.untrainedTargets(ctx, characterID, graph, targets)
	if len(targets) == 0 {
		app.flash(ctx, fmt.Sprintf("This character has already trained all %d Magic 14 skills to V, so there is nothing to plan.", total))
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	if len(skipped) > 0 {
		app.flash(ctx, fmt.Sprintf("Magic 14 plan created without the %d skill(s) already trained to V: %s.", len(skipped), strings.Join(skipped, ", ")))
	}
	name := app.uniquePlanName(ctx, userID, characterID, "Magic 14")
	planID, err := app.createPlanWithItems(ctx, userID, characterID, name, targets)
	if err != nil {
		logging.Errorf("skill plans: create magic 14 for character %d: %v", characterID, err)
		skillPlansRedirect(w, r, characterID, 0)
		return
	}
	skillPlansRedirect(w, r, characterID, planID)
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
