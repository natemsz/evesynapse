package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetGlance, Title: "At a Glance", Category: wcatOverview, Feature: "networth",
		Href:        "/characters/",
		Description: "The fleet's key figures in one band: net worth, liquid wallets, assets, and how many pilots are training.",
		Default:     wSize{3, 1},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "2x1 3x1", Says: "Net worth, wallets, assets and training as tiles."},
		},
		Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders, esi.SnapSkillqueue},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildGlance(in.Bundles, in.Shared.NetWorth(ctx))
		},
	})
}
