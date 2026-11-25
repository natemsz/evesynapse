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
			{Name: "small", Sizes: "1x1", Says: "The top idea in short."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "Each strategy with its top idea in short."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "One tab per strategy, each idea in short."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "One tab per strategy, each idea in full."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Every strategy side by side."},
		},
		Build: func(ctx context.Context, app *Application, in *widgetInput) any {
			v := &tradeIdeasView{Widget: ideas.Stored(ctx, app.queries, app.cfg.TradeIdeasConfigured())}
			switch {
			case v.HasIdeas:
				v.Note = "As of " + v.AsOf + ", " + v.Model + " on the Forge spread scanner."
			case v.Configured:
				v.Empty = wEmpty{Text: "No ideas yet. They are written after the next Forge market sweep."}
			default:
				v.Empty = wEmpty{Text: "Trade Ideas are not set up on this server."}
			}
			return v
		},
	})
}

// tradeIdeasView is the stored answer as the Trade Ideas forms draw it.
type tradeIdeasView struct {
	*ideas.Widget
	Note  string // when the answer was written, and by which model
	Empty wEmpty
}
