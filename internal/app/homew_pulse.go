package app

import (
	"context"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetPulse, Title: "Corp Pulse", Category: wcatCorp, Feature: "corp_members",
		Requires:    "corpdata",
		Href:        "/corporations/",
		Description: "What changed in the corporation: members in and out, wallet movements, structure arrivals and timers. Needs a qualifying in-game role.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 2x1 2x2 3x1 3x2", Says: "This week's corporation changes."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildPulse(ctx, in.UserID)
		},
	})
}
