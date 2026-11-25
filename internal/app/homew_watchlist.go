package app

import (
	"context"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetWatchlist, Title: "Market Watchlist", Category: wcatMarket, Feature: "market_public",
		Href:        "/market/",
		Description: "The items you're watching: latest prices, 7- and 30-day moves, and a flag when one crosses your alert line.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3 3x1 3x2 3x3", Says: "Watched items with their price moves."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildWatchlistWidget(ctx, in.Shared.market)
		},
	})
}
