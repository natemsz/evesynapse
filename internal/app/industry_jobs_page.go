package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// The My Jobs page: every industry job across the characters in
// scope, read from each character's stored job snapshot, with the
// facilities they run in, each pilot's slot use against the slots
// their skills give them, and one job open in the inspector.

// Slot classes: the three job queues EVE counts separately.
const (
	slotManufacturing = iota
	slotScience
	slotReactions
)

// jobSlotClass is the queue a job's activity uses.
func jobSlotClass(activityID int64) int {
	switch activityID {
	case 1:
		return slotManufacturing
	case 9, 11:
		return slotReactions
	default:
		return slotScience
	}
}

// The skills that add slots: base one of each, plus a slot per level
// of the skill and of its advanced version.
var slotSkills = [3][2]int64{
	slotManufacturing: {3387, 24625},  // Mass Production, Advanced Mass Production
	slotScience:       {3406, 24624},  // Laboratory Operation, Advanced Laboratory Operation
	slotReactions:     {45748, 45749}, // Mass Reactions, Advanced Mass Reactions
}

// jobActivityTabs are the activity filters, in EVE's own order.
var jobActivityTabs = []struct {
	key, label string
	ids        []int64
}{
	{"manufacturing", "Manufacturing", []int64{1}},
	{"me", "ME research", []int64{4}},
	{"te", "TE research", []int64{3}},
	{"copying", "Copying", []int64{5}},
	{"invention", "Invention", []int64{8}},
	{"reverse", "Reverse engineering", []int64{7}},
	{"reactions", "Reactions", []int64{9, 11}},
}

// jobStateTabs are the state filters.
var jobStateTabs = []struct{ key, label string }{
	{"active", "Running"},
	{"ready", "Ready to deliver"},
	{"paused", "Paused"},
	{"delivered", "Delivered"},
	{"ended", "Cancelled or reverted"},
}

// jobState folds ESI's status and the clock into one of the state
// filter keys: an active job whose end has passed is ready.
func jobState(j esi.IndustryJob, now time.Time) string {
	switch j.Status {
	case "active":
		if t, err := time.Parse(time.RFC3339, j.EndDate); err == nil && !t.After(now) {
			return "ready"
		}
		return "active"
	case "ready", "paused", "delivered":
		return j.Status
	default:
		return "ended"
	}
}

// jobOccupiesSlot: a job holds its slot until it is delivered.
func jobOccupiesSlot(state string) bool {
	return state == "active" || state == "ready" || state == "paused"
}

type jobFilter struct {
	Label string
	Count int
	Href  string
	On    bool
}

type jobTile struct {
	Label string
	Value string
	Sub   string
	Href  string // the tile filters the list, when set
	On    bool
	Tone  string // "ok" | "warn" | ""
	Until string // RFC3339: the value counts down to it
}

type jobFacility struct {
	ID       int64
	Title    string
	Place    placeRef
	Jobs     int
	Running  int
	Ready    int
	CostText string
	Href     string
	On       bool
}

type jobSlotUse struct {
	Label     string
	Used, Max int
	MaxKnown  bool
	Full      bool
	Pct       string // the meter's fill, 0 to 100
}

type jobPilot struct {
	Name  string
	ID    int64
	Slots []jobSlotUse
}

type jobListRow struct {
	industryJobRow
	JobID     int64
	State     string
	StateText string
	Href      string
	On        bool
}

type jobInspector struct {
	Row         jobListRow
	Start       string
	End         string
	EndRaw      string // RFC3339, for the countdown
	Duration    string
	ProgressPct string // 0 to 100, one decimal: the bar's width in a 100-unit SVG
	Progress    string // "62%"
	Licensed    string
	Successful  string
	Probability string
	BlueprintAt placeRef
	OutputAt    placeRef
	CloseHref   string
}

type jobsView struct {
	Selector   assetScopeSelector
	ScopeCount int
	Warming    []string // characters whose job snapshot has not landed
	Notes      []string // "Name: why the jobs are missing" (a missing scope, a refused fetch)
	Tiles      []jobTile
	Facilities []jobFacility
	AllHref    string
	AllOn      bool
	DirHref    string // back to the facility directory (a phone's top level)
	Pilots     []jobPilot
	Activities []jobFilter
	States     []jobFilter
	Rows       []jobListRow
	Cut        int
	ListTitle  string
	Depth      int
	Insp       *jobInspector
	Any        bool // any jobs at all in scope, before the filters
}

type jobsQuery struct {
	scope    charScope
	activity string
	state    string
	facility int64
	job      int64
}

func (q jobsQuery) href(mut map[string]string) string {
	v := url.Values{}
	// Links inside the list stay at the list level (a phone's second
	// screen); only DirHref, which clears this, returns to the
	// facility directory.
	v.Set("list", "1")
	v.Set("scope", q.scope.param())
	v.Set("activity", q.activity)
	v.Set("state", q.state)
	if q.facility != 0 {
		v.Set("facility", strconv.FormatInt(q.facility, 10))
	}
	if q.job != 0 {
		v.Set("job", strconv.FormatInt(q.job, 10))
	}
	for k, val := range mut {
		v.Set(k, val)
	}
	return withQuery("/industry/jobs/", v)
}

type scopedJob struct {
	esi.IndustryJob
	owner db.Character
	state string
}

// buildJobsView reads the jobs and skills of every character in
// scope (one stored snapshot each) and shapes the page.
func (app *Application) buildJobsView(ctx context.Context, r *http.Request, characters []db.Character) *jobsView {
	q := r.URL.Query()
	jq := jobsQuery{scope: parseCharScope(q.Get("scope"), characters)}
	jq.activity = q.Get("activity")
	jq.state = q.Get("state")
	jq.facility, _ = strconv.ParseInt(q.Get("facility"), 10, 64)
	jq.job, _ = strconv.ParseInt(q.Get("job"), 10, 64)

	view := &jobsView{}
	view.Selector = buildScopeSelector(characters, jq.scope, func(scope string) string {
		return jobsQuery{scope: parseCharScope(scope, characters), activity: jq.activity, state: jq.state}.href(nil)
	})

	now := time.Now()
	var all []scopedJob
	var slotMax = map[int64][3]int{}
	var slotKnown = map[int64]bool{}
	var scoped []db.Character
	for _, ch := range characters {
		if !jq.scope.has(ch.CharacterID) {
			continue
		}
		scoped = append(scoped, ch)
		var jobs esi.IndustryJobs
		if st := app.econSection(ctx, ch.CharacterID, esi.SnapIndustryJobs, &jobs); !st.Loaded {
			if st.Note != "" {
				view.Notes = append(view.Notes, ch.Name+": "+st.Note)
			} else {
				view.Warming = append(view.Warming, ch.Name)
			}
		}
		for _, j := range jobs {
			all = append(all, scopedJob{IndustryJob: j, owner: ch, state: jobState(j, now)})
		}
		var skills esi.Skills
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkills, &skills) {
			levels := map[int64]int{}
			for _, s := range skills.Skills {
				levels[s.SkillID] = s.ActiveSkillLevel
			}
			var m [3]int
			for c, ids := range slotSkills {
				m[c] = 1 + levels[ids[0]] + levels[ids[1]]
			}
			slotMax[ch.CharacterID] = m
			slotKnown[ch.CharacterID] = true
		}
	}
	view.ScopeCount = len(scoped)
	view.Any = len(all) > 0

	// Tiles: what is running against the slots, what is ready, what
	// installs cost over 30 days, and the next job to finish.
	running, ready, slots, slotsKnown := 0, 0, 0, true
	var cost30 float64
	var next *scopedJob
	for i := range all {
		j := &all[i]
		switch j.state {
		case "active":
			running++
			if t, err := time.Parse(time.RFC3339, j.EndDate); err == nil {
				if next == nil || t.Before(jobTime(next.EndDate)) {
					next = j
				}
			}
		case "ready":
			ready++
		}
		if t, err := time.Parse(time.RFC3339, j.StartDate); err == nil && now.Sub(t) <= 30*24*time.Hour {
			cost30 += j.Cost
		}
	}
	for _, ch := range scoped {
		if !slotKnown[ch.CharacterID] {
			slotsKnown = false
			continue
		}
		m := slotMax[ch.CharacterID]
		slots += m[0] + m[1] + m[2]
	}
	runSub := "no slot data yet"
	if slots > 0 {
		runSub = fmt.Sprintf("of %d slots", slots)
		if !slotsKnown {
			runSub += " known so far"
		}
	}
	view.Tiles = []jobTile{
		{Label: "Running", Value: strconv.Itoa(running), Sub: runSub, Href: jq.href(map[string]string{"state": "active", "job": ""}), On: jq.state == "active"},
		{Label: "Ready to deliver", Value: strconv.Itoa(ready), Sub: "finished, waiting for delivery", Href: jq.href(map[string]string{"state": "ready", "job": ""}), On: jq.state == "ready", Tone: map[bool]string{true: "warn", false: ""}[ready > 0]},
		{Label: "Install cost, 30 days", Value: iskCompact(cost30) + " ISK", Sub: "jobs started in the last 30 days"},
	}
	if next != nil {
		name := app.typeNameOrID(ctx, next.ProductTypeID)
		if next.ProductTypeID == 0 {
			name = app.typeNameOrID(ctx, next.BlueprintTypeID)
		}
		view.Tiles = append(view.Tiles, jobTile{Label: "Next to finish", Value: name, Sub: next.owner.Name + ", " + formatFinish(next.EndDate), Until: next.EndDate,
			Href: jq.href(map[string]string{"job": strconv.FormatInt(next.JobID, 10)})})
	}

	// Facilities: the directory, busiest first.
	byFac := map[int64]*jobFacility{}
	var facOrder []int64
	for _, j := range all {
		id := j.FacilityID
		if id == 0 {
			id = j.StationID
		}
		f := byFac[id]
		if f == nil {
			title := app.econLocationTitle(ctx, id)
			f = &jobFacility{ID: id, Title: title, Place: app.linkPlace(ctx, id, title)}
			byFac[id] = f
			facOrder = append(facOrder, id)
		}
		f.Jobs++
		switch j.state {
		case "active":
			f.Running++
		case "ready":
			f.Ready++
		}
	}
	facCost := map[int64]float64{}
	for _, j := range all {
		id := j.FacilityID
		if id == 0 {
			id = j.StationID
		}
		facCost[id] += j.Cost
	}
	sort.SliceStable(facOrder, func(a, b int) bool {
		fa, fb := byFac[facOrder[a]], byFac[facOrder[b]]
		if fa.Running+fa.Ready != fb.Running+fb.Ready {
			return fa.Running+fa.Ready > fb.Running+fb.Ready
		}
		return fa.Jobs > fb.Jobs
	})
	for _, id := range facOrder {
		f := byFac[id]
		f.CostText = iskCompact(facCost[id])
		f.On = id != 0 && jq.facility == id
		f.Href = jq.href(map[string]string{"facility": strconv.FormatInt(id, 10), "job": ""})
		view.Facilities = append(view.Facilities, *f)
	}
	view.AllOn = jq.facility == 0
	view.AllHref = jq.href(map[string]string{"facility": "", "job": ""})
	view.DirHref = jq.href(map[string]string{"list": "", "facility": "", "activity": "", "state": "", "job": ""})

	// Slot use per pilot.
	used := map[int64]*[3]int{}
	for _, j := range all {
		if !jobOccupiesSlot(j.state) {
			continue
		}
		u := used[j.owner.CharacterID]
		if u == nil {
			u = &[3]int{}
			used[j.owner.CharacterID] = u
		}
		u[jobSlotClass(j.ActivityID)]++
	}
	labels := [3]string{"Manufacturing", "Science", "Reactions"}
	for _, ch := range scoped {
		p := jobPilot{Name: ch.Name, ID: ch.CharacterID}
		u := used[ch.CharacterID]
		for c := 0; c < 3; c++ {
			s := jobSlotUse{Label: labels[c], MaxKnown: slotKnown[ch.CharacterID]}
			if u != nil {
				s.Used = u[c]
			}
			if s.MaxKnown {
				s.Max = slotMax[ch.CharacterID][c]
				s.Full = s.Used >= s.Max
				pct := 0
				if s.Max > 0 {
					pct = s.Used * 100 / s.Max
				}
				if pct > 100 {
					pct = 100
				}
				s.Pct = strconv.Itoa(pct)
			}
			p.Slots = append(p.Slots, s)
		}
		view.Pilots = append(view.Pilots, p)
	}

	// Filters, counted over the facility's jobs (or all of them).
	inFacility := func(j scopedJob) bool {
		if jq.facility == 0 {
			return true
		}
		id := j.FacilityID
		if id == 0 {
			id = j.StationID
		}
		return id == jq.facility
	}
	activityOf := func(id int64) string {
		for _, t := range jobActivityTabs {
			for _, a := range t.ids {
				if a == id {
					return t.key
				}
			}
		}
		return ""
	}
	actCount, stateCount := map[string]int{}, map[string]int{}
	facTotal := 0
	for _, j := range all {
		if !inFacility(j) {
			continue
		}
		facTotal++
		actCount[activityOf(j.ActivityID)]++
		stateCount[j.state]++
	}
	view.Activities = append(view.Activities, jobFilter{Label: "All activities", Count: facTotal, Href: jq.href(map[string]string{"activity": "", "job": ""}), On: jq.activity == ""})
	for _, t := range jobActivityTabs {
		if actCount[t.key] == 0 && jq.activity != t.key {
			continue
		}
		view.Activities = append(view.Activities, jobFilter{Label: t.label, Count: actCount[t.key], Href: jq.href(map[string]string{"activity": t.key, "job": ""}), On: jq.activity == t.key})
	}
	view.States = append(view.States, jobFilter{Label: "All states", Count: facTotal, Href: jq.href(map[string]string{"state": "", "job": ""}), On: jq.state == ""})
	for _, t := range jobStateTabs {
		if stateCount[t.key] == 0 && jq.state != t.key {
			continue
		}
		view.States = append(view.States, jobFilter{Label: t.label, Count: stateCount[t.key], Href: jq.href(map[string]string{"state": t.key, "job": ""}), On: jq.state == t.key})
	}

	// The list: filtered, running and ready first by end, then the
	// rest most recently ended first.
	var picked []scopedJob
	for _, j := range all {
		if !inFacility(j) || (jq.activity != "" && activityOf(j.ActivityID) != jq.activity) || (jq.state != "" && j.state != jq.state) {
			continue
		}
		picked = append(picked, j)
	}
	rank := map[string]int{"ready": 0, "active": 1, "paused": 2, "delivered": 3, "ended": 4}
	sort.SliceStable(picked, func(a, b int) bool {
		ra, rb := rank[picked[a].state], rank[picked[b].state]
		if ra != rb {
			return ra < rb
		}
		ea, eb := jobTime(picked[a].EndDate), jobTime(picked[b].EndDate)
		if ra <= 2 {
			return ea.Before(eb)
		}
		return ea.After(eb)
	})
	if len(picked) > maxIndustryRows {
		view.Cut = len(picked) - maxIndustryRows
		picked = picked[:maxIndustryRows]
	}
	stateText := map[string]string{}
	for _, t := range jobStateTabs {
		stateText[t.key] = t.label
	}
	stateText["active"] = "Running"
	stateText["ready"] = "Ready"
	for _, j := range picked {
		row := jobListRow{industryJobRow: app.jobRow(ctx, j.IndustryJob), JobID: j.JobID, State: j.state, StateText: stateText[j.state]}
		row.Href = jq.href(map[string]string{"job": strconv.FormatInt(j.JobID, 10)})
		row.On = j.JobID == jq.job
		view.Rows = append(view.Rows, row)
	}

	// The list's title names the facility and filters in play.
	view.ListTitle = "Jobs"
	if f := byFac[jq.facility]; f != nil && jq.facility != 0 {
		view.ListTitle = "Jobs at " + f.Title
	}

	// The inspector.
	view.Depth = 1
	if jq.facility == 0 && q.Get("list") != "1" && jq.activity == "" && jq.state == "" {
		view.Depth = 0
	}
	if jq.job != 0 {
		for _, j := range all {
			if j.JobID != jq.job {
				continue
			}
			view.Insp = app.jobInspectorFor(ctx, j, stateText[j.state], jq.href(map[string]string{"job": ""}), now)
			view.Depth = 2
			break
		}
	}
	return view
}

func jobTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// jobRow shapes one job's list line.
func (app *Application) jobRow(ctx context.Context, j esi.IndustryJob) industryJobRow {
	row := industryJobRow{
		Activity:        industryActivityLabel(j.ActivityID),
		Blueprint:       app.typeNameOrID(ctx, j.BlueprintTypeID),
		BlueprintTypeID: j.BlueprintTypeID,
		ProductTypeID:   j.ProductTypeID,
		Installer:       app.displayCharacter(ctx, j.InstallerID),
		InstallerID:     j.InstallerID,
		Status:          humanizeEnum(j.Status),
		Cost:            esi.FormatISK(j.Cost),
	}
	if j.ProductTypeID > 0 {
		row.Product = app.typeNameOrID(ctx, j.ProductTypeID)
	}
	facilityID := j.FacilityID
	if facilityID == 0 {
		facilityID = j.StationID
	}
	if facilityID > 0 {
		row.Facility = app.linkPlace(ctx, facilityID, app.econLocationTitle(ctx, facilityID))
	}
	if j.SuccessfulRuns > 0 || j.Status == "delivered" {
		row.Runs = fmt.Sprintf("%d / %s", j.SuccessfulRuns, esi.FormatInt(j.Runs))
	} else {
		row.Runs = esi.FormatInt(j.Runs)
	}
	row.Ends = formatFinish(j.EndDate)
	return row
}

func (app *Application) jobInspectorFor(ctx context.Context, j scopedJob, stateText, closeHref string, now time.Time) *jobInspector {
	in := &jobInspector{
		Row:       jobListRow{industryJobRow: app.jobRow(ctx, j.IndustryJob), JobID: j.JobID, State: j.state, StateText: stateText},
		Start:     formatFinish(j.StartDate),
		End:       formatFinish(j.EndDate),
		EndRaw:    j.EndDate,
		Duration:  humanDuration(time.Duration(j.Duration) * time.Second),
		CloseHref: closeHref,
	}
	start, end := jobTime(j.StartDate), jobTime(j.EndDate)
	if span := end.Sub(start); span > 0 {
		done := now.Sub(start)
		if j.state != "active" {
			done = span
		}
		pct := float64(done) / float64(span) * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		in.ProgressPct = strconv.FormatFloat(pct, 'f', 1, 64)
		in.Progress = strconv.Itoa(int(pct)) + "%"
	}
	if j.LicensedRuns > 0 && j.LicensedRuns != j.Runs {
		in.Licensed = esi.FormatInt(j.LicensedRuns)
	}
	if j.SuccessfulRuns > 0 {
		in.Successful = esi.FormatInt(j.SuccessfulRuns)
	}
	if j.Probability > 0 && j.ActivityID == 8 {
		in.Probability = strconv.Itoa(int(j.Probability*100+0.5)) + "%"
	}
	if j.BlueprintLocationID > 0 {
		in.BlueprintAt = app.linkPlace(ctx, j.BlueprintLocationID, app.econLocationTitle(ctx, j.BlueprintLocationID))
	}
	if j.OutputLocationID > 0 {
		in.OutputAt = app.linkPlace(ctx, j.OutputLocationID, app.econLocationTitle(ctx, j.OutputLocationID))
	}
	return in
}
