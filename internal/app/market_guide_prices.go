package app

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The stored market guide (schema 021). GET /markets/prices/ is one
// public call covering every type in the game, so the worker keeps it
// mirrored in the guide_prices table on ESI's own cache window. Asset
// valuation reads the table through here — it does not depend on
// someone having visited the Market page first (the in-memory copy the
// Market page fetches live still wins when present: freshest first).
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
		return app.storedPricesCache // nil on first failure: "none"
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

// TypicalSells returns the swept typical sell per type —
// the first tier of the house price order (own swept typicals,
// then the stored true-value estimated sell, then unpriced; the
// guide's average is banned as a price source, owner doctrine
// 2026-10-17). The Forge's book is the base map; a type The
// Forge carries no typical for fills from its own global-market
// row (globalMarketRegion) when the sweep holds one — the
// global book is that type's own market (PLEX), never another
// hub's, and a Forge row always wins where both exist. Only
// types with a usable typical sell appear.
// Cached on the true-value window: the stats reload a few times
// an hour, not per render. This is also the resolution point the
// Fuzzwork fallback slots into later, below the estimated sell.
//
// Bounded read: the stats table carries one row per type traded
// in the region — the same whole-table read the true-value pass
// already makes of it each cycle. The global region's table is
// a handful of rows on top.
func (app *Application) TypicalSells(ctx context.Context) map[int64]float64 {
	app.forgeTypicalsMu.Lock()
	defer app.forgeTypicalsMu.Unlock()
	if app.forgeTypicalsCache != nil && time.Since(app.forgeTypicalsLoaded) < trueValueCacheWindow {
		return app.forgeTypicalsCache
	}
	rows, err := app.queries.ListMarketRegionStatsByRegion(ctx, defaultMarketRegion)
	if err != nil {
		logging.Errorf("prices: load forge typical sells: %v", err)
		return app.forgeTypicalsCache
	}
	out := make(map[int64]float64, len(rows))
	for _, row := range rows {
		if row.TypicalSell > 0 {
			out[row.TypeID] = row.TypicalSell
		}
	}
	// The global fill: types with no Forge typical take their
	// global-market row's typical, at the same own-swept tier.
	gRows, err := app.queries.ListMarketRegionStatsByRegion(ctx, globalMarketRegion)
	if err != nil {
		logging.Errorf("prices: load global typical sells: %v", err)
	} else {
		for _, row := range gRows {
			if row.TypicalSell > 0 {
				if _, ok := out[row.TypeID]; !ok {
					out[row.TypeID] = row.TypicalSell
				}
			}
		}
	}
	app.forgeTypicalsCache, app.forgeTypicalsLoaded = out, time.Now()
	return out
}
