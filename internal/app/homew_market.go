package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetMarket, Title: "Market", Category: wcatMarket, Feature: "market",
		Requires:    "market",
		Href:        "/orders/",
		Description: "Open orders across characters: counts, sell/buy value, orders expiring soonest.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2 1x3 2x1 2x2 2x3 3x1 3x2 3x3", Says: "Open orders and the ones expiring soonest."},
		},
		Needs: []string{esi.SnapOrders},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildMarket(ctx, in.Bundles, app.ordersConfigFor(ctx, in.UserID), in.Shared.market)
		},
	})
}
