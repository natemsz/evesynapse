package app

import (
	"context"
	"fmt"
	"strings"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetProdLine, Title: "Production Lines", Category: wcatIndustry, Feature: "industry",
		Href:        "/industry/",
		Description: "Your pinned production lines: what is due, what needs a restart, and what to buy for the next run.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many lines are due or need a restart, and the first of them."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The lines that need you, side by side."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Every line with its state, progress and timing."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Every line, and what to buy for the next run."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds who runs each line and what each line is missing."},
		},
		Needs: []string{esi.SnapIndustryJobs, esi.SnapAssets},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			w := app.buildProdLine(ctx, in.UserID, in.Bundles, in.Draws("medium") || in.Draws("large"), in.Draws("large"))
			return prodLineViewOf(w)
		},
	})
}

// prodLineView is what the Production Lines forms draw.
type prodLineView struct {
	*prodLineWidget
	Lead  wLead
	Empty wEmpty
}

func prodLineViewOf(w *prodLineWidget) *prodLineView {
	v := &prodLineView{prodLineWidget: w}
	switch {
	case !w.HasPins:
		v.Empty = wEmpty{Text: "No pinned lines yet.", Step: "Pin a blueprint from your Blueprints.", Href: "/industry/"}
	case !w.JobsKnown:
		v.Empty = wEmpty{Text: "Job data is still syncing."}
	}
	var parts []string
	if w.Due > 0 {
		parts = append(parts, fmt.Sprintf("%d due", w.Due))
	}
	switch {
	case w.Restart == 1:
		parts = append(parts, "1 needs a restart")
	case w.Restart > 1:
		parts = append(parts, fmt.Sprintf("%d need a restart", w.Restart))
	}
	v.Lead = wLead{Label: "Lines needing you", Value: esi.FormatInt(int64(w.Attention)),
		Full: fmt.Sprintf("%d of %d pinned lines need you", w.Attention, len(w.Lines)), Sub: strings.Join(parts, ", ")}
	if w.Attention == 0 {
		v.Lead.Sub = fmt.Sprintf("%d line%s running", len(w.Lines), pluralSuffix(len(w.Lines)))
	}
	return v
}
