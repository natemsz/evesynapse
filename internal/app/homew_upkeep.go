package app

import (
	"context"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetUpkeep, Title: "Upkeep & Fuel", Category: wcatCorp, Feature: "corp_structures",
		Requires:    "corpdata",
		Href:        "/corporations/structures/",
		Description: "Structures low on fuel, unanchoring, or running a state timer. Station Managers and Directors only.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3", Says: "Structure fuel and timer deadlines."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildUpkeep(ctx, in.UserID)
		},
	})
}
