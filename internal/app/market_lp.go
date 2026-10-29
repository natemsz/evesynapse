package app

// The LP store mirror (schema 102): every NPC corporation's loyalty
// store, read from public ESI, and each offer's value per swept hub
// region. The pass seeds the store list from ESI's NPC corporation
// list once a day, re-reads a bounded slice of due stores each cycle
// (each store once a day), and names the stores that sell something.
// Values are written from the stat pack in SQL (RefreshLPOfferValues):
// for every hub when a store's offers change, and for one region in
// the transaction that completes its sweep, so the LP store page only
// ever reads finished rows.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// lpCorpListKind names the NPC corporation list's row in
	// market_fetch_state.
	lpCorpListKind = "lp_npccorps"
	// lpStoreGate is how often the store list and each store's
	// offers are re-read: offers change with game patches, not
	// by the hour.
	lpStoreGate = 24 * time.Hour
	// lpStoresPerCycle bounds the stores read in one cycle; the
	// full list (about 280) settles within a quarter hour of a
	// fresh start, then trickles at the daily gate.
	lpStoresPerCycle = 20
	// lpFetchesPerCycle is the pass's own fetch lane.
	lpFetchesPerCycle = lpStoresPerCycle + 4
)

// lpOffer is one ESI loyalty store offer.
type lpOffer struct {
	OfferID       int64   `json:"offer_id"`
	TypeID        int64   `json:"type_id"`
	Quantity      int64   `json:"quantity"`
	LPCost        int64   `json:"lp_cost"`
	ISKCost       float64 `json:"isk_cost"`
	AKCost        int64   `json:"ak_cost"`
	RequiredItems []struct {
		TypeID   int64 `json:"type_id"`
		Quantity int64 `json:"quantity"`
	} `json:"required_items"`
}

// refreshLPStores is the worker pass. It reports how many stores it
// read and whether ESI's error limit stopped it.
func (app *Application) refreshLPStores(ctx context.Context) (stored int, limited bool) {
	allowance := &fetchBudget{left: lpFetchesPerCycle}
	if app.marketFetchDue(ctx, lpCorpListKind, lpStoreGate) && allowance.take() {
		var corps []int64
		if err := app.esi.Get(ctx, "", "/corporations/npccorps/", &corps); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return 0, true
			}
			logging.Warnf("worker: lp stores: corporation list: %v", err)
			app.recordMarketFetch(ctx, lpCorpListKind, fetchStateError, "corporation list unavailable")
		} else {
			for _, id := range corps {
				if err := app.queries.UpsertLPStoreCorp(ctx, id); err != nil {
					logging.Errorf("worker: lp stores: seed %d: %v", id, err)
				}
			}
			app.recordMarketFetch(ctx, lpCorpListKind, fetchStateOK, "")
		}
	}

	due, err := app.queries.ListDueLPStoreCorps(ctx, db.ListDueLPStoreCorpsParams{
		Cutoff: sql.NullTime{Time: time.Now().UTC().Add(-lpStoreGate), Valid: true}, RowCap: lpStoresPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: lp stores: due list: %v", err)
		return 0, false
	}
	changed := false
	for _, corpID := range due {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		var offers []lpOffer
		err := app.esi.Get(ctx, "", fmt.Sprintf("/loyalty/stores/%d/offers/", corpID), &offers)
		now := time.Now().UTC()
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			return stored, true
		case err != nil:
			if code, ok := esi.StatusCode(err); ok && code == http.StatusNotFound {
				// No store: settled, not retried until the gate.
				if app.storeLPOffers(ctx, corpID, nil, "none", now) {
					changed = true
				}
				continue
			}
			logging.Warnf("worker: lp stores: offers for %d: %v", corpID, err)
			// Transient: stamp it so the slice moves on; it comes
			// due again in an hour.
			_ = app.queries.MarkLPStoreCorp(ctx, db.MarkLPStoreCorpParams{
				CorporationID: corpID, State: fetchStateError, OfferCount: 0,
				FetchedAt: sql.NullTime{Time: now.Add(-lpStoreGate + time.Hour), Valid: true},
			})
			continue
		}
		if app.storeLPOffers(ctx, corpID, offers, fetchStateOK, now) {
			changed = true
		}
		stored++
	}

	if ids, err := app.queries.ListUnnamedLPStoreCorps(ctx); err == nil && len(ids) > 0 {
		names, ltd := app.resolveUniverseNamesLimited(ctx, allowance, ids)
		for id, name := range names {
			if err := app.queries.SetLPStoreCorpName(ctx, db.SetLPStoreCorpNameParams{CorporationID: id, Name: name}); err != nil {
				logging.Errorf("worker: lp stores: name %d: %v", id, err)
			}
		}
		limited = limited || ltd
	}

	if changed {
		now := time.Now().UTC()
		for _, region := range marketRegions {
			if err := app.refreshLPOfferValues(ctx, app.queries, region.ID, now); err != nil {
				logging.Errorf("worker: lp stores: values for %s: %v", marketRegionLabel(region.ID), err)
			}
		}
	}
	return stored, limited
}

// storeLPOffers replaces one store's offers in one transaction and
// stamps the store. It reports whether the offers were replaced.
func (app *Application) storeLPOffers(ctx context.Context, corpID int64, offers []lpOffer, state string, now time.Time) bool {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: lp stores: begin %d: %v", corpID, err)
		return false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteLPOffersByCorp(ctx, corpID); err != nil {
		logging.Errorf("worker: lp stores: clear %d: %v", corpID, err)
		return false
	}
	if err := qtx.DeleteLPOfferRequirementsByCorp(ctx, corpID); err != nil {
		logging.Errorf("worker: lp stores: clear requirements %d: %v", corpID, err)
		return false
	}
	for _, o := range offers {
		if o.OfferID <= 0 || o.TypeID <= 0 {
			continue
		}
		quantity := max(o.Quantity, 1)
		if err := qtx.InsertLPOffer(ctx, db.InsertLPOfferParams{
			CorporationID: corpID, OfferID: o.OfferID, TypeID: o.TypeID, Quantity: quantity,
			LpCost: o.LPCost, IskCost: o.ISKCost, AkCost: o.AKCost,
		}); err != nil {
			logging.Errorf("worker: lp stores: offer %d/%d: %v", corpID, o.OfferID, err)
			return false
		}
		for _, req := range o.RequiredItems {
			if req.TypeID <= 0 || req.Quantity <= 0 {
				continue
			}
			if err := qtx.InsertLPOfferRequirement(ctx, db.InsertLPOfferRequirementParams{
				CorporationID: corpID, OfferID: o.OfferID, TypeID: req.TypeID, Quantity: req.Quantity,
			}); err != nil {
				logging.Errorf("worker: lp stores: requirement %d/%d: %v", corpID, o.OfferID, err)
				return false
			}
		}
	}
	if err := qtx.MarkLPStoreCorp(ctx, db.MarkLPStoreCorpParams{
		CorporationID: corpID, State: state, OfferCount: int64(len(offers)), FetchedAt: sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		logging.Errorf("worker: lp stores: stamp %d: %v", corpID, err)
		return false
	}
	return tx.Commit() == nil
}

// refreshLPOfferValues rewrites one hub region's offer values from
// the stat pack. q may be a transaction's queries: the region sweep
// calls it inside the transaction that replaces the region's stats.
func (app *Application) refreshLPOfferValues(ctx context.Context, q *db.Queries, regionID int64, now time.Time) error {
	if _, ok := marketRegionName(regionID); !ok {
		return nil // the global book carries no LP products
	}
	if err := q.DeleteLPOfferValuesByRegion(ctx, regionID); err != nil {
		return err
	}
	_, err := q.RefreshLPOfferValues(ctx, db.RefreshLPOfferValuesParams{RegionID: regionID, Now: now})
	return err
}
