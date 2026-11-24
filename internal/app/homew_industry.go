package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetIndustry, Title: "Industry", Category: wcatIndustry, Feature: "industry",
		Href:        "/industry/jobs/",
		Description: "Active industry jobs across characters, soonest delivery first.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3", Says: "Active jobs, soonest delivery first."},
		},
		Needs: []string{esi.SnapIndustryJobs},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildIndustry(ctx, in.Bundles)
		},
	})
}
