package app

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetAgenda, Title: "Coming Up", Category: wcatOverview, Feature: "briefing",
		Href:        "/calendar/",
		Description: "Everything with a time on it, in time order: fleets, calendar events, skills, jobs, extractors, couriers and structures.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The next two things coming up."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The next thing beside a timeline of the rest."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Everything coming up, under Today, Tomorrow and Later."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The timeline over the list."},
			{Name: "large", Sizes: "3x2 3x3", Says: "A column for each day of the window."},
		},
		Needs: []string{esi.SnapSkillqueue, esi.SnapIndustryJobs, esi.SnapPlanets, needPlanetLayouts, esi.SnapContracts, esi.SnapCalendar},
		Options: []widgetOption{
			optScope(),
			optChoice("window", "Look ahead", "3d", "24h:24 hours", "3d:3 days", "7d:7 days"),
			withHint(optMulti("kinds", "Show", strings.Join(agendaKindIDs(), ","), 0, agendaKindChoices()...),
				"Kinds you leave unticked stay off the list."),
		},
		Build: buildAgendaWidget,
	})
}

// agendaKinds are what Coming Up lists, in its settings' order. Group
// picks the timeline colour: four groups, one categorical colour each.
var agendaKinds = []struct{ ID, Label, Group string }{
	{"fleet", "Fleets", "event"},
	{"calendar", "Calendar events", "event"},
	{"skill", "Skill training", "skill"},
	{"industry", "Industry jobs", "make"},
	{"pi", "Extractors", "make"},
	{"courier", "Courier deadlines", "due"},
	{"structure", "Structure fuel and timers", "due"},
}

// agendaGroups are the timeline's colours and their legend words.
var agendaGroups = []struct{ ID, Label, Slot string }{
	{"event", "Fleets and events", "1"},
	{"skill", "Skills", "2"},
	{"make", "Jobs and extractors", "3"},
	{"due", "Deadlines", "4"},
}

func agendaKindIDs() []string {
	out := make([]string, len(agendaKinds))
	for i, k := range agendaKinds {
		out[i] = k.ID
	}
	return out
}

func agendaKindChoices() []string {
	out := make([]string, len(agendaKinds))
	for i, k := range agendaKinds {
		out[i] = k.ID + ":" + k.Label
	}
	return out
}

func agendaGroupOf(kind string) string {
	for _, k := range agendaKinds {
		if k.ID == kind {
			return k.Group
		}
	}
	return "event"
}

// agendaWindows are the look-ahead choices.
var agendaWindows = map[string]time.Duration{"24h": 24 * time.Hour, "3d": 72 * time.Hour, "7d": 7 * 24 * time.Hour}

// agendaMax is the most items the widget lists.
const agendaMax = 40

// agendaItem is one thing with a time on it.
type agendaItem struct {
	Kind   string
	Group  string
	Slot   string // the group's colour
	What   template.HTML
	Who    template.HTML // the pilot or owner it belongs to; empty when the whole account
	Href   string
	AtRaw  string
	When   string // the moment in words, for the title
	Left   string // "in 3h 20m"
	Live   bool   // a fleet already running
	at     time.Time
	charID int64
}

// agendaDay is one day of the window, for the large form's columns and
// the tall form's headings.
type agendaDay struct {
	Label string
	Items []agendaItem
}

// agendaMark is one item on the timeline.
type agendaMark struct {
	Left, Top float64
	Slot      string
	Label     string
}

// agendaView is what the Coming Up forms draw.
type agendaView struct {
	Items  []agendaItem
	Groups []agendaDay // Today, Tomorrow, Later
	Days   []agendaDay // one per day of the window
	First  *agendaItem
	Marks  []agendaMark
	Ticks  []wTick
	Legend []struct{ Slot, Label string }
	Table  wTable
	Window string // "3 days"
	Empty  wEmpty
}

func buildAgendaWidget(ctx context.Context, app *Application, in *widgetInput) any {
	span, ok := agendaWindows[in.Opts.Get("window")]
	if !ok {
		span = agendaWindows["3d"]
	}
	now := in.Now.UTC()
	end := now.Add(span)
	v := &agendaView{Window: map[time.Duration]string{24 * time.Hour: "24 hours", 72 * time.Hour: "3 days", 7 * 24 * time.Hour: "7 days"}[span]}
	v.Empty = wEmpty{Text: "Nothing coming up in the next " + v.Window + "."}
	wants := map[string]bool{}
	for _, k := range strings.Split(in.Opts.Get("kinds"), ",") {
		wants[k] = true
	}
	viewer := viewerChars(in.Chars)
	var items []agendaItem
	add := func(kind string, at time.Time, what, who template.HTML, href string, charID int64) {
		if !wants[kind] || at.After(end) {
			return
		}
		raw := at.UTC().Format(time.RFC3339)
		it := agendaItem{Kind: kind, Group: agendaGroupOf(kind), What: what, Who: who, Href: href,
			AtRaw: raw, When: formatFinish(raw), at: at, charID: charID}
		if at.After(now) {
			it.Left = "in " + humanDuration(at.Sub(now))
		}
		items = append(items, it)
	}

	if wants["fleet"] {
		for _, f := range in.Shared.Fleets(ctx) {
			if !f.Access.View {
				continue
			}
			what := template.HTML(`<a href="` + template.HTMLEscapeString(opURL(f.Op.ID)) + `">` + template.HTMLEscapeString(f.Op.Title) + `</a>`)
			at := f.Op.StartsAt
			if f.Live {
				at = now
			}
			add("fleet", at, what, template.HTML(template.HTMLEscapeString(f.Owner)), opURL(f.Op.ID), 0)
			if f.Live {
				items[len(items)-1].Live, items[len(items)-1].Left = true, "running now"
			}
		}
	}
	// An event several pilots are invited to is one item naming them all.
	eventAt := map[int64]int{}
	for _, b := range in.Bundles {
		who := briefCharLink(viewer, b.ch.CharacterID, b.ch.Name)
		id := b.ch.CharacterID
		if wants["calendar"] && b.calendarKnown && moduleCompiledIn("calendar") {
			for _, ev := range b.calendar {
				at, ok := parseRFC3339(ev.EventDate)
				if !ok || at.Before(now) {
					continue
				}
				if i, seen := eventAt[ev.EventID]; seen {
					items[i].Who += ", " + who
					continue
				}
				title := ev.Title
				if title == "" {
					title = "Calendar event"
				}
				href := fmt.Sprintf("/calendar/?character=%d&view=1", id)
				n := len(items)
				add("calendar", at, template.HTML(template.HTMLEscapeString(title)), who, href, id)
				if len(items) > n {
					eventAt[ev.EventID] = n
				}
			}
		}
		if wants["skill"] && b.queueKnown {
			if head := queueHead(b.queue, now); head != nil {
				if at, ok := parseRFC3339(head.FinishDate); ok && at.After(now) {
					what := itemLink(head.SkillID, app.typeNameOrID(ctx, head.SkillID)) + template.HTML(" "+romanNumeral(int64(head.FinishedLevel))+" finishes")
					add("skill", at, what, who, "/characters/", id)
				}
			}
			if last, ok := in.Shared.QueueEnds()[id]; ok && last.After(now) {
				add("skill", last, template.HTML("Skill queue runs out"), who, fmt.Sprintf("/character/%d/", id), id)
			}
		}
		if wants["industry"] && b.jobsKnown {
			for _, j := range b.jobs {
				if jobState(j, now) != "active" {
					continue
				}
				at, ok := parseRFC3339(j.EndDate)
				if !ok {
					continue
				}
				nameID := j.ProductTypeID
				if nameID == 0 {
					nameID = j.BlueprintTypeID
				}
				what := itemLink(nameID, app.typeNameOrID(ctx, nameID)) + template.HTML(" "+strings.ToLower(industryActivityLabel(j.ActivityID))+" done")
				add("industry", at, what, who, "/industry/jobs/", id)
			}
		}
		if wants["courier"] && b.contractsKnown {
			for _, c := range b.contracts {
				if c.Status != "in_progress" || c.Type != "courier" || c.AcceptorID != id {
					continue
				}
				deadline, ok := courierDeadline(c)
				if !ok || deadline.Before(now) {
					continue
				}
				title := c.Title
				if title == "" {
					title = "Courier contract"
				}
				add("courier", deadline, template.HTML(template.HTMLEscapeString(title)+" due"), who, "/contracts/", id)
			}
		}
	}
	if wants["pi"] {
		for _, r := range forChars(in.Shared.PIExpiries(ctx), in.Chars, func(r piExpiryLine) int64 { return r.CharID }) {
			if r.Expired {
				continue
			}
			what := template.HTML(template.HTMLEscapeString(r.Planet) + " extractor stops")
			if r.Count > 1 {
				what = template.HTML(fmt.Sprintf("%s: %d extractors stop", template.HTMLEscapeString(r.Planet), r.Count))
			}
			add("pi", r.at, what, briefCharLink(viewer, r.CharID, r.Char), "/planets/", r.CharID)
		}
	}
	if wants["structure"] {
		if up := in.Shared.Upkeep(ctx); up != nil {
			for _, c := range up.Corps {
				for _, it := range c.Items {
					if it.Urgent || it.at.Before(now) {
						continue
					}
					add("structure", it.at, template.HTML(template.HTMLEscapeString(it.Name+": "+it.Text)),
						template.HTML(template.HTMLEscapeString(c.Name)), "/corporations/structures/", 0)
				}
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	if len(items) > agendaMax {
		items = items[:agendaMax]
	}
	slots := map[string]string{}
	for _, g := range agendaGroups {
		slots[g.ID] = g.Slot
	}
	for i := range items {
		items[i].Slot = slots[items[i].Group]
	}
	v.Items = items
	if len(items) == 0 {
		return v
	}
	v.First = &v.Items[0]
	if in.Draws("tall") {
		v.Groups = agendaByWhen(items, now)
	}
	if in.Draws("large") {
		v.Days = agendaByDay(items, now, span)
	}
	if in.Draws("wide") || in.Draws("medium") {
		agendaTrack(v, now, span)
	}
	return v
}

// agendaByWhen groups items under Today, Tomorrow and Later (EVE time).
func agendaByWhen(items []agendaItem, now time.Time) []agendaDay {
	today := now.Truncate(24 * time.Hour)
	groups := []agendaDay{{Label: "Today"}, {Label: "Tomorrow"}, {Label: "Later"}}
	for _, it := range items {
		switch d := it.at.UTC().Truncate(24 * time.Hour); {
		case !d.After(today):
			groups[0].Items = append(groups[0].Items, it)
		case d.Equal(today.Add(24 * time.Hour)):
			groups[1].Items = append(groups[1].Items, it)
		default:
			groups[2].Items = append(groups[2].Items, it)
		}
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.Items) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// agendaByDay is a column per UTC day the window touches, from today
// to the day it ends, empty days included: the window runs from now,
// so a 24-hour one reaches into tomorrow and every item it holds has
// a column.
func agendaByDay(items []agendaItem, now time.Time, span time.Duration) []agendaDay {
	today := now.Truncate(24 * time.Hour)
	n := int(now.Add(span).Truncate(24*time.Hour).Sub(today)/(24*time.Hour)) + 1
	days := make([]agendaDay, n)
	for i := range days {
		d := today.AddDate(0, 0, i)
		switch i {
		case 0:
			days[i].Label = "Today"
		case 1:
			days[i].Label = "Tomorrow"
		default:
			days[i].Label = d.Format("Mon 2 Jan")
		}
	}
	for _, it := range items {
		i := int(it.at.UTC().Truncate(24*time.Hour).Sub(today) / (24 * time.Hour))
		if i < 0 {
			i = 0
		}
		if i < n {
			days[i].Items = append(days[i].Items, it)
		}
	}
	return days
}

// agendaTrack places each item on a line from now to the window's end,
// stacking marks that would overlap, with a legend for the groups it
// shows and a table twin.
func agendaTrack(v *agendaView, now time.Time, span time.Duration) {
	shown := map[string]bool{}
	lastAt := map[int]float64{} // per stack level, the last mark's left
	for _, it := range v.Items {
		left := float64(it.at.Sub(now)) / float64(span) * 100
		left = min(max(left, 0), 100)
		level := 0
		for ; level < 3; level++ {
			if prev, ok := lastAt[level]; !ok || left-prev >= 2.5 {
				break
			}
		}
		lastAt[level] = left
		label := it.When + ": " + agendaPlain(it.What)
		v.Marks = append(v.Marks, agendaMark{Left: left, Top: 25 + float64(level)*25, Slot: it.Slot, Label: label})
		shown[it.Group] = true
		v.Table.Rows = append(v.Table.Rows, []string{it.When, agendaPlain(it.What)})
	}
	for _, g := range agendaGroups {
		if shown[g.ID] {
			v.Legend = append(v.Legend, struct{ Slot, Label string }{g.Slot, g.Label})
		}
	}
	v.Table.Caption = "Coming up in the next " + v.Window
	v.Table.Head = []string{"When", "What"}
	v.Ticks = []wTick{{Label: "Now", Pos: 0, Anchor: "start"}, {Label: agendaSpanLabel(span / 2), Pos: 50, Anchor: "middle"}, {Label: agendaSpanLabel(span), Pos: 100, Anchor: "end"}}
}

// agendaSpanLabel is a tick's distance from now: "12 hours", "3 days".
func agendaSpanLabel(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	days := d.Hours() / 24
	if days == float64(int(days)) {
		return fmt.Sprintf("%d days", int(days))
	}
	return fmt.Sprintf("%.1f days", days)
}

// agendaPlain is an item's words without their links, for titles and
// the table twin.
func agendaPlain(h template.HTML) string {
	return html.UnescapeString(agendaTags.ReplaceAllString(string(h), ""))
}

var agendaTags = regexp.MustCompile(`<[^>]*>`)
