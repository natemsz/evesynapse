package app

import (
	"context"

	"evesynapse/internal/ideas"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetTradeIdeas, Title: "Trade Ideas", Category: wcatMarket, Feature: "tradeideas",
		Requires:    "tradeideas",
		Href:        "/market/ideas/",
		Description: "A model's read on the Forge spread scanner, one tab per trade strategy. Refreshed twice a day; the Trade Ideas page has the full list.",
		Default:     wSize{2, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3", Says: "Trade ideas, one tab per strategy."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return ideas.Stored(ctx, app.queries, app.cfg.TradeIdeasConfigured())
		},
	})
}
