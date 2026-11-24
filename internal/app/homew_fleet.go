package app

import (
	"context"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetFleet, Title: "Fleet Overview", Category: wcatPilots, Feature: "fleet",
		Href:        "/characters/",
		Description: "Every linked character at a glance: where they are, what they're flying, what they're training.",
		Default:     wSize{3, 2},
		Forms: []homeWidgetForm{
			{Name: "all", Sizes: "2x2 2x3 3x2 3x3", Says: "Every pilot: location, ship, training and wallet."},
		},
		Needs: []string{esi.SnapProfile, esi.SnapCorpInfo, esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapMailLabels},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			return app.buildFleet(ctx, in.UserID, in.Bundles)
		},
	})
}
