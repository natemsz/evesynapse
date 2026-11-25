package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetWalletFlow, Title: "Wallet Flow", Category: wcatMoney, Feature: "wallet",
		Requires:    "wallet",
		Href:        "/wallet/",
		Description: "Income vs spend over 7 and 30 days with top sources, sinks, and a per-character breakdown.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "2x1 2x2 2x3 3x1 3x2 3x3", Says: "Income against spend over 7 and 30 days."},
		},
		Needs: []string{esi.SnapWalletJournal},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildWalletFlow(in.Bundles)
		},
	})
}
