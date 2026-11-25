package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetPI, Title: "Planetary Industry", Category: wcatIndustry, Feature: "planets",
		Href:        "/planets/",
		Description: "Colonies across your characters: extractor timers, expired heads, and the next planet needing a visit.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3", Says: "Extractor timers, soonest first."},
		},
		Needs: []string{esi.SnapPlanets, needPlanetLayouts},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildPI(ctx, in.Bundles)
		},
	})
}
