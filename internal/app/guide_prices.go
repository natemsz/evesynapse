package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	if app.storedPricesCache != nil && app.storedPricesStamp == meta.FetchedAt {
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

// refreshGuidePrices mirrors GET /markets/prices/ into the
// guide_prices table. One public call; ESI's Expires header is
// the only cadence (an hour's fallback when it is missing), so
// a fresh table is never refetched early. The rows replace the
// previous set wholesale inside one transaction, and the live
// in-memory guide is refreshed from the same payload so the
// Market page shares it. Reports whether it stored, and
// whether ESI's error limit stopped it.
func (app *Application) refreshGuidePrices(ctx context.Context) (stored bool, limited bool) {
	now := time.Now()
	if meta, ok := app.guideMeta(ctx); ok && meta.CachedUntil != "" {
		if until, err := time.Parse(time.RFC3339, meta.CachedUntil); err == nil && now.Before(until) {
			return false, false // still inside ESI's cache window
		}
	}

	body, header, err := app.esi.FetchRaw(ctx, "", "/markets/prices/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return false, true
		}
		if ctx.Err() == nil {
			logging.Errorf("worker: guide prices: %v", err)
		}
		return false, false
	}
	var rows []esi.MarketPrice
	if err := json.Unmarshal(body, &rows); err != nil {
		logging.Errorf("worker: guide prices: decode: %v", err)
		return false, false
	}

	cachedUntil := now.Add(time.Hour)
	if exp := header.Get("Expires"); exp != "" {
		if t, perr := http.ParseTime(exp); perr == nil {
			cachedUntil = t
		}
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: guide prices: begin tx: %v", err)
		return false, false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteGuidePrices(ctx); err != nil {
		logging.Errorf("worker: guide prices: clear: %v", err)
		return false, false
	}
	for _, row := range rows {
		if row.TypeID <= 0 {
			continue
		}
		if err := qtx.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
			TypeID:        row.TypeID,
			AdjustedPrice: row.AdjustedPrice,
			AveragePrice:  row.AveragePrice,
		}); err != nil {
			logging.Errorf("worker: guide prices: store type %d: %v", row.TypeID, err)
			return false, false
		}
	}
	if err := qtx.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt:   now.UTC().Format(time.RFC3339),
		CachedUntil: cachedUntil.UTC().Format(time.RFC3339),
	}); err != nil {
		logging.Errorf("worker: guide prices: store meta: %v", err)
		return false, false
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: guide prices: commit: %v", err)
		return false, false
	}

	// The same payload refreshes the live guide the Market page
	// reads, so both copies agree until the next window.
	prices := make(map[int64]esi.MarketPrice, len(rows))
	for _, row := range rows {
		prices[row.TypeID] = row
	}
	app.pricesMu.Lock()
	app.prices = prices
	app.pricesExpiry = cachedUntil
	app.pricesMu.Unlock()

	logging.Infof("worker: guide prices stored (%d types, fresh until %s)",
		len(rows), cachedUntil.UTC().Format(time.RFC3339))
	return true, false
}
