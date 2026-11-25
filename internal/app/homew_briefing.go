package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetBriefing, Title: "Briefing", Category: wcatOverview, Feature: "briefing",
		Description: "What changed since you last looked, and what needs you in the next day.",
		Default:     wSize{3, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "2x1 3x1", Says: "What changed and what needs you, as cards."},
		},
		Needs: []string{esi.SnapSkillqueue, esi.SnapSkills, esi.SnapIndustryJobs, esi.SnapContracts, esi.SnapOrders, esi.SnapOrdersHistory, esi.SnapPlanets, esi.SnapMail, esi.SnapMailLabels, esi.SnapCalendar, needPlanetLayouts},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildBriefing(ctx, in.Bundles, in.Shared.BriefingSince(ctx), in.Now, in.Shared.market)
		},
	})
}
