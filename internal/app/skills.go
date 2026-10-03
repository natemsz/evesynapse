package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
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
}

// handleSkills renders the full Skill Sheet for one of the
// signed-in user's characters (switchable via ?character=). All
// data comes through the snapshot cache (skills + skillqueue),
// and every name is resolved from local caches only — the worker
// pre-warms snapshots and names, so this page never waits on ESI
// name lookups.
func (app *Application) handleSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		app.render(ctx, w, http.StatusOK, "skills.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("skills: list characters for user %d: %v", userID, err)
		data.Error = "Could not load skill data; check the server log."
		app.render(ctx, w, http.StatusOK, "skills.html", data)
		return
	}
	if len(characters) == 0 {
		app.render(ctx, w, http.StatusOK, "skills.html", data)
		return
	}

	// Active character: an explicit ?character= the user owns wins,
	// then the session character, then the first linked character.
	active := characters[0]
	pick := func(id int64) bool {
		for _, ch := range characters {
			if ch.CharacterID == id {
				active = ch
				return true
			}
		}
		return false
	}
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want != 0 && pick(want) {
		// An explicit pick becomes the session's acting
		// character (see pickCharacter in character.go).
		app.sessions.Put(ctx, sessionCharacterID, int(active.CharacterID))
		app.sessions.Put(ctx, sessionCharacterName, active.Name)
	} else if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid == 0 || !pick(sid) {
		active = characters[0]
	}
	for _, ch := range characters {
		data.SkillsChars = append(data.SkillsChars, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
		})
	}

	view := &skillsView{CharacterName: active.Name, CharacterID: active.CharacterID}
	data.Skills = view

	var skills esi.Skills
	if err := app.esi.GetCached(ctx, active, esi.SnapSkills, &skills); err != nil {
		log.Printf("skills: load skills for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapSkills}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(ctx, w, http.StatusOK, "skills.html", data)
		return
	}
	view.Loaded = true

	// The queue degrades on its own: a failed fetch leaves the
	// skills sections intact with an empty queue block.
	var queue esi.Skillqueue
	if err := app.esi.GetCached(ctx, active, esi.SnapSkillqueue, &queue); err != nil {
		log.Printf("skills: load queue for character %d: %v", active.CharacterID, err)
	} else {
		app.fillQueue(ctx, view, queue)
	}

	app.fillSkillSections(ctx, view, skills)
	app.fillBrowse(ctx, view, active, skills, userID)
	app.render(ctx, w, http.StatusOK, "skills.html", data)
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
			Skill:    nameFor(entry.SkillID),
			SkillID:  entry.SkillID,
			Level:    esi.RomanLevel(entry.FinishedLevel),
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
				Name:    skillName,
				TypeID:  s.SkillID,
				Trained: esi.RomanLevel(s.TrainedSkillLevel),
				Active:  esi.RomanLevel(s.ActiveSkillLevel),
				SP:      esi.FormatInt(s.SkillpointsInSkill),
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
