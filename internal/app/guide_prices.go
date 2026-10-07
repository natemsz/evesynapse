package app

import (
	"context"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// v0.3.04: the stored market guide (schema 021). GET
// /markets/prices/ is one public call covering every type in
// the game, so the worker keeps it mirrored in the guide_prices
// table on ESI's own cache window. Asset valuation reads the
// table through here — it no longer depends on someone having
// visited the Market page first (the in-memory copy the Market
// page fetches live still wins when present: freshest first).
//
// This file is the reading side. The worker's refresh
// (refreshGuidePrices) is in market_worker.go.
// ---------------------------------------------------------------------------

// guideMeta reads the refresh bookkeeping row; ok=false before
// the worker's first successful refresh.
func (app *Application) guideMeta(ctx context.Context) (db.GuidePricesMetum, bool) {
	meta, err := app.queries.GetGuidePricesMeta(ctx)
	if err != nil {
		return db.GuidePricesMetum{}, false
	}
	return meta, true
}

// storedGuidePrices returns the worker-stored price guide as a
// map, reloading from the table only when a refresh has landed
// since the last read (the fetched_at stamp is the version).
// Nil when nothing has been stored yet — "no guide at all" is
// distinct from "a guide that lacks this type".
func (app *Application) storedGuidePrices(ctx context.Context) map[int64]esi.MarketPrice {
	meta, ok := app.guideMeta(ctx)
	if !ok {
		return nil
	}
	app.storedPricesMu.Lock()
	defer app.storedPricesMu.Unlock()
	if app.storedPricesCache != nil && app.storedPricesStamp.Equal(meta.FetchedAt) {
		return app.storedPricesCache
	}
	rows, err := app.queries.ListGuidePrices(ctx)
	if err != nil {
		logging.Errorf("prices: load stored guide: %v", err)
		return app.storedPricesCache // nil on first failure: honest "none"
	}
	if len(rows) == 0 {
		app.storedPricesCache, app.storedPricesStamp = nil, meta.FetchedAt
		return nil
	}
	prices := make(map[int64]esi.MarketPrice, len(rows))
	for _, row := range rows {
		prices[row.TypeID] = esi.MarketPrice{
			TypeID:        row.TypeID,
			AdjustedPrice: row.AdjustedPrice,
			AveragePrice:  row.AveragePrice,
		}
	}
	app.storedPricesCache, app.storedPricesStamp = prices, meta.FetchedAt
	return prices
}

// valuationPrices is the price map asset valuation runs on:
// the live in-memory guide when a Market visit has fetched it,
// the worker-stored guide otherwise. Nil only when neither
// exists yet (first minutes of a fresh deployment).
func (app *Application) valuationPrices(ctx context.Context) map[int64]esi.MarketPrice {
	if prices := app.cachedPrices(); prices != nil {
		return prices
	}
	return app.storedGuidePrices(ctx)
}
