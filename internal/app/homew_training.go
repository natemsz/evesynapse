package app

import (
	"context"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetTraining, Title: "Training Horizon", Category: wcatPilots, Feature: "skills",
		Href:        "/characters/",
		Description: "The next skill finishes across every character, how long each queue runs, and who is sitting idle.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The next finish and how many pilots are idle."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The next finishes side by side."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "The upcoming finishes with their levels."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The finishes and how long each pilot's queue runs."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds the idle pilots, with a link to plan their skills."},
		},
		Needs: []string{esi.SnapSkillqueue},
		Options: []widgetOption{
			optScope(),
			optChoice("horizon", "Show finishes", "all", "24h:In the next 24 hours", "7d:In the next 7 days", "all:Everything queued"),
		},
		Build: buildTrainingWidget,
	})
}

// trainingHorizons is the horizon option's span per value.
var trainingHorizons = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// trainingWideCap is how many finishes the wide form sets side by
// side; the row shows the ones that fit.
const trainingWideCap = 4

// trainingView is what the Training Horizon forms draw.
type trainingView struct {
	*trainingWidget
	Next      []trainingEvent // wide: the soonest finishes
	Meters    []trainingMeter // medium, large: one per pilot, idle first
	PlansHref string          // large: where to plan an idle pilot's skills
	Pilots    int             // pilots whose queue is known
	Within    string          // the horizon in words, "" for everything queued
	Empty     wEmpty
}

// trainingMeter is one pilot's queue as a w-meters row: how long it
// runs, against the longest queue on the card.
type trainingMeter struct {
	Name, Text, Tone string
	Pct              float64
	left             time.Duration
}

func buildTrainingWidget(ctx context.Context, app *Application, in *widgetInput) any {
	span := trainingHorizons[in.Opts.Get("horizon")]
	v := &trainingView{trainingWidget: app.buildTrainingWithin(ctx, in.Bundles, in.Now, span)}
	switch in.Opts.Get("horizon") {
	case "24h":
		v.Within = "in the next 24 hours"
	case "7d":
		v.Within = "in the next 7 days"
	}
	v.Empty = wEmpty{Text: "Nothing in training yet.", Step: "Open your characters", Href: "/characters/"}
	if v.Within != "" {
		v.Empty = wEmpty{Text: "No skill finishes " + v.Within + "."}
	}
	for _, b := range in.Bundles {
		if b.queueKnown {
			v.Pilots++
		}
	}
	if in.Draws("wide") {
		v.Next = v.Events
		if len(v.Next) > trainingWideCap {
			v.Next = v.Next[:trainingWideCap]
		}
	}
	if in.Draws("medium") || in.Draws("large") {
		v.Meters = trainingMeters(in.Bundles, in.Shared.QueueEnds(), in.Now)
	}
	if in.Draws("large") && moduleCompiledIn("skillplans") {
		v.PlansHref = "/skills/plans"
	}
	return v
}

// trainingMeters is each pilot's queue length, idle pilots first and
// then the queues that run out soonest, each against the longest.
func trainingMeters(bundles []*charSnaps, ends map[int64]time.Time, now time.Time) []trainingMeter {
	var out []trainingMeter
	var longest time.Duration
	for _, b := range bundles {
		if !b.queueKnown {
			continue
		}
		m := trainingMeter{Name: b.ch.Name, Text: "Idle", Tone: "bad"}
		if end, ok := ends[b.ch.CharacterID]; ok && end.After(now) {
			m.left, m.Tone = end.Sub(now), ""
			m.Text = humanDuration(m.left)
			if m.left < 24*time.Hour {
				m.Tone = "warn"
			}
			if m.left > longest {
				longest = m.left
			}
		}
		out = append(out, m)
	}
	for i := range out {
		if longest > 0 {
			out[i].Pct = 100 * float64(out[i].left) / float64(longest)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].left < out[j].left })
	return out
}
