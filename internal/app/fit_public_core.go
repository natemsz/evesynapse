package app

import (
	"context"
	"encoding/json"
	"time"

	"evesynapse/internal/app/module"

	"evesynapse/internal/fitview"
)

// publicPage is the page data anonymous-safe routes start from:
// the session's login state, never assumed.
func (app *Application) publicPage(ctx context.Context) pageData {
	loggedIn := app.sessions.GetBool(ctx, sessionAuthenticated)
	return pageData{
		LoggedIn:      loggedIn,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// Head metadata for every public page is the one shared
// mechanism: module.PageMeta, composed by module.RenderPageMeta
// (the fit page's original escaping discipline, now with the
// canonical and twitter card the mechanism adds).

// publicFitsPath, publicFitCard and publicFitCardOf are the public fits
// module's card, read by the ship pages.
const publicFitsPath = module.PublicFitsPath

type publicFitCard = module.PublicFitCard

var publicFitCardOf = module.PublicFitCardOf

// PublicFitBody is the fit-derived part of a public fit's page: priced
// now the way the fitting tool's stats panel does, and the modules,
// simulation and EFT read from the stored page (char_fit_public_store.go);
// a view never runs the engine.
func (app *Application) PublicFitBody(ctx context.Context, fitID int64, updatedAt time.Time, itemsJSON string, shipTypeID int64) module.PublicFitBody {
	var doc fitview.FitDoc
	if json.Unmarshal([]byte(itemsJSON), &doc) != nil {
		return module.PublicFitBody{Preparing: true}
	}
	if doc.ShipTypeID == 0 {
		doc.ShipTypeID = shipTypeID
	}
	cost := app.fitBuilder().FitCost(ctx, &doc)
	body := module.PublicFitBody{PriceKnown: cost.Known, PriceRaw: cost.TotalRaw, PriceTotal: cost.Total}
	if stored, ok := app.loadPublicFitStored(ctx, fitID, updatedAt); ok {
		body.Sections, body.EFT = stored.Sections, stored.EFT
		if stored.Sim != nil {
			// The stored page may be shared between requests: render
			// a copy, as the tool's own workbench made read-only.
			sim := *stored.Sim
			sim.ReadOnly = true
			sim.Cost = cost
			body.Sim = &sim
		}
	} else {
		body.Preparing = true
	}
	return body
}
