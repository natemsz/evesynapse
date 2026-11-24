package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetNetWorth, Title: "Net Worth", Category: wcatMoney, Feature: "networth",
		Href:        "/wallet/",
		Description: "Wallets, assets and open-order escrow across all characters, at market prices. An estimate.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "1x1 1x2", Says: "The total, how it splits, and its history."},
		},
		Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return in.Shared.NetWorth(ctx)
		},
	})
}
