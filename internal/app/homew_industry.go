package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"evesynapse/internal/app/contracts"
	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetIndustry, Title: "Industry", Category: wcatIndustry, Feature: "industry",
		Href:        "/industry/jobs/",
		Description: "Industry jobs across your characters: what is ready, what finishes next, and how many slots are in use.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Jobs ready and running, and the next delivery."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "Slots in use for each kind of job, and the next delivery."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Your jobs, ready ones first, with time left."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Slots in use over your jobs."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Your jobs with their progress, and each character's slots."},
		},
		Needs: []string{esi.SnapIndustryJobs, esi.SnapSkills},
		Options: []widgetOption{
			optScope(),
			optChoice("activity", "Jobs", "all", "all:Every kind", "manufacturing:Manufacturing", "science:Science", "reactions:Reactions"),
		},
		Build: buildIndustryWidget,
	})
}

// industryJobCap is how many jobs the list carries; the card shows
// the ones that fit and links to the Jobs page for the rest.
const industryJobCap = 24

// industrySlotNames are the three queues by slot class.
var industrySlotNames = [3]string{"Manufacturing", "Science", "Reactions"}

// industryActivityClass maps the activity option to a slot class.
var industryActivityClass = map[string]int{
	"manufacturing": slotManufacturing,
	"science":       slotScience,
	"reactions":     slotReactions,
}

// industryJob is one job in the widget's list.
type industryJob struct {
	Char     string
	CharID   int64
	Activity string
	Product  string
	TypeID   int64
	Icon     string
	Ready    bool
	EndRaw   string // RFC3339, for the countdown
	Ends     string // the finish, written out
	Left     string // time left; "" once ready
	HasBar   bool
	Pct      float64 // share of the job's run already done, 0 to 100
	end      time.Time
}

// industryMeter is one w-meters row: slots used of available.
type industryMeter struct {
	Name, Text, Tone string
	Pct              float64
}

// industryPilot is one character's slot use in the large form.
type industryPilot struct {
	Name   string
	CharID int64
	Meters []industryMeter
}

// industryHomeView is what the Industry forms draw.
type industryHomeView struct {
	Known   bool // at least one character's job snapshot is loaded
	Ready   int
	Running int
	Lead    wLead
	Next    *industryJob  // the running job that finishes first
	Jobs    []industryJob // ready first, then by finish
	Slots   []industryMeter
	Pilots  []industryPilot
	Note    string // whose slots the meters leave out
	Summary string // small: "6 active jobs · 1 ready for delivery", as live said it
	Empty   wEmpty
}

func buildIndustryWidget(ctx context.Context, app *Application, in *widgetInput) any {
	now := in.Now
	class, oneClass := industryActivityClass[in.Opts.Get("activity")]
	v := &industryHomeView{}

	var jobs []industryJob
	var nameIDs []int64
	var used, have [3]int
	skillsMissing := 0
	for _, b := range in.Bundles {
		if !b.jobsKnown {
			continue
		}
		v.Known = true
		var mine []esi.IndustryJob
		for _, j := range b.jobs {
			if !oneClass || jobSlotClass(j.ActivityID) == class {
				mine = append(mine, j)
			}
		}
		for _, j := range mine {
			state := jobState(j, now)
			if state != "active" && state != "ready" && state != "paused" {
				continue
			}
			nameID := j.ProductTypeID
			if nameID == 0 {
				nameID = j.BlueprintTypeID
			}
			// Only manufacturing and reactions make an item; every other
			// activity's product is a blueprint, drawn with its own art.
			category := int64(contracts.BlueprintCategoryID)
			if j.ActivityID == 1 || j.ActivityID == 11 {
				category = 0
			}
			row := industryJob{
				Char: b.ch.Name, CharID: b.ch.CharacterID,
				Activity: industryActivityLabel(j.ActivityID),
				TypeID:   nameID,
				Icon:     contracts.TypeCategoryIconURL(nameID, category, 64),
				Ready:    state == "ready",
				EndRaw:   j.EndDate,
				Ends:     formatFinish(j.EndDate),
			}
			row.end, _ = parseRFC3339(j.EndDate)
			if row.Ready {
				v.Ready++
			} else {
				v.Running++
				if !row.end.IsZero() {
					row.Left = humanDuration(row.end.Sub(now))
				}
			}
			if start, ok := parseRFC3339(j.StartDate); ok && row.end.After(start) {
				pct := float64(now.Sub(start)) * 100 / float64(row.end.Sub(start))
				row.HasBar, row.Pct = true, min(max(pct, 0), 100)
			}
			nameIDs = append(nameIDs, nameID)
			jobs = append(jobs, row)
		}
		if in.Draws("wide") || in.Draws("medium") || in.Draws("large") {
			if !b.skillsKnown {
				skillsMissing++
				continue
			}
			m, u := slotMaxFor(briefingSkillLevels(b.skills)), slotsUsed(b.jobs, now)
			for c := range m {
				have[c] += m[c]
				used[c] += u[c]
			}
			if in.Draws("large") {
				v.Pilots = append(v.Pilots, industryPilot{Name: b.ch.Name, CharID: b.ch.CharacterID,
					Meters: industryMeters(u, m, class, oneClass)})
			}
		}
	}
	if !v.Known {
		v.Empty = wEmpty{Text: "Jobs appear here once your characters have synced."}
		return v
	}

	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].Ready != jobs[j].Ready {
			return jobs[i].Ready
		}
		return jobs[i].end.Before(jobs[j].end)
	})
	// The next delivery is the first running job of them all, picked
	// before the list is cut: a list of only ready jobs can still have
	// jobs running behind it.
	for i := range jobs {
		if !jobs[i].Ready {
			next := jobs[i]
			v.Next = &next
			break
		}
	}
	if len(jobs) > industryJobCap {
		jobs = jobs[:industryJobCap]
	}
	names := app.esi.CachedTypeNames(ctx, nameIDs)
	productOf := func(typeID int64) string {
		if n := names[typeID]; n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", typeID)
	}
	for i := range jobs {
		jobs[i].Product = productOf(jobs[i].TypeID)
	}
	if v.Next != nil {
		v.Next.Product = productOf(v.Next.TypeID)
	}
	v.Jobs = jobs

	if v.Ready > 0 {
		v.Lead = wLead{Label: "Ready to deliver", Value: esi.FormatInt(int64(v.Ready)), Full: fmt.Sprintf("%d jobs ready to deliver", v.Ready),
			Sub: fmt.Sprintf("%d running", v.Running), Href: "/industry/jobs/?state=ready"}
	} else {
		v.Lead = wLead{Label: "Jobs running", Value: esi.FormatInt(int64(v.Running)), Full: fmt.Sprintf("%d jobs running", v.Running)}
	}
	if len(jobs) == 0 {
		v.Empty = wEmpty{Text: "No jobs running right now.", Step: "Start one from your Blueprints.", Href: "/industry/"}
	}
	if in.Draws("wide") || in.Draws("medium") {
		v.Slots = industryMeters(used, have, class, oneClass)
	}
	if skillsMissing > 0 && (len(v.Slots) > 0 || len(v.Pilots) > 0) {
		v.Note = fmt.Sprintf("Slots leave out %d character%s whose skills have not synced.", skillsMissing, pluralSuffix(skillsMissing))
	}
	v.Summary = pluralCount(int64(v.Running), "active job", "active jobs") + fmt.Sprintf(" · %d ready for delivery", v.Ready)

	return v
}

// industryMeters is one meter per queue, or the one the activity
// option picks: slots used of the slots the skills give.
func industryMeters(used, have [3]int, class int, oneClass bool) []industryMeter {
	var out []industryMeter
	for c := range have {
		if oneClass && c != class {
			continue
		}
		m := industryMeter{Name: industrySlotNames[c], Text: fmt.Sprintf("%d of %d", used[c], have[c])}
		if have[c] > 0 {
			m.Pct = min(float64(used[c])*100/float64(have[c]), 100)
		}
		if used[c] > have[c] {
			m.Tone = "warn"
		}
		out = append(out, m)
	}
	return out
}
