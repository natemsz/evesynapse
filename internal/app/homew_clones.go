package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetClones, Title: "Clones and Fatigue", Category: wcatPilots, Feature: "clones",
		Href:        "/characters/",
		Description: "Jump fatigue, when each pilot can clone jump again, and where the jump clones wait.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many pilots carry jump fatigue and when the last of it clears."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Each pilot's fatigue, next clone jump and home station."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Each pilot beside where their jump clones wait."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds each pilot's active implants."},
		},
		Needs:   []string{esi.SnapClones, esi.SnapImplants, esi.SnapFatigue, esi.SnapSkills},
		Options: []widgetOption{optScope()},
		Build:   buildClonesWidget,
	})
}

// infomorphSynchronizingID is the skill that takes an hour per level
// off the 24-hour clone jump cooldown.
const infomorphSynchronizingID = 33399

// clonesWidgetJumpClones is the most jump clones the widget lists.
const clonesWidgetJumpClones = 30

// clonePilotRow is one pilot's timers.
type clonePilotRow struct {
	CharID      int64
	Name        string
	Fatigued    bool
	FatigueLeft string
	FatigueRaw  string // RFC3339, for the countdown
	FatigueAt   string
	CloneKnown  bool
	CloneReady  bool
	CloneLeft   string
	CloneRaw    string
	CloneAt     string
	Home        string
	JumpClones  int
	Implants    []implantRow
	fatigueEnd  time.Time
}

// jumpCloneRow is one jump clone and where it waits.
type jumpCloneRow struct {
	CharID   int64
	Pilot    string
	Name     string
	Location string
	Implants int
}

// clonesWidgetView is what the Clones and Fatigue forms draw.
type clonesWidgetView struct {
	Lead     wLead
	Longest  *clonePilotRow // the pilot whose fatigue clears last
	Fatigued int
	Pilots   []clonePilotRow
	Clones   []jumpCloneRow
	Known    bool
	Empty    wEmpty
}

func buildClonesWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &clonesWidgetView{Empty: wEmpty{Text: "Clone and fatigue details have not loaded yet.", Step: "Open your pilots", Href: "/characters/"}}
	now := in.Now.UTC()
	large := in.Draws("large")
	detail := in.Draws("tall") || in.Draws("medium") || large
	for _, b := range in.Bundles {
		if b.clones == nil && b.fatigue == nil && !b.implantsKnown {
			continue
		}
		v.Known = true
		row := clonePilotRow{CharID: b.ch.CharacterID, Name: b.ch.Name}
		if b.fatigue != nil {
			if end, ok := parseRFC3339(b.fatigue.JumpFatigueExpireDate); ok && end.After(now) {
				row.Fatigued, row.fatigueEnd = true, end
				row.FatigueLeft = humanDuration(end.Sub(now))
				row.FatigueRaw = end.Format(time.RFC3339)
				row.FatigueAt = formatFinish(row.FatigueRaw)
				v.Fatigued++
			}
		}
		if b.clones != nil && detail {
			row.CloneKnown, row.CloneReady = true, true
			if last, ok := parseRFC3339(b.clones.LastCloneJumpDate); ok {
				if ready := last.Add(time.Duration(24-infomorphLevel(b)) * time.Hour); ready.After(now) {
					row.CloneReady = false
					row.CloneLeft = humanDuration(ready.Sub(now))
					row.CloneRaw = ready.UTC().Format(time.RFC3339)
					row.CloneAt = formatFinish(row.CloneRaw)
				}
			}
			if b.clones.HomeLocation.LocationID > 0 {
				row.Home = app.locationTitle(ctx, b.clones.HomeLocation.LocationID, b.clones.HomeLocation.LocationType)
			}
			row.JumpClones = len(b.clones.JumpClones)
		}
		if large && b.implantsKnown {
			names := app.esi.CachedTypeNames(ctx, b.implants)
			for i, id := range b.implants {
				name, ok := names[id]
				if !ok {
					name = fmt.Sprintf("Type #%d", id)
				}
				row.Implants = append(row.Implants, implantRow{Slot: i + 1, Name: name, TypeID: id})
			}
		}
		if (in.Draws("medium") || large) && b.clones != nil {
			for i, jc := range b.clones.JumpClones {
				if len(v.Clones) >= clonesWidgetJumpClones {
					break
				}
				name := jc.Name
				if name == "" {
					name = "Jump clone " + strconv.Itoa(i+1)
				}
				v.Clones = append(v.Clones, jumpCloneRow{CharID: b.ch.CharacterID, Pilot: b.ch.Name, Name: name,
					Location: app.locationTitle(ctx, jc.LocationID, jc.LocationType), Implants: len(jc.Implants)})
			}
		}
		v.Pilots = append(v.Pilots, row)
	}
	// Fatigued pilots first, the longest fatigue at the top; then by name.
	sort.SliceStable(v.Pilots, func(i, j int) bool {
		a, b := v.Pilots[i], v.Pilots[j]
		if a.Fatigued != b.Fatigued {
			return a.Fatigued
		}
		if a.Fatigued && !a.fatigueEnd.Equal(b.fatigueEnd) {
			return a.fatigueEnd.After(b.fatigueEnd)
		}
		return a.Name < b.Name
	})
	if v.Fatigued > 0 {
		v.Longest = &v.Pilots[0]
		v.Lead = wLead{Label: "Pilots with jump fatigue", Value: strconv.Itoa(v.Fatigued),
			Full: pluralCount(int64(v.Fatigued), "pilot with jump fatigue", "pilots with jump fatigue")}
	} else {
		v.Lead = wLead{Label: "Jump fatigue", Value: "None", Full: "No pilot carries jump fatigue"}
	}
	return v
}

// infomorphLevel is the pilot's trained Infomorph Synchronizing level,
// 0 when the skills have not loaded.
func infomorphLevel(b *charSnaps) int {
	if !b.skillsKnown {
		return 0
	}
	for _, s := range b.skills.Skills {
		if s.SkillID == infomorphSynchronizingID {
			return s.ActiveSkillLevel
		}
	}
	return 0
}
