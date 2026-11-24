package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetProdLine, Title: "Production Lines", Category: wcatIndustry, Feature: "industry",
		Href:        "/industry/",
		Description: "Your pinned production lines: what is due, what needs a restart, and what to buy for the next run.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3 3x1 3x2 3x3", Says: "Your pinned lines and what to buy for the next run."},
		},
		Needs: []string{esi.SnapIndustryJobs, esi.SnapAssets},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildProdLine(ctx, in.UserID, in.Bundles, in.Size.W, in.Size.H)
		},
	})
}
