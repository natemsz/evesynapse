package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetUpkeep, Title: "Upkeep & Fuel", Category: wcatCorp, Feature: "corp_structures",
		Requires:    "corpdata",
		Href:        "/corporations/structures/",
		Description: "Structures low on fuel, unanchoring, or running a state timer. Station Managers and Directors only.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many are past due or due within 7 days, and the soonest."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The soonest three, each with the time it has left."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Every structure with a meter of the time it has left."},
			{Name: "medium", Sizes: "2x2 2x3 3x2", Says: "Every structure, grouped by corporation."},
		},
		Build: buildUpkeepWidget,
	})
}

// upkeepMeter is one deadline as a w-meters row: the time left of
// the week the widget watches.
type upkeepMeter struct {
	Name string
	Pct  float64
	Text string
	Tone string // "bad" past due, "warn" inside two days
	at   time.Time
}

// upkeepCorpMeters is one corporation's deadlines as meters.
type upkeepCorpMeters struct {
	Name   string
	Meters []upkeepMeter
}

// upkeepView is what the Upkeep & Fuel forms draw.
type upkeepView struct {
	Lead    wLead
	Urgent  int
	Soonest string // "Astrahus X: fuel expires, in 2d 3h"
	Meters  []upkeepMeter
	Corps   []upkeepCorpMeters // medium: grouped by corporation
	Corp    string             // the one corporation's name, when there is one (several name each line)
	Many    bool               // more than one corporation
	Empty   wEmpty
}

// upkeepWideMeters is how many deadlines the wide form sets side by side.
const upkeepWideMeters = 3

func buildUpkeepWidget(ctx context.Context, app *Application, in *widgetInput) any {
	raw := in.Shared.Upkeep(ctx)
	v := &upkeepView{Urgent: raw.Urgent, Many: len(raw.Corps) > 1}
	if len(raw.Corps) == 0 {
		v.Empty = wEmpty{Text: "All structures are fueled, or none of your characters has the Station Manager or Director role needed to check."}
		return v
	}
	var soonest *upkeepItem
	for _, c := range raw.Corps {
		cm := upkeepCorpMeters{Name: c.Name}
		for i := range c.Items {
			it := &c.Items[i]
			if soonest == nil || it.at.Before(soonest.at) {
				soonest = it
			}
			name := it.Name
			if v.Many && !in.Draws("medium") {
				name += " (" + c.Name + ")"
			}
			m := upkeepMeterFor(*it, name, in.Now)
			v.Meters = append(v.Meters, m)
			cm.Meters = append(cm.Meters, upkeepMeterFor(*it, it.Name, in.Now))
		}
		v.Corps = append(v.Corps, cm)
	}
	if !v.Many {
		v.Corp = raw.Corps[0].Name
	}
	sortUpkeepMeters(v.Meters)
	v.Lead = wLead{Label: "Due within 7 days", Value: strconv.Itoa(raw.Count), Full: fmt.Sprintf("%d deadlines within 7 days", raw.Count)}
	if soonest != nil {
		v.Soonest = soonest.Name + ": " + soonest.Text + ", " + soonest.When
	}
	return v
}

// upkeepMeterFor is one deadline as a meter of the week watched.
func upkeepMeterFor(it upkeepItem, name string, now time.Time) upkeepMeter {
	left := it.at.Sub(now)
	m := upkeepMeter{Name: name, Pct: 100 * left.Hours() / (upkeepFuelDays * 24), at: it.at}
	switch {
	case it.Urgent || left <= 0:
		m.Pct, m.Tone, m.Text = 0, "bad", it.Text+", past due"
	case left < 48*time.Hour:
		m.Tone, m.Text = "warn", it.Text+", "+humanDuration(left)+" left"
	default:
		m.Text = it.Text + ", " + humanDuration(left) + " left"
	}
	if m.Pct > 100 {
		m.Pct = 100
	}
	return m
}

// sortUpkeepMeters puts the soonest deadline first.
func sortUpkeepMeters(ms []upkeepMeter) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].at.Before(ms[j].at) })
}

// Soon is the wide form's deadlines: the soonest three.
func (v *upkeepView) Soon() []upkeepMeter {
	if len(v.Meters) > upkeepWideMeters {
		return v.Meters[:upkeepWideMeters]
	}
	return v.Meters
}
