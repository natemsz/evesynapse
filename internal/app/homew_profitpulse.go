package app

import (
	"context"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetProfitPulse, Title: "Profit pulse", Category: wcatMoney, Feature: "market",
		Href:        "/profit/",
		Description: "The Profit Analysis headline for the last 30 days: what came in, what you kept, and the item that earned the most.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3 3x1 3x2 3x3", Says: "Gross in and kept profit over 30 days."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildProfitPulse(ctx, in.UserID, in.Size.W, in.Size.H)
		},
	})
}
