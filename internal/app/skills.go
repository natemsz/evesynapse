package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// skillsView is the Skill Sheet page body for one character.
// Loaded is false when the skills snapshot could not be produced
// at all; Warming marks the cold-start case (no snapshot yet —
// the worker is still importing), which gets friendlier copy
// than a hard failure. Queue data degrades independently: a
// failed queue fetch leaves Queue empty and Training blank.
type skillsView struct {
	CharacterName string
	CharacterID   int64
	Loaded        bool
	Warming       bool
	TotalSP       string // thousands-separated
	UnallocatedSP string // thousands-separated, "" when zero
	LevelVCount   int    // skills trained to level V
	SkillsKnown   int
	Training      string // currently-training line, "" when paused/empty
	Queue         []skillQueueRow
	CompletesAt   string // finish time of the last queue entry, "" when queue empty
	Groups        []skillGroupSection

	// Browse (Phase 4): the full skill catalog from the SDE
	// graph, with plan quick-add forms. BrowseWarming marks the
	// pre-import state; BrowseGroups stay empty then.
	Browse        []browseSkillGroup
	BrowseWarming bool
	Plans         []skillPlanSummary
}

// browseSkillGroup is one catalog block of the Browse section.
type browseSkillGroup struct {
	Name   string
	Skills []browseSkillRow
}

// browseSkillRow is one catalog skill with the character's state.
type browseSkillRow struct {
	TypeID    int64
	Name      string
	Rank      string // "2x"
	Trained   string // roman level, "—" when untrained
	TrainedLvl int   // numeric trained level, 0 when untrained
	SP        string // formatted SP in skill, "" when untrained
	Primary   string // attribute name
	Secondary string
	Prereqs   int // direct prerequisite skills
}

// skillGroupSection is one skill-category block of the sheet.
type skillGroupSection struct {
	Name       string
	SubtotalSP string // thousands-separated
	Skills     []skillRow
}

// skillQueueRow is one line of the skill-queue table.
type skillQueueRow struct {
	Num      int // 1-based queue position for display
	Skill    string
	SkillID  int64
	Level    string // roman level the entry completes
	Finishes string // UTC timestamp, "" when ESI gives none
	// LevelNum is the numeric finished level; TrainedLvl the
	// numeric trained level (0 when the skill is untrained);
	// State is "training" for the position-0 entry and "queued"
	// for the rest, driving the 5-box level indicator. Each row
	// marks queued only up to its own level, not the highest
	// queued level for the skill.
	LevelNum   int
	TrainedLvl int
	State      string
}

// handleSkills used to render the standalone Skill Sheet. The
// sheet now lives on the unified character page (/character/),
// so this route redirects there, preserving an explicit
// ?character= pick.
func (app *Application) handleSkills(w http.ResponseWriter, r *http.Request) {
	target := "/character/"
	if cid := r.URL.Query().Get("character"); cid != "" {
		target = "/character/?character=" + cid
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fillBrowse loads the SDE skill catalog (grouped, with the
// character's trained state) plus the character's plans for the
// quick-add forms. A missing SDE skill graph renders the honest
// warming state instead of an empty catalog.
func (app *Application) fillBrowse(ctx context.Context, view *skillsView, ch db.Character, skills esi.Skills, userID int64) {
	if n, err := app.queries.CountSDESkillMeta(ctx); err != nil || n == 0 {
		if err != nil {
			log.Printf("skills: count skill meta: %v", err)
		}
		view.BrowseWarming = true
		return
	}
	catalog, err := app.queries.ListSDESkillCatalog(ctx)
	if err != nil {
		log.Printf("skills: skill catalog: %v", err)
		view.BrowseWarming = true
		return
	}

	trained := make(map[int64]esi.Skill, len(skills.Skills))
	for _, s := range skills.Skills {
		trained[s.SkillID] = s
	}
	ids := make([]int64, 0, len(catalog))
	for _, row := range catalog {
		ids = append(ids, row.TypeID)
	}
	prereqCounts := make(map[int64]int)
	if rows, err := app.queries.ListSDERequirementsByTypes(ctx, ids); err == nil {
		for _, r := range rows {
			prereqCounts[r.TypeID]++
		}
	} else {
		log.Printf("skills: catalog requirements: %v", err)
	}

	var groups []browseSkillGroup
	for _, row := range catalog {
		if len(groups) == 0 || groups[len(groups)-1].Name != row.GroupName {
			groups = append(groups, browseSkillGroup{Name: row.GroupName})
		}
		brow := browseSkillRow{
			TypeID:    row.TypeID,
			Name:      row.Name,
			Rank:      formatRank(row.Rank),
			Trained:   "—",
			Primary:   attributeName(row.PrimaryAttr),
			Secondary: attributeName(row.SecondaryAttr),
			Prereqs:   prereqCounts[row.TypeID],
		}
		if s, ok := trained[row.TypeID]; ok {
			brow.Trained = esi.RomanLevel(s.TrainedSkillLevel)
			brow.TrainedLvl = s.TrainedSkillLevel
			brow.SP = esi.FormatInt(s.SkillpointsInSkill)
		}
		groups[len(groups)-1].Skills = append(groups[len(groups)-1].Skills, brow)
	}
	view.Browse = groups

	plans, err := app.queries.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: userID, CharacterID: ch.CharacterID})
	if err != nil {
		log.Printf("skills: plans for character %d: %v", ch.CharacterID, err)
		return
	}
	for _, p := range plans {
		items, err := app.queries.ListSkillPlanItems(ctx, p.ID)
		if err != nil {
			continue
		}
		view.Plans = append(view.Plans, skillPlanSummary{ID: p.ID, Name: p.Name, Items: len(items), Character: p.CharacterID})
	}
}

// fillQueue builds the queue block: one row per entry in queue
// order, the currently-training line (the position-0 entry, same
// semantics as the home page), and the completion time of the
// final entry.
func (app *Application) fillQueue(ctx context.Context, view *skillsView, queue esi.Skillqueue) {
	sorted := append(esi.Skillqueue(nil), queue...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].QueuePosition < sorted[j].QueuePosition
	})

	ids := make([]int64, 0, len(sorted))
	for _, entry := range sorted {
		ids = append(ids, entry.SkillID)
	}
	names := app.esi.CachedTypeNames(ctx, ids)
	nameFor := func(id int64) string {
		if name, ok := names[id]; ok {
			return name
		}
		return fmt.Sprintf("Type #%d", id)
	}

	for i, entry := range sorted {
		view.Queue = append(view.Queue, skillQueueRow{
			Num:      i + 1,
			Skill:   nameFor(entry.SkillID),
			SkillID:  entry.SkillID,
			Level:    esi.RomanLevel(entry.FinishedLevel),
			LevelNum: entry.FinishedLevel,
			Finishes: formatFinish(entry.FinishDate),
		})
		if entry.QueuePosition == 0 && view.Training == "" {
			line := fmt.Sprintf("%s %s", nameFor(entry.SkillID), esi.RomanLevel(entry.FinishedLevel))
			if f := formatFinish(entry.FinishDate); f != "" {
				line += " — finishes " + f
			}
			view.Training = line
		}
	}

	if n := len(sorted); n > 0 {
		view.CompletesAt = formatFinish(sorted[n-1].FinishDate)
	}
}

// formatFinish renders an ESI RFC3339 timestamp the way the home
// page does; unparseable values pass through, empty stays empty.
func formatFinish(raw string) string {
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return raw
}

// fillSkillSections computes the header stats and the per-group
// skill tables (every skill, heaviest first, SP subtotal per
// group).
func (app *Application) fillSkillSections(ctx context.Context, view *skillsView, skills esi.Skills) {
	view.TotalSP = esi.FormatInt(skills.TotalSP)
	if skills.UnallocatedSP > 0 {
		view.UnallocatedSP = esi.FormatInt(skills.UnallocatedSP)
	}
	view.SkillsKnown = len(skills.Skills)

	ids := make([]int64, 0, len(skills.Skills))
	for _, s := range skills.Skills {
		ids = append(ids, s.SkillID)
		if s.TrainedSkillLevel == 5 {
			view.LevelVCount++
		}
	}
	// Cache-only resolution: names, type→group links and group
	// names all come from the worker-warmed caches; anything still
	// missing renders as "Type #<id>" / "Ungrouped" until a later
	// worker pass fills it in.
	names := app.esi.CachedTypeNames(ctx, ids)
	groupsByType := app.esi.CachedTypeGroups(ctx, ids)

	groupIDsSeen := make(map[int64]bool)
	for _, gid := range groupsByType {
		if gid > 0 {
			groupIDsSeen[gid] = true
		}
	}
	groupIDs := make([]int64, 0, len(groupIDsSeen))
	for gid := range groupIDsSeen {
		groupIDs = append(groupIDs, gid)
	}
	groupNames := app.esi.CachedGroupNames(ctx, groupIDs)

	type workingRow struct {
		row skillRow
		sp  int64
	}
	type workingSection struct {
		name string
		sp   int64
		rows []workingRow
	}
	byGroup := make(map[int64]*workingSection)
	var sections []*workingSection
	for _, s := range skills.Skills {
		gid := groupsByType[s.SkillID] // 0 when unresolved
		sec, ok := byGroup[gid]
		if !ok {
			name := "Ungrouped"
			if gid > 0 {
				if gn, ok := groupNames[gid]; ok {
					name = gn
				} else {
					name = fmt.Sprintf("Group #%d", gid)
				}
			}
			sec = &workingSection{name: name}
			byGroup[gid] = sec
			sections = append(sections, sec)
		}
		skillName, ok := names[s.SkillID]
		if !ok {
			skillName = fmt.Sprintf("Type #%d", s.SkillID)
		}
		sec.sp += s.SkillpointsInSkill
		sec.rows = append(sec.rows, workingRow{
			row: skillRow{
				Name:       skillName,
				TypeID:     s.SkillID,
				Trained:    esi.RomanLevel(s.TrainedSkillLevel),
				Active:     esi.RomanLevel(s.ActiveSkillLevel),
				SP:         esi.FormatInt(s.SkillpointsInSkill),
				TrainedLvl: s.TrainedSkillLevel,
			},
			sp: s.SkillpointsInSkill,
		})
	}

	for _, sec := range sections {
		sort.Slice(sec.rows, func(i, j int) bool {
			if sec.rows[i].sp != sec.rows[j].sp {
				return sec.rows[i].sp > sec.rows[j].sp
			}
			return sec.rows[i].row.Name < sec.rows[j].row.Name
		})
	}
	sort.Slice(sections, func(i, j int) bool {
		// "Ungrouped" (unresolved group IDs) always sits last.
		if sections[i].name == "Ungrouped" {
			return false
		}
		if sections[j].name == "Ungrouped" {
			return true
		}
		return sections[i].name < sections[j].name
	})
	for _, sec := range sections {
		out := skillGroupSection{
			Name:       sec.name,
			SubtotalSP: esi.FormatInt(sec.sp),
		}
		for _, wr := range sec.rows {
			out.Skills = append(out.Skills, wr.row)
		}
		view.Groups = append(view.Groups, out)
	}
}
