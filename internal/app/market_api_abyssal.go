package app

// Abyssal wants (schema 108): mutated items an API caller asked to
// have priced that the corpus has not seen. A request never reaches
// ESI itself; it queues the item, and this pass reads a bounded
// handful of rolls per cycle on its own fetch lane, landing them in
// abyssal_items the same way a contract listing's item lands (no
// sighting: the caller's item is not a listing, so it never enters
// the calibration corpus). The scoring pass then prices it like any
// other, and the caller's next ask finds the stored estimate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	abyssalWantsPerCycle  = 20
	abyssalWantRetryAfter = 10 * time.Minute
	abyssalWantMaxTries   = 5
	abyssalWantKeep       = 7 * 24 * time.Hour
)

// Want states.
const (
	abyssalWantPending  = "pending"
	abyssalWantFetched  = "fetched"
	abyssalWantNotFound = "not_mutated" // ESI knows no dynamic item by that id and type
	abyssalWantFailed   = "failed"
)

// refreshAbyssalWants is the worker pass.
func (app *Application) refreshAbyssalWants(ctx context.Context) (stored int, limited bool) {
	now := time.Now().UTC()
	_ = app.queries.PruneAbyssalWants(ctx, now.Add(-abyssalWantKeep))
	wants, err := app.queries.ListPendingAbyssalWants(ctx, db.ListPendingAbyssalWantsParams{
		RetryBefore: now.Add(-abyssalWantRetryAfter), RowCap: abyssalWantsPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: abyssal wants: list: %v", err)
		return 0, false
	}
	allowance := &fetchBudget{left: abyssalWantsPerCycle}
	for _, w := range wants {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		state, err := app.fetchAbyssalWant(ctx, w.ItemID, w.TypeID, now)
		if errors.Is(err, esi.ErrErrorLimit) {
			return stored, true
		}
		if err != nil {
			logging.Warnf("worker: abyssal wants: item %d: %v", w.ItemID, err)
			if w.Attempts+1 >= abyssalWantMaxTries {
				state = abyssalWantFailed
			} else {
				state = abyssalWantPending
			}
		}
		if err := app.queries.SetAbyssalWantState(ctx, db.SetAbyssalWantStateParams{
			ItemID: w.ItemID, State: state, TriedAt: timeSet(now),
		}); err != nil {
			logging.Errorf("worker: abyssal wants: mark %d: %v", w.ItemID, err)
		}
		if state == abyssalWantFetched {
			stored++
		}
	}
	return stored, false
}

// fetchAbyssalWant reads one item's rolls and stores them.
func (app *Application) fetchAbyssalWant(ctx context.Context, itemID, typeID int64, now time.Time) (string, error) {
	body, _, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/dogma/dynamic/items/%d/%d/", typeID, itemID))
	if err != nil {
		if code, ok := esi.StatusCode(err); ok && (code == http.StatusNotFound || code == 422) {
			_ = app.queries.InsertAbyssalProbe(ctx, db.InsertAbyssalProbeParams{ItemID: itemID, TypeID: typeID, IsDynamic: false, ProbedAt: now})
			return abyssalWantNotFound, nil
		}
		return "", err
	}
	var dyn esi.DynamicItem
	if err := json.Unmarshal(body, &dyn); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	attrs, err := json.Marshal(dyn.DogmaAttributes)
	if err != nil {
		return "", fmt.Errorf("encode rolls: %w", err)
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err := q.InsertAbyssalProbe(ctx, db.InsertAbyssalProbeParams{ItemID: itemID, TypeID: typeID, IsDynamic: true, ProbedAt: now}); err != nil {
		return "", err
	}
	if err := q.UpsertAbyssalItem(ctx, db.UpsertAbyssalItemParams{
		ItemID: itemID, TypeID: typeID, SourceTypeID: dyn.SourceTypeID, MutatorTypeID: dyn.MutatorTypeID,
		CreatedBy: dyn.CreatedBy, Attributes: attrs, FetchedAt: now, FirstSeenAt: now,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return abyssalWantFetched, nil
}
