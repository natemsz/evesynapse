package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// skillsView is the Skill Sheet page body for one character.
// Loaded is false when the skills snapshot could not be produced
// at all; the template dims to an unavailable note. Queue data
// degrades independently: a failed queue fetch leaves Queue
// empty and Training blank.
type skillsView struct {
	CharacterName string
	Loaded        bool
	TotalSP       string // thousands-separated
	UnallocatedSP string // thousands-separated, "" when zero
	LevelVCount   int    // skills trained to level V
	SkillsKnown   int
	Training      string // currently-training line, "" when paused/empty
	Queue         []skillQueueRow
	CompletesAt   string // finish time of the last queue entry, "" when queue empty
	Groups        []skillGroupSection
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
	Level    string // roman level the entry completes
	Finishes string // UTC timestamp, "" when ESI gives none
}

// maxGroupNameLookups bounds the number of ESI /universe/groups
// lookups a single skill-sheet render will make.
const maxGroupNameLookups = 40

// handleSkills renders the full Skill Sheet for one of the
// signed-in user's characters (switchable via ?character=). All
// data comes through the snapshot cache (skills + skillqueue), so
// the page only calls ESI inside the cache window.
func (app *application) handleSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.ssoConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		app.render(w, http.StatusOK, "skills.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("skills: list characters for user %d: %v", userID, err)
		data.Error = "Could not load skill data; check the server log."
		app.render(w, http.StatusOK, "skills.html", data)
		return
	}
	if len(characters) == 0 {
		app.render(w, http.StatusOK, "skills.html", data)
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
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want == 0 || !pick(want) {
		if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid == 0 || !pick(sid) {
			active = characters[0]
		}
	}
	for _, ch := range characters {
		data.SkillsChars = append(data.SkillsChars, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
		})
	}

	view := &skillsView{CharacterName: active.Name}
	data.Skills = view

	var skills esiSkills
	if err := app.getCached(ctx, active, snapSkills, &skills); err != nil {
		log.Printf("skills: load skills for character %d: %v", active.CharacterID, err)
		app.render(w, http.StatusOK, "skills.html", data)
		return
	}
	view.Loaded = true

	// The queue degrades on its own: a failed fetch leaves the
	// skills sections intact with an empty queue block.
	var queue esiSkillqueue
	if err := app.getCached(ctx, active, snapSkillqueue, &queue); err != nil {
		log.Printf("skills: load queue for character %d: %v", active.CharacterID, err)
	} else {
		app.fillQueue(ctx, view, queue)
	}

	app.fillSkillSections(ctx, view, skills)
	app.render(w, http.StatusOK, "skills.html", data)
}

// fillQueue builds the queue block: one row per entry in queue
// order, the currently-training line (the position-0 entry, same
// semantics as the home page), and the completion time of the
// final entry.
func (app *application) fillQueue(ctx context.Context, view *skillsView, queue esiSkillqueue) {
	sorted := append(esiSkillqueue(nil), queue...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].QueuePosition < sorted[j].QueuePosition
	})

	ids := make([]int64, 0, len(sorted))
	for _, entry := range sorted {
		ids = append(ids, entry.SkillID)
	}
	names := app.resolveTypeNames(ctx, ids)
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
			Level:    romanLevel(entry.FinishedLevel),
			Finishes: formatFinish(entry.FinishDate),
		})
		if entry.QueuePosition == 0 && view.Training == "" {
			line := fmt.Sprintf("%s %s", nameFor(entry.SkillID), romanLevel(entry.FinishedLevel))
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
func (app *application) fillSkillSections(ctx context.Context, view *skillsView, skills esiSkills) {
	view.TotalSP = formatInt(skills.TotalSP)
	if skills.UnallocatedSP > 0 {
		view.UnallocatedSP = formatInt(skills.UnallocatedSP)
	}
	view.SkillsKnown = len(skills.Skills)

	ids := make([]int64, 0, len(skills.Skills))
	for _, s := range skills.Skills {
		ids = append(ids, s.SkillID)
		if s.TrainedSkillLevel == 5 {
			view.LevelVCount++
		}
	}
	// Names first: the fetch also populates the type→group cache
	// resolveTypeGroups reads, so most group IDs come free.
	names := app.resolveTypeNames(ctx, ids)
	groupsByType := app.resolveTypeGroups(ctx, ids)

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
	groupNames := app.resolveGroupNames(ctx, groupIDs)

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
				Trained: romanLevel(s.TrainedSkillLevel),
				Active:  romanLevel(s.ActiveSkillLevel),
				SP:      formatInt(s.SkillpointsInSkill),
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
			SubtotalSP: formatInt(sec.sp),
		}
		for _, wr := range sec.rows {
			out.Skills = append(out.Skills, wr.row)
		}
		view.Groups = append(view.Groups, out)
	}
}

// resolveGroupNames resolves skill-group ID → display name via
// GET /universe/groups/{id}/ (public), cached in-process, capped
// per render. Unresolved groups are absent from the result; the
// caller falls back to "Group #<id>".
func (app *application) resolveGroupNames(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))

	app.groupNamesMu.Lock()
	var missing []int64
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if name, ok := app.groupNames[id]; ok {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	app.groupNamesMu.Unlock()

	lookups := 0
	for _, id := range missing {
		if lookups >= maxGroupNameLookups {
			break
		}
		lookups++
		var g esiGroup
		if err := esiGet(ctx, "", fmt.Sprintf("/universe/groups/%d/", id), &g); err != nil {
			log.Printf("skills: group name lookup %d: %v", id, err)
			continue
		}
		if g.Name == "" {
			continue
		}
		out[id] = g.Name
		app.groupNamesMu.Lock()
		app.groupNames[id] = g.Name
		app.groupNamesMu.Unlock()
	}
	return out
}
