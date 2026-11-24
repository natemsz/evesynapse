package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetTraining, Title: "Training Horizon", Category: wcatPilots, Feature: "skills",
		Href:        "/characters/",
		Description: "The next 10 skill finishes across every character, plus who is sitting idle.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3", Says: "The next skill finishes and who is idle."},
		},
		Needs: []string{esi.SnapSkillqueue},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildTraining(ctx, in.Bundles)
		},
	})
}
