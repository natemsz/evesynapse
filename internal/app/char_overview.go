package app

// The redesigned Character page (the Pilot Overview): a
// three-pane workspace over the same stored snapshots the page
// has always read. The left pane is the skill directory with
// per-category SP share bars and the Clone Bay; the center
// switches between the trained sheet, the browse catalog and
// the clones surface; the right pane is an inspector for the
// selected skill or clone, holding the staged plan.
//
// All view state lives in the URL (surface, group, selection,
// search, level filter, target plan, staged items), the same
// server-driven pattern as the Assets page: every control is a
// link or a GET form, and a render is a pure function of the
// snapshots plus that state. The only math beyond formatting is
// the skillplan package's own (SP per level, training times,
// plan computation), reused as the audit directs.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/skillplan"
)

// overviewState is the page's view state as the URL carries it.
type overviewState struct {
	Surface string // "sheet" | "browse" | "clones"
	Group   string // selected skill group name, "" = default
	SkillID int64  // inspector skill selection, 0 = default
	Clone   string // inspector clone selection: "active" or a jump clone id
	Query   string // the one search over sheet and browse
	Level   string // level filter: "all" | "v" | "mid" | "none"
	PlanID  int64  // target plan for the staged plan's save
	Stage   []stageItem
}

// stageItem is one staged plan entry: a skill and the level it
// is staged to train to.
type stageItem struct {
	SkillID int64
	Level   int
}

// parseOverviewState reads the view state from the request URL,
// tolerating junk by falling back to defaults.
func parseOverviewState(r *http.Request) overviewState {
	q := r.URL.Query()
	st := overviewState{Surface: "sheet", Level: "all", Clone: "active"}
	switch q.Get("surface") {
	case "browse", "clones":
		st.Surface = q.Get("surface")
	}
	st.Group = q.Get("group")
	st.SkillID, _ = strconv.ParseInt(q.Get("skill"), 10, 64)
	if c := q.Get("clone"); c != "" {
		st.Clone = c
	}
	st.Query = strings.TrimSpace(q.Get("q"))
	switch q.Get("lvl") {
	case "v", "mid", "none":
		st.Level = q.Get("lvl")
	}
	st.PlanID, _ = strconv.ParseInt(q.Get("plan"), 10, 64)
	for _, part := range strings.Split(q.Get("stage"), ",") {
		idText, levelText, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		id, err1 := strconv.ParseInt(idText, 10, 64)
		level, err2 := strconv.Atoi(levelText)
		if err1 != nil || err2 != nil || id <= 0 || level < 1 || level > 5 {
			continue
		}
		st.stageSet(id, level)
	}
	return st
}

// stageSet stages a skill at a level, keeping the highest level
// anyone asked for and the order skills were first staged in.
func (st *overviewState) stageSet(skillID int64, level int) {
	for i, it := range st.Stage {
		if it.SkillID == skillID {
			if level > it.Level {
				st.Stage[i].Level = level
			}
			return
		}
	}
	st.Stage = append(st.Stage, stageItem{SkillID: skillID, Level: level})
}

// stageLevel reports the level a skill is staged at, 0 when it
// is not staged.
func (st overviewState) stageLevel(skillID int64) int {
	for _, it := range st.Stage {
		if it.SkillID == skillID {
			return it.Level
		}
	}
	return 0
}

// with returns a copy of the state with mutate applied, for
// building a link's target state.
func (st overviewState) with(mutate func(*overviewState)) overviewState {
	c := st
	c.Stage = append([]stageItem(nil), st.Stage...)
	if mutate != nil {
		mutate(&c)
	}
	return c
}

// href renders the state as a character-page URL.
func (st overviewState) href(characterID int64) string {
	v := url.Values{}
	v.Set("character", strconv.FormatInt(characterID, 10))
	if st.Surface != "" && st.Surface != "sheet" {
		v.Set("surface", st.Surface)
	}
	if st.Group != "" {
		v.Set("group", st.Group)
	}
	if st.SkillID != 0 {
		v.Set("skill", strconv.FormatInt(st.SkillID, 10))
	}
	if st.Clone != "" && st.Clone != "active" {
		v.Set("clone", st.Clone)
	}
	if st.Query != "" {
		v.Set("q", st.Query)
	}
	if st.Level != "" && st.Level != "all" {
		v.Set("lvl", st.Level)
	}
	if st.PlanID != 0 {
		v.Set("plan", strconv.FormatInt(st.PlanID, 10))
	}
	if len(st.Stage) > 0 {
		parts := make([]string, 0, len(st.Stage))
		for _, it := range st.Stage {
			parts = append(parts, fmt.Sprintf("%d:%d", it.SkillID, it.Level))
		}
		v.Set("stage", strings.Join(parts, ","))
	}
	return "/character/?" + v.Encode()
}

// hiddenFields lists the state's form fields (everything except
// the search text and the level filter, which are controls of
// their own) for the toolbar's GET form.
func (st overviewState) hiddenFields(characterID int64) []ovHiddenField {
	fields := []ovHiddenField{{Name: "character", Value: strconv.FormatInt(characterID, 10)}}
	add := func(name, value string) {
		if value != "" {
			fields = append(fields, ovHiddenField{Name: name, Value: value})
		}
	}
	if st.Surface != "sheet" {
		add("surface", st.Surface)
	}
	add("group", st.Group)
	if st.SkillID != 0 {
		add("skill", strconv.FormatInt(st.SkillID, 10))
	}
	if st.Clone != "active" {
		add("clone", st.Clone)
	}
	if st.PlanID != 0 {
		add("plan", strconv.FormatInt(st.PlanID, 10))
	}
	if len(st.Stage) > 0 {
		parts := make([]string, 0, len(st.Stage))
		for _, it := range st.Stage {
			parts = append(parts, fmt.Sprintf("%d:%d", it.SkillID, it.Level))
		}
		add("stage", strings.Join(parts, ","))
	}
	return fields
}

type ovHiddenField struct {
	Name  string
	Value string
}

// ---------------------------------------------------------------------------
// View model.
// ---------------------------------------------------------------------------

// overviewView is the assembled workspace the template renders.
type overviewView struct {
	TotalSPCompact string // "84.62M" for the page head

	DirAll    dirGroup
	DirGroups []dirGroup
	DirClones []dirCloneCard

	Tabs       []ovTab
	GroupName  string // the open group
	CrumbCap   string
	Hidden     []ovHiddenField
	LevelChips []ovChip
	PlanID     int64
	PlanName   string

	// Now Training strip (sheet surface).
	ShowTraining bool
	TrainName    string
	TrainToRoman string
	TrainPips    []ovPip
	TrainSPHour  string // "2,430 SP per hour", "" when unknown
	QueueChips   []queueChip
	QueueAfter   int
	TrainingDone string // raw finish for the countdown

	// The center table (sheet and browse surfaces).
	Rows           []ovSkillRow
	RowsNote       string // explains an empty table
	FoldedGroups   []dirGroup
	ShowBrowseNote bool // catalog still warming

	// Active-clone implant strip (sheet and browse surfaces).
	ShowImplantStrip bool
	ImplantCards     []implantRangeCard

	// Inspector.
	SkillInsp *inspectorSkill
	CloneInsp *cloneInspector
	Staged    *stagedPlanView

	// Timers card: the clone jump cooldown (the fatigue rows
	// ride the character view itself).
	CloneJumpLeft string // "in 14h 22m" or "ready"
	CloneJumpAt   string // formatted ready time
	CloneJumpRaw  string // RFC3339 for the live countdown
	SysSecClass   string // chip class for the character's own system

	// Planetary industry: per-colony pills for the summary
	// card and the full per-colony table under the panes.
	ColonyPills []colonyPill
	Colonies    []colonyDetailRow
}

// dirGroup is one skill-directory entry: a category with its
// trained SP, its share of the total, and its counts.
type dirGroup struct {
	Name       string
	SPCompact  string
	SPRaw      int64
	Trained    int
	Total      int
	SharePct   int    // 0..100, drives the share bar's width class
	ShareLabel string // "34% of SP"
	Selected   bool
	Href       string
}

type ovTab struct {
	Label string
	Count string
	On    bool
	Href  string
}

type ovChip struct {
	Label string
	On    bool
	Href  string
}

type queueChip struct {
	Num   int
	Label string // "Astrogeology → V"
}

// ovPip is one level box of a skill row: trained levels filled,
// queue and staged levels in the overlay color.
type ovPip struct {
	Class string // "" | "f" | "plan"
}

// ovSkillRow is one row of the sheet or browse table.
type ovSkillRow struct {
	TypeID      int64
	Name        string
	Group       string
	Level       int
	LevelTile   string // roman level, "0" when untrained
	Rank        string // "6x"
	AttrPair    string // "WIL / PER"
	SP          string
	MaxSP       string
	Mastered    bool
	Untrained   bool
	Pips        []ovPip
	Selected    bool
	SelectHref  string
	StageLevel  int // the level the stage button stages, 0 = none
	StageHref   string
	StagedLevel int
}

// implantRangeCard is one slot-range card of the active-clone
// implant strip.
type implantRangeCard struct {
	Range string // "1-5"
	Title string // "Attribute enhancers"
	Names string // comma-joined implant names in the range
	Note  string // "Slot 10 empty" and friends
	Href  string
}

// dirCloneCard is one Clone Bay card in the directory: the
// active clone or one jump clone.
type dirCloneCard struct {
	Key          string // "active" or the jump clone id
	CharacterID  int64
	Name         string
	Active       bool
	Location     string
	SystemName   string
	SystemSec    string
	SecClass     string
	ImplantCount int
	SlotBadges   []ovSlotBadge // the active clone's ten slots
	Selected     bool
	Href         string
	// Rename affordance (jump clones only).
	CloneID    int64
	CustomName string
	ESIName    string
}

type ovSlotBadge struct {
	Num    int
	Filled bool
}

// inspectorSkill is the inspector in skill mode.
type inspectorSkill struct {
	TypeID      int64
	Name        string
	Group       string
	Rank        string // "6x"
	Level       int
	LevelLabel  string // "Level 4 of 5"
	SP          string
	Description string
	Primary     string
	Secondary   string
	SPPerMinute string // "40.5", "" when attributes are unknown
	Milestones  []milestoneRow
	StageLevel  int // next level the stage button stages, 0 = none
	StageHref   string
	StagedLevel int
}

type milestoneRow struct {
	Label string // "Level 1"
	State string // "Trained · 1,500 SP" | "1,264,470 SP to go · about 19d 11h"
	Done  bool
}

// cloneInspector is the inspector in clone mode.
type cloneInspector struct {
	Title        string
	Location     string
	SystemName   string
	SystemSec    string
	SecClass     string
	ImplantCount int
	Enhancers    []implantSlotRow // slots 1-5
	Hardwirings  []implantSlotRow // slots 6-10
	// Rename affordance (jump clones only).
	CloneID    int64
	CustomName string
	ESIName    string
}

// implantSlotRow is one slot line of the clone inspector.
type implantSlotRow struct {
	Slot   int
	Name   string
	TypeID int64
	Bonus  string // "+3 Perception", "" when no known bonus
	Empty  bool
}

// stagedPlanView is the inspector's staged plan.
type stagedPlanView struct {
	Items      []stagedItemView
	TotalSP    string
	TotalTime  string
	StartsNote string // "Starts when your current queue finishes."
	PrereqNote string // "Includes 3 prerequisite skills."
	Export     string // EVE plan text, one "Name level" line per level
	SavePlanID int64
	ItemsField string // "skill:level,..." for the save form
}

// stagedItemView is one staged entry with its own figures from
// the plan computation.
type stagedItemView struct {
	SkillID  int64
	Name     string
	Level    int
	Roman    string
	SP       string // SP this item trains, "" when dropped
	Time     string
	Note     string // "Already trained to this level" and friends
	DropHref string
}

// colonyPill is one colony's expiry pill on the PI card.
type colonyPill struct {
	Planet string
	Label  string // "expired" | "3h 12m"
	Class  string // "bad" | "soon" | ""
}

// colonyDetailRow is one row of the per-colony table.
type colonyDetailRow struct {
	Planet     string
	System     string
	Type       string
	Upgrade    int64
	Pins       int64
	Outputs    string // extractor products and factory schematics
	Extractors int
	Factories  int
	Expiry     string // "in 2d 3h" | "expired" | ""
	Expired    bool
	Updated    string
	Warming    bool // layout snapshot not in yet
}

// ---------------------------------------------------------------------------
// Small formatters.
// ---------------------------------------------------------------------------

// compactSP renders a skill-point figure the directory's way:
// "28.5M", "950K", or the plain number under a thousand.
func compactSP(sp int64, decimals int) string {
	switch {
	case sp >= 1_000_000:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.*f", decimals, float64(sp)/1e6), "0"), ".") + "M"
	case sp >= 10_000:
		return fmt.Sprintf("%.0fK", float64(sp)/1e3)
	case sp >= 1_000:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", float64(sp)/1e3), "0"), ".") + "K"
	default:
		return esi.FormatInt(sp)
	}
}

// attrCode renders a training attribute's three-letter code
// (WIL, PER, INT, MEM, CHA): the first three letters of its
// name, upper-cased, which is the game's own abbreviation for
// all five attributes.
func attrCode(id int64) string {
	name := skillplan.AttributeName(id)
	if len(name) < 3 {
		return name
	}
	return strings.ToUpper(name[:3])
}

// secChipClass returns the site's own security color class
// (sec-0..sec-10) for a system security value; the chip markup
// pairs it with the shared .sec-pill class.
func secChipClass(sec float64) string {
	_, tier := secRound(sec)
	return fmt.Sprintf("sec-%d", tier)
}

// pipsFor builds the five level boxes of a row: trained levels
// filled, levels the queue or the staged plan covers in the
// overlay color.
func pipsFor(trained, coveredTo int) []ovPip {
	pips := make([]ovPip, 0, 5)
	for i := 1; i <= 5; i++ {
		p := ovPip{}
		switch {
		case i <= trained:
			p.Class = "f"
		case i <= coveredTo:
			p.Class = "plan"
		}
		pips = append(pips, p)
	}
	return pips
}

// attrSetOf converts the attributes snapshot to the plan
// engine's set; an absent or empty snapshot falls back to the
// flat set, flagged by the second return.
func attrSetOf(attrs esi.Attributes, known bool) (skillplan.AttrSet, bool) {
	if !known || (attrs.Charisma == 0 && attrs.Intelligence == 0 && attrs.Memory == 0 && attrs.Perception == 0 && attrs.Willpower == 0) {
		return skillplan.FlatAttrSet, false
	}
	return skillplan.AttrSet{
		Charisma: attrs.Charisma, Intelligence: attrs.Intelligence,
		Memory: attrs.Memory, Perception: attrs.Perception, Willpower: attrs.Willpower,
	}, true
}

// fmtRate renders an SP-per-minute rate without a trailing
// ".0": "40" or "40.5".
func fmtRate(x float64) string {
	if x == float64(int64(x)) {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'f', 1, 64)
}

// ---------------------------------------------------------------------------
// Assembly.
// ---------------------------------------------------------------------------

// ovSkillGraph is the overview's skillplan.Graph: SDE table
// reads, memoized per render (plan expansion revisits the same
// skills). It mirrors the skillplans module's graph; the math
// it feeds is the module's own skillplan.Compute.
type ovSkillGraph struct {
	app  *Application
	ctx  context.Context
	meta map[int64]skillplan.SkillMeta
	miss map[int64]bool
	reqs map[int64][]skillplan.Requirement
}

func newOvSkillGraph(app *Application, ctx context.Context) *ovSkillGraph {
	return &ovSkillGraph{
		app: app, ctx: ctx,
		meta: make(map[int64]skillplan.SkillMeta),
		miss: make(map[int64]bool),
		reqs: make(map[int64][]skillplan.Requirement),
	}
}

func (g *ovSkillGraph) Meta(skillID int64) (skillplan.SkillMeta, bool) {
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

func (g *ovSkillGraph) Requirements(typeID int64) []skillplan.Requirement {
	if rows, ok := g.reqs[typeID]; ok {
		return rows
	}
	dbRows, err := g.app.queries.ListSDERequirementsByType(g.ctx, typeID)
	if err != nil {
		logging.Errorf("character: requirements for type %d: %v", typeID, err)
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

// ovCharTraining assembles the plan engine's character input
// from the view's snapshot-derived maps: trained SP per skill,
// the levels the live queue completes, and the queue's end.
func ovCharTraining(view *characterView) skillplan.CharTraining {
	ct := skillplan.CharTraining{
		SP:       make(map[int64]int64, len(view.SkillSP)),
		QueuedTo: make(map[int64]int),
	}
	for id, sp := range view.SkillSP {
		ct.SP[id] = sp
	}
	ct.Unallocated = view.UnallocatedRaw
	for _, row := range view.Queue {
		if row.LevelNum > ct.QueuedTo[row.SkillID] {
			ct.QueuedTo[row.SkillID] = row.LevelNum
		}
	}
	if t, err := time.Parse(time.RFC3339, view.QueueEndRaw); err == nil {
		ct.QueueEnd = t
	}
	return ct
}

// fillOverview assembles the workspace from the sections the
// other fills loaded. It runs last in fillCharacterView.
func (app *Application) fillOverview(ctx context.Context, ch db.Character, view *characterView) {
	ov := &overviewView{}
	view.Overview = ov
	st := view.State
	charID := ch.CharacterID
	now := time.Now()

	ov.TotalSPCompact = compactSP(view.TotalSPRaw, 2)
	ov.Hidden = st.hiddenFields(charID)
	if view.SystemSec != "" {
		if sec, err := strconv.ParseFloat(view.SystemSec, 64); err == nil {
			ov.SysSecClass = secChipClass(sec)
		}
	}
	// The trained banner's quick-add buttons stage the next
	// level of each finished skill.
	for i := range view.TrainedEntries {
		e := &view.TrainedEntries[i]
		if e.SkillID != 0 && e.NextLevel > 0 {
			e.StageHref = st.with(func(c *overviewState) {
				c.stageSet(e.SkillID, e.NextLevel)
				c.SkillID = e.SkillID
			}).href(charID)
		}
	}

	catByID := make(map[int64]skillCatalogEntry, len(view.Catalog))
	catByName := make(map[string]skillCatalogEntry, len(view.Catalog))
	for _, e := range view.Catalog {
		catByID[e.TypeID] = e
		catByName[e.Name] = e
	}

	// The staged plan's computation runs first: the table's
	// pips overlay reads its levels, and the inspector shows
	// its outcome.
	var outcome *skillplan.Outcome
	if len(st.Stage) > 0 && view.SkillsKnown && !view.BrowseWarming {
		targets := make([]skillplan.Target, 0, len(st.Stage))
		for i, it := range st.Stage {
			targets = append(targets, skillplan.Target{SkillID: it.SkillID, Level: it.Level, Intent: i})
		}
		attrs, _ := attrSetOf(view.Attrs, view.AttrsKnown)
		oc := skillplan.Compute(newOvSkillGraph(app, ctx), targets, ovCharTraining(view), attrs, now)
		outcome = &oc
	}

	// Target plan: the URL's pick when it is one of the
	// character's plans, else the first plan.
	for _, p := range view.Plans {
		if ov.PlanID == 0 || p.ID == st.PlanID {
			ov.PlanID, ov.PlanName = p.ID, p.Name
		}
		if p.ID == st.PlanID {
			break
		}
	}

	selectedGroup := app.ovDirectory(ctx, view, ov, catByID)
	app.ovTabs(view, ov, selectedGroup)
	app.ovTrainingStrip(view, ov, catByID)
	app.ovRows(ctx, view, ov, catByID, selectedGroup)
	app.ovImplantStrip(view, ov)
	app.ovStagedView(ctx, view, ov, outcome, catByID)
	app.ovClones(ctx, view, ov)
	app.ovCloneJump(view, ov, catByName, now)
	app.ovColonies(ctx, view, ov)
}

// ovDirectory builds the skill directory (the All entry rides
// DirAll) and returns the selected group name.
func (app *Application) ovDirectory(ctx context.Context, view *characterView, ov *overviewView, catByID map[int64]skillCatalogEntry) string {
	st := view.State
	type agg struct {
		sp      int64
		trained int
		total   int
	}
	groups := map[string]*agg{}
	get := func(name string) *agg {
		a, ok := groups[name]
		if !ok {
			a = &agg{}
			groups[name] = a
		}
		return a
	}
	for _, e := range view.Catalog {
		get(e.GroupName).total++
	}
	// Trained counts and SP per group: the catalog names the
	// group; a skill the catalog lacks falls back to its sheet
	// section's name.
	sheetGroup := map[int64]string{}
	for _, sec := range view.SkillGroups {
		for _, row := range sec.Skills {
			sheetGroup[row.TypeID] = sec.Name
		}
	}
	for id, sp := range view.SkillSP {
		name := ""
		if e, ok := catByID[id]; ok {
			name = e.GroupName
		}
		if name == "" {
			name = sheetGroup[id]
		}
		if name == "" {
			name = "Ungrouped"
		}
		a := get(name)
		a.sp += sp
		a.trained++
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if groups[names[i]].sp != groups[names[j]].sp {
			return groups[names[i]].sp > groups[names[j]].sp
		}
		return names[i] < names[j]
	})

	selected := ""
	if st.Group != "" {
		if _, ok := groups[st.Group]; ok {
			selected = st.Group
		}
	}
	if selected == "" && view.TrainingSkillID != 0 {
		if e, ok := catByID[view.TrainingSkillID]; ok {
			if _, ok := groups[e.GroupName]; ok {
				selected = e.GroupName
			}
		}
	}
	if selected == "" && len(names) > 0 {
		selected = names[0]
	}

	for _, name := range names {
		a := groups[name]
		share := 0
		if view.TotalSPRaw > 0 {
			share = int(float64(a.sp)/float64(view.TotalSPRaw)*100 + 0.5)
		}
		total := a.total
		if total < a.trained {
			total = a.trained
		}
		g := dirGroup{
			Name:       name,
			SPCompact:  compactSP(a.sp, 1),
			SPRaw:      a.sp,
			Trained:    a.trained,
			Total:      total,
			SharePct:   share,
			ShareLabel: fmt.Sprintf("%d%% of SP", share),
			Selected:   name == selected && !st.AllGroups(),
		}
		g.Href = st.with(func(c *overviewState) {
			c.Group = name
			c.SkillID = 0
		}).href(view.CharacterID)
		ov.DirGroups = append(ov.DirGroups, g)
	}
	ov.DirAll = dirGroup{
		Name:       "All categories",
		SPCompact:  compactSP(view.TotalSPRaw, 1),
		SPRaw:      view.TotalSPRaw,
		Trained:    view.SkillsCount,
		Total:      len(view.Catalog),
		SharePct:   100,
		ShareLabel: "100% of SP",
		Selected:   st.AllGroups(),
	}
	ov.DirAll.Href = st.with(func(c *overviewState) {
		c.Group = "all"
		c.SkillID = 0
	}).href(view.CharacterID)
	ov.GroupName = selected
	if st.AllGroups() {
		ov.GroupName = "All categories"
	}
	return selected
}

// AllGroups reports whether the directory's All entry is the
// current selection (group=all in the URL).
func (st overviewState) AllGroups() bool { return st.Group == "all" }

// ovTabs builds the surface pills and the level filter chips.
func (app *Application) ovTabs(view *characterView, ov *overviewView, selectedGroup string) {
	st := view.State
	charID := view.CharacterID
	mk := func(label, count, surface string) ovTab {
		return ovTab{
			Label: label, Count: count, On: st.Surface == surface,
			Href: st.with(func(c *overviewState) { c.Surface = surface }).href(charID),
		}
	}
	ov.Tabs = []ovTab{
		mk("Trained skills", strconv.Itoa(view.SkillsCount), "sheet"),
		mk("Skill browse", strconv.Itoa(len(view.Catalog)), "browse"),
	}
	if view.ClonesKnown || view.ImplantsKnown {
		ov.Tabs = append(ov.Tabs, mk("Clones & implants", strconv.Itoa(1+len(view.JumpClones)), "clones"))
	}
	for _, chip := range []struct{ label, level string }{
		{"All levels", "all"}, {"Level V", "v"}, {"Levels I-IV", "mid"}, {"Untrained", "none"},
	} {
		ov.LevelChips = append(ov.LevelChips, ovChip{
			Label: chip.label, On: st.Level == chip.level,
			Href: st.with(func(c *overviewState) { c.Level = chip.level }).href(charID),
		})
	}
	ov.CrumbCap = "Sheet and browse render from stored snapshots"
}

// ovTrainingStrip fills the Now Training strip from the queue
// section the queue fill built.
func (app *Application) ovTrainingStrip(view *characterView, ov *overviewView, catByID map[int64]skillCatalogEntry) {
	if view.Training == "" || len(view.Queue) == 0 {
		return
	}
	head := view.Queue[0]
	ov.ShowTraining = true
	ov.TrainName = head.Skill
	ov.TrainToRoman = head.Level
	ov.TrainPips = pipsFor(head.TrainedLvl, head.LevelNum)
	ov.TrainingDone = view.TrainingFinish
	if e, ok := catByID[head.SkillID]; ok {
		attrs, known := attrSetOf(view.Attrs, view.AttrsKnown)
		if known {
			rate := skillplan.SPPerMinute(skillplan.SkillMeta{Rank: e.Rank, Primary: e.Primary, Secondary: e.Secondary}, attrs)
			ov.TrainSPHour = esi.FormatInt(int64(rate*60+0.5)) + " SP per hour"
		}
	}
	for i, row := range view.Queue[1:] {
		ov.QueueChips = append(ov.QueueChips, queueChip{Num: i + 1, Label: row.Skill + " → " + row.Level})
	}
	ov.QueueAfter = len(view.Queue) - 1
}

// ovRows builds the center table for the sheet and browse
// surfaces, the folded group rows under it, and the inspector's
// skill.
func (app *Application) ovRows(ctx context.Context, view *characterView, ov *overviewView, catByID map[int64]skillCatalogEntry, selectedGroup string) {
	st := view.State
	if st.Surface == "clones" {
		return
	}
	if st.Surface == "browse" && view.BrowseWarming {
		ov.ShowBrowseNote = true
		return
	}
	matchLevel := func(level int) bool {
		switch st.Level {
		case "v":
			return level == 5
		case "mid":
			return level >= 1 && level <= 4
		case "none":
			return level == 0
		}
		return true
	}
	matchQuery := func(name string) bool {
		return st.Query == "" || strings.Contains(strings.ToLower(name), strings.ToLower(st.Query))
	}
	inScope := func(group string) bool {
		return st.Query != "" || st.AllGroups() || group == selectedGroup
	}
	queuedTo := map[int64]int{}
	for _, row := range view.Queue {
		if row.LevelNum > queuedTo[row.SkillID] {
			queuedTo[row.SkillID] = row.LevelNum
		}
	}
	build := func(id int64, name, group string, level int, sp int64) (ovSkillRow, bool) {
		if !inScope(group) || !matchLevel(level) || !matchQuery(name) {
			return ovSkillRow{}, false
		}
		r := ovSkillRow{TypeID: id, Name: name, Group: group, Level: level, SP: esi.FormatInt(sp)}
		r.LevelTile = "0"
		if level > 0 {
			r.LevelTile = esi.RomanLevel(level)
		}
		r.Mastered = level >= 5
		r.Untrained = level == 0
		if e, ok := catByID[id]; ok {
			r.Rank = humanize.Rank(e.Rank)
			r.AttrPair = attrCode(e.Primary) + " / " + attrCode(e.Secondary)
			r.MaxSP = esi.FormatInt(skillplan.SPForLevel(e.Rank, 5))
		}
		covered := queuedTo[id]
		if sl := st.stageLevel(id); sl > covered {
			covered = sl
		}
		r.Pips = pipsFor(level, covered)
		r.StagedLevel = st.stageLevel(id)
		if level < 5 {
			next := level + 1
			if r.StagedLevel >= next {
				next = r.StagedLevel + 1
			}
			if next <= 5 {
				r.StageLevel = next
				r.StageHref = st.with(func(c *overviewState) {
					c.stageSet(id, next)
					c.SkillID = id
				}).href(view.CharacterID)
			}
		}
		r.SelectHref = st.with(func(c *overviewState) { c.SkillID = id }).href(view.CharacterID)
		return r, true
	}

	if st.Surface == "browse" {
		for _, e := range view.Catalog {
			if r, ok := build(e.TypeID, e.Name, e.GroupName, view.SkillLevels[e.TypeID], view.SkillSP[e.TypeID]); ok {
				ov.Rows = append(ov.Rows, r)
			}
		}
	} else {
		for _, sec := range view.SkillGroups {
			for _, sr := range sec.Skills {
				if r, ok := build(sr.TypeID, sr.Name, sec.Name, sr.TrainedLvl, view.SkillSP[sr.TypeID]); ok {
					ov.Rows = append(ov.Rows, r)
				}
			}
		}
	}

	// The inspector's skill: the URL's pick when it names a
	// catalog skill, else the training skill, else the first
	// row.
	target := st.SkillID
	if _, ok := catByID[target]; !ok {
		target = 0
	}
	if target == 0 && view.TrainingSkillID != 0 {
		if _, ok := catByID[view.TrainingSkillID]; ok {
			target = view.TrainingSkillID
		}
	}
	if target == 0 && len(ov.Rows) > 0 {
		target = ov.Rows[0].TypeID
	}
	for i := range ov.Rows {
		if ov.Rows[i].TypeID == target {
			ov.Rows[i].Selected = true
		}
	}
	if target != 0 {
		ov.SkillInsp = app.ovInspectorSkill(ctx, view, catByID[target])
	}

	if st.Query == "" && !st.AllGroups() {
		for _, g := range ov.DirGroups {
			if g.Name != selectedGroup {
				ov.FoldedGroups = append(ov.FoldedGroups, g)
			}
		}
	}
	if len(ov.Rows) == 0 {
		switch {
		case st.Query != "":
			ov.RowsNote = "No skills match that search. The search covers the trained sheet and the browse catalog."
		case st.Level == "none" && st.Surface == "sheet":
			ov.RowsNote = "Every skill in this view is trained. Untrained skills live in the browse catalog."
		case st.Level != "all":
			ov.RowsNote = "No skills at this level in the current view."
		default:
			ov.RowsNote = "No skills to show in this group yet."
		}
	}
}

// ovInspectorSkill builds the inspector for one catalog skill.
func (app *Application) ovInspectorSkill(ctx context.Context, view *characterView, e skillCatalogEntry) *inspectorSkill {
	st := view.State
	level := view.SkillLevels[e.TypeID]
	curSP := view.SkillSP[e.TypeID]
	meta := skillplan.SkillMeta{Rank: e.Rank, Primary: e.Primary, Secondary: e.Secondary}
	attrs, attrsKnown := attrSetOf(view.Attrs, view.AttrsKnown)
	insp := &inspectorSkill{
		TypeID:     e.TypeID,
		Name:       e.Name,
		Group:      e.GroupName,
		Rank:       humanize.Rank(e.Rank),
		Level:      level,
		LevelLabel: fmt.Sprintf("Level %d of 5", level),
		SP:         esi.FormatInt(curSP),
		Primary:    skillplan.AttributeName(e.Primary),
		Secondary:  skillplan.AttributeName(e.Secondary),
	}
	if attrsKnown {
		insp.SPPerMinute = fmtRate(skillplan.SPPerMinute(meta, attrs))
	}
	if detail, err := app.queries.GetTypeDetail(ctx, e.TypeID); err == nil {
		insp.Description = strings.TrimSpace(detail.Description)
	} else {
		app.notePageWantFromContext(ctx, pageWantTypeDescription, e.TypeID)
	}
	for l := 1; l <= 5; l++ {
		cum := skillplan.SPForLevel(e.Rank, l)
		m := milestoneRow{Label: "Level " + esi.RomanLevel(l)}
		if l <= level {
			m.Done = true
			m.State = "Trained · " + esi.FormatInt(cum) + " SP"
		} else {
			togo := cum - curSP
			m.State = esi.FormatInt(togo) + " SP to go"
			if attrsKnown {
				if secs := skillplan.TrainSeconds(meta, attrs, togo); secs > 0 {
					m.State += " · about " + humanDuration(time.Duration(secs)*time.Second)
				}
			}
		}
		insp.Milestones = append(insp.Milestones, m)
	}
	insp.StagedLevel = st.stageLevel(e.TypeID)
	next := level + 1
	if insp.StagedLevel >= next {
		next = insp.StagedLevel + 1
	}
	if next <= 5 {
		insp.StageLevel = next
		insp.StageHref = st.with(func(c *overviewState) {
			c.stageSet(e.TypeID, next)
			c.SkillID = e.TypeID
		}).href(view.CharacterID)
	}
	return insp
}

// ovImplantStrip builds the active clone's slot-range cards.
func (app *Application) ovImplantStrip(view *characterView, ov *overviewView) {
	st := view.State
	if st.Surface == "clones" || !view.ImplantsKnown {
		return
	}
	ov.ShowImplantStrip = true
	bySlot := map[int]implantRow{}
	for _, im := range view.Implants {
		bySlot[im.Slot] = im
	}
	for _, rng := range []struct {
		lo, hi int
		title  string
	}{
		{1, 5, "Attribute enhancers"},
		{6, 10, "Skill hardwirings"},
	} {
		card := implantRangeCard{
			Range: fmt.Sprintf("%d-%d", rng.lo, rng.hi),
			Title: rng.title,
			Href: st.with(func(c *overviewState) {
				c.Surface = "clones"
				c.Clone = "active"
			}).href(view.CharacterID),
		}
		var names, empty []string
		for slot := rng.lo; slot <= rng.hi; slot++ {
			if im, ok := bySlot[slot]; ok {
				names = append(names, im.Name)
			} else {
				empty = append(empty, strconv.Itoa(slot))
			}
		}
		card.Names = strings.Join(names, ", ")
		switch {
		case len(names) == 0:
			card.Note = "No implants in these slots"
		case len(empty) == 1:
			card.Note = "Slot " + empty[0] + " empty"
		case len(empty) > 1:
			card.Note = "Slots " + strings.Join(empty, ", ") + " empty"
		}
		ov.ImplantCards = append(ov.ImplantCards, card)
	}
}

// ovStagedView renders the computed staged plan into the
// inspector's staged section.
func (app *Application) ovStagedView(ctx context.Context, view *characterView, ov *overviewView, outcome *skillplan.Outcome, catByID map[int64]skillCatalogEntry) {
	st := view.State
	if len(st.Stage) == 0 {
		return
	}
	sv := &stagedPlanView{SavePlanID: ov.PlanID}
	ov.Staged = sv
	{
		parts := make([]string, 0, len(st.Stage))
		for _, it := range st.Stage {
			parts = append(parts, fmt.Sprintf("%d:%d", it.SkillID, it.Level))
		}
		sv.ItemsField = strings.Join(parts, ",")
	}
	if outcome == nil {
		// The computation could not run (catalog warming):
		// list the staged items plainly, without figures.
		for _, it := range st.Stage {
			name := fmt.Sprintf("Type #%d", it.SkillID)
			if e, ok := catByID[it.SkillID]; ok {
				name = e.Name
			}
			sv.Items = append(sv.Items, stagedItemView{
				SkillID: it.SkillID, Name: name, Level: it.Level, Roman: esi.RomanLevel(it.Level),
				Note:     "Skill data is still loading, so no times yet.",
				DropHref: st.with(func(c *overviewState) { c.stageDrop(it.SkillID) }).href(view.CharacterID),
			})
		}
		return
	}
	stepsBySkill := map[int64]skillplan.Step{}
	prereqs := 0
	for _, step := range outcome.Steps {
		stepsBySkill[step.SkillID] = step
		if step.Prereq {
			prereqs++
		}
	}
	dropped := map[int64]string{}
	for _, d := range outcome.Dropped {
		dropped[d.SkillID] = d.Reason
	}
	ids := make([]int64, 0, len(outcome.Steps))
	for _, step := range outcome.Steps {
		ids = append(ids, step.SkillID)
	}
	names := app.esi.CachedTypeNames(ctx, ids)
	nameFor := func(id int64) string {
		if e, ok := catByID[id]; ok {
			return e.Name
		}
		if n, ok := names[id]; ok {
			return n
		}
		return fmt.Sprintf("Type #%d", id)
	}
	var export strings.Builder
	for _, step := range outcome.Steps {
		for l := step.FromLevel + 1; l <= step.ToLevel; l++ {
			fmt.Fprintf(&export, "%s %d\n", nameFor(step.SkillID), l)
		}
	}
	sv.Export = strings.TrimRight(export.String(), "\n")
	sv.TotalSP = esi.FormatInt(outcome.TotalSP)
	sv.TotalTime = humanDuration(time.Duration(outcome.TotalSecond) * time.Second)
	if outcome.StartsAt.After(time.Now()) {
		sv.StartsNote = "Starts when your current queue finishes, " + formatFinish(outcome.StartsAt.UTC().Format(time.RFC3339)) + "."
	}
	if prereqs > 0 {
		plural := "s"
		if prereqs == 1 {
			plural = ""
		}
		sv.PrereqNote = fmt.Sprintf("Includes %d prerequisite skill%s the plan pulls in.", prereqs, plural)
	}
	for _, it := range st.Stage {
		item := stagedItemView{
			SkillID:  it.SkillID,
			Name:     nameFor(it.SkillID),
			Level:    it.Level,
			Roman:    esi.RomanLevel(it.Level),
			DropHref: st.with(func(c *overviewState) { c.stageDrop(it.SkillID) }).href(view.CharacterID),
		}
		if step, ok := stepsBySkill[it.SkillID]; ok && !step.Prereq {
			item.SP = esi.FormatInt(step.SPRemaining) + " SP"
			item.Time = humanDuration(time.Duration(step.Seconds) * time.Second)
		} else if reason, ok := dropped[it.SkillID]; ok {
			switch reason {
			case "already trained":
				item.Note = "Already trained to this level."
			case "already in queue":
				item.Note = "Already in the training queue."
			default:
				item.Note = "Covered by the rest of the plan."
			}
		} else {
			item.Note = "Covered by a prerequisite above."
		}
		sv.Items = append(sv.Items, item)
	}
}

// stageDrop removes one skill from the staged plan.
func (st *overviewState) stageDrop(skillID int64) {
	kept := st.Stage[:0]
	for _, it := range st.Stage {
		if it.SkillID != skillID {
			kept = append(kept, it)
		}
	}
	st.Stage = kept
}

// ---------------------------------------------------------------------------
// Clones.
// ---------------------------------------------------------------------------

// implantBonusAttrs maps the five character-attribute bonus
// dogma attributes to the attribute they raise (ESI dogma
// attribute ids 175-179, verified against the dogma endpoint).
var implantBonusAttrs = map[int64]int64{
	175: skillplan.AttrCharisma,
	176: skillplan.AttrIntelligence,
	177: skillplan.AttrMemory,
	178: skillplan.AttrPerception,
	179: skillplan.AttrWillpower,
}

// ovImplantBonuses reads the stored SDE dogma attributes of the
// given implant types and renders the attribute bonus lines
// ("+3 Perception"). Hardwiring percentage bonuses ride dozens
// of different attributes; only the five attribute bonuses get
// a line, the rest stay names, so nothing is mislabeled.
func (app *Application) ovImplantBonuses(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	rows, err := app.queries.ListSDETypeAttributesByIDs(ctx, ids)
	if err != nil {
		logging.Errorf("character: implant attributes: %v", err)
		return out
	}
	for _, row := range rows {
		attrID, ok := implantBonusAttrs[row.AttributeID]
		if !ok || row.Value == 0 {
			continue
		}
		line := fmt.Sprintf("+%s %s", fmtRate(row.Value), skillplan.AttributeName(attrID))
		if out[row.TypeID] != "" {
			out[row.TypeID] += " · "
		}
		out[row.TypeID] += line
	}
	return out
}

// ovCloneSystem resolves a jump clone location's system and
// security for the clone bay chip: station locations read the
// SDE station and system rows; structures have no SDE row, so
// they get no chip.
func (app *Application) ovCloneSystem(ctx context.Context, locID int64, locType string) (string, string, string) {
	if locType != "station" || locID == 0 {
		return "", "", ""
	}
	station, err := app.queries.GetSDEStation(ctx, locID)
	if err != nil {
		return "", "", ""
	}
	sys, err := app.queries.GetSDESystem(ctx, station.SystemID)
	if err != nil {
		return "", "", ""
	}
	return sys.Name, fmt.Sprintf("%.1f", sys.Security), secChipClass(sys.Security)
}

// ovClones builds the directory's Clone Bay cards and the clone
// inspector.
func (app *Application) ovClones(ctx context.Context, view *characterView, ov *overviewView) {
	st := view.State
	if !view.ClonesKnown && !view.ImplantsKnown {
		return
	}
	charID := view.CharacterID

	// One dogma read for every implant on the page.
	var allIDs []int64
	for _, im := range view.Implants {
		allIDs = append(allIDs, im.TypeID)
	}
	for _, jc := range view.JumpClones {
		for _, im := range jc.Implants {
			allIDs = append(allIDs, im.TypeID)
		}
	}
	bonuses := app.ovImplantBonuses(ctx, allIDs)

	activeLoc := view.DockedAt
	if activeLoc == "" {
		activeLoc = view.SystemName
	}
	active := dirCloneCard{
		Key:          "active",
		CharacterID:  charID,
		Name:         "Active clone",
		Active:       true,
		Location:     activeLoc,
		SystemName:   view.SystemName,
		ImplantCount: len(view.Implants),
		Selected:     st.Clone == "active",
		Href: st.with(func(c *overviewState) {
			c.Surface = "clones"
			c.Clone = "active"
		}).href(charID),
	}
	if view.SystemSec != "" {
		active.SystemSec = view.SystemSec
		if sec, err := strconv.ParseFloat(view.SystemSec, 64); err == nil {
			active.SecClass = secChipClass(sec)
		}
	}
	for slot := 1; slot <= 10; slot++ {
		active.SlotBadges = append(active.SlotBadges, ovSlotBadge{Num: slot, Filled: slot <= len(view.Implants)})
	}
	ov.DirClones = append(ov.DirClones, active)

	for _, jc := range view.JumpClones {
		key := strconv.FormatInt(jc.CloneID, 10)
		card := dirCloneCard{
			Key:          key,
			CharacterID:  charID,
			Name:         jc.DisplayName,
			Location:     jc.Location,
			ImplantCount: len(jc.Implants),
			Selected:     st.Clone == key,
			CloneID:      jc.CloneID,
			CustomName:   jc.CustomName,
			ESIName:      jc.Name,
			Href: st.with(func(c *overviewState) {
				c.Surface = "clones"
				c.Clone = key
			}).href(charID),
		}
		card.SystemName, card.SystemSec, card.SecClass = app.ovCloneSystem(ctx, jc.LocationID, jc.LocType)
		ov.DirClones = append(ov.DirClones, card)
	}

	// The inspector follows the directory's selection.
	slotRows := func(implants []implantRow, lo, hi int) []implantSlotRow {
		bySlot := map[int]implantRow{}
		for _, im := range implants {
			bySlot[im.Slot] = im
		}
		rows := make([]implantSlotRow, 0, hi-lo+1)
		for slot := lo; slot <= hi; slot++ {
			if im, ok := bySlot[slot]; ok {
				rows = append(rows, implantSlotRow{Slot: slot, Name: im.Name, TypeID: im.TypeID, Bonus: bonuses[im.TypeID]})
			} else {
				rows = append(rows, implantSlotRow{Slot: slot, Name: "Empty slot", Empty: true})
			}
		}
		return rows
	}
	if st.Clone == "active" {
		ci := &cloneInspector{
			Title:        "Active clone",
			Location:     activeLoc,
			SystemName:   view.SystemName,
			SystemSec:    active.SystemSec,
			SecClass:     active.SecClass,
			ImplantCount: len(view.Implants),
			Enhancers:    slotRows(view.Implants, 1, 5),
			Hardwirings:  slotRows(view.Implants, 6, 10),
		}
		ov.CloneInsp = ci
		return
	}
	for _, jc := range view.JumpClones {
		if strconv.FormatInt(jc.CloneID, 10) != st.Clone {
			continue
		}
		sysName, sysSec, secClass := app.ovCloneSystem(ctx, jc.LocationID, jc.LocType)
		ov.CloneInsp = &cloneInspector{
			Title:        jc.DisplayName,
			Location:     jc.Location,
			SystemName:   sysName,
			SystemSec:    sysSec,
			SecClass:     secClass,
			ImplantCount: len(jc.Implants),
			Enhancers:    slotRows(jc.Implants, 1, 5),
			Hardwirings:  slotRows(jc.Implants, 6, 10),
			CloneID:      jc.CloneID,
			CustomName:   jc.CustomName,
			ESIName:      jc.Name,
		}
		return
	}
	// A stale clone id in the URL falls back to the active pod.
	ov.CloneInsp = &cloneInspector{
		Title:        "Active clone",
		Location:     activeLoc,
		SystemName:   view.SystemName,
		SystemSec:    active.SystemSec,
		SecClass:     active.SecClass,
		ImplantCount: len(view.Implants),
		Enhancers:    slotRows(view.Implants, 1, 5),
		Hardwirings:  slotRows(view.Implants, 6, 10),
	}
}

// ovCloneJump derives the clone jump cooldown for the timers
// card from the stored last clone jump: 24 hours less one hour
// per level of Infomorph Synchronizing, the game's own rule.
func (app *Application) ovCloneJump(view *characterView, ov *overviewView, catByName map[string]skillCatalogEntry, now time.Time) {
	if !view.ClonesKnown || view.LastCloneJumpRaw == "" {
		return
	}
	last, err := time.Parse(time.RFC3339, view.LastCloneJumpRaw)
	if err != nil {
		return
	}
	level := 0
	if e, ok := catByName["Infomorph Synchronizing"]; ok {
		level = view.SkillLevels[e.TypeID]
	}
	ready := last.Add(time.Duration(24-level) * time.Hour)
	if ready.After(now) {
		ov.CloneJumpLeft = "in " + humanDuration(ready.Sub(now))
		ov.CloneJumpAt = formatFinish(ready.UTC().Format(time.RFC3339))
		ov.CloneJumpRaw = ready.UTC().Format(time.RFC3339)
	} else {
		ov.CloneJumpLeft = "ready"
	}
}

// ---------------------------------------------------------------------------
// Planetary industry.
// ---------------------------------------------------------------------------

// ovColonies builds the PI card's per-colony expiry pills and
// the full per-colony table from the stored colony and layout
// snapshots, the same rows the colonies page renders.
func (app *Application) ovColonies(ctx context.Context, view *characterView, ov *overviewView) {
	if !view.PIKnown {
		return
	}
	var colonies esi.Colonies
	if !app.loadCorpSnapshot(ctx, view.CharacterID, esi.SnapPlanets, &colonies) {
		return
	}
	now := time.Now()
	rows := make([]colonyDetailRow, 0, len(colonies))
	for _, colony := range colonies {
		row := colonyDetailRow{
			Planet:  app.planetDisplayName(ctx, colony.PlanetID),
			System:  app.locationTitle(ctx, colony.SolarSystemID, "solar_system"),
			Type:    displayPlanetType(colony.PlanetType),
			Upgrade: colony.UpgradeLevel,
			Pins:    colony.NumPins,
			Updated: formatFinish(colony.LastUpdate),
		}
		layout, ok := app.loadPlanetLayout(ctx, view.CharacterID, colony.PlanetID)
		if !ok {
			row.Warming = true
			rows = append(rows, row)
			continue
		}
		exRows, facRows, _ := app.colonyLayoutRows(ctx, layout, now)
		row.Extractors = len(exRows)
		row.Factories = len(facRows)
		var outputs []string
		seenOut := map[string]bool{}
		for _, ex := range exRows {
			if !seenOut[ex.Product] {
				seenOut[ex.Product] = true
				outputs = append(outputs, ex.Product)
			}
		}
		if len(facRows) > 0 {
			var facs []string
			seenFac := map[string]bool{}
			for _, f := range facRows {
				if f.Schematic != "" && !seenFac[f.Schematic] {
					seenFac[f.Schematic] = true
					facs = append(facs, f.Schematic)
				}
			}
			if len(facs) > 0 {
				outputs = append(outputs, strings.Join(facs, ", "))
			}
		}
		row.Outputs = strings.Join(outputs, " → ")
		// Soonest extractor expiry, for the row and the pill.
		var soonest time.Time
		soonestOK := false
		for _, ex := range layoutExtractors(layout) {
			if !ex.ExpiryOK {
				continue
			}
			if ex.Expiry.Before(now) {
				row.Expired = true
			}
			if !soonestOK || ex.Expiry.Before(soonest) {
				soonest, soonestOK = ex.Expiry, true
			}
		}
		pill := colonyPill{Planet: row.Planet}
		switch {
		case row.Expired:
			row.Expiry = "expired"
			pill.Label, pill.Class = "expired", "bad"
			ov.ColonyPills = append(ov.ColonyPills, pill)
		case soonestOK:
			left := soonest.Sub(now)
			row.Expiry = "in " + humanDuration(left)
			pill.Label = humanDuration(left)
			if left < 24*time.Hour {
				pill.Class = "soon"
			}
			ov.ColonyPills = append(ov.ColonyPills, pill)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].System != rows[j].System {
			return rows[i].System < rows[j].System
		}
		return rows[i].Planet < rows[j].Planet
	})
	ov.Colonies = rows
}
