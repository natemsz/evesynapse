package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Daily wallet history: the worker writes one row per character per day
// from the snapshots it already keeps. No extra ESI calls.
//
// A day's row keeps the latest values seen that day: it is rewritten
// when the wallet snapshot advances and left alone otherwise. net_worth
// is filled only when the house price sources can value assets the way
// the home Net worth widget does; otherwise it stays NULL, which must
// never read as zero.
// ---------------------------------------------------------------------------

// sampleWalletHistory records (or refreshes) today's wallet
// history row for one character. Best-effort: every failure is a
// quiet return — history must never break the sync cycle.
func (app *Application) sampleWalletHistory(ctx context.Context, ch db.Character, now time.Time) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: ch.CharacterID, Kind: esi.SnapWallet,
	})
	if err != nil {
		return // no wallet snapshot yet — nothing to record
	}
	var balance float64
	if err := json.Unmarshal([]byte(snap.Payload), &balance); err != nil {
		return
	}

	day := now.UTC().Format("2006-01-02")
	if existing, err := app.queries.GetWalletHistorySample(ctx, db.GetWalletHistorySampleParams{
		UserID: ch.UserID, CharacterID: ch.CharacterID, Day: day,
	}); err == nil {
		// Already recorded from this exact wallet snapshot:
		// nothing newer exists to write.
		if !snap.FetchedAt.After(existing.SampledAt) {
			return
		}
	}

	netWorth := sql.NullFloat64{}
	// Net worth is sampled only when a house price source exists
	// at all — swept typicals or stored estimated sells — so a
	// day with no prices leaves net worth unset rather than
	// storing a bare balance.
	if typicals := app.TypicalSells(ctx); len(typicals) > 0 || len(app.TrueValues(ctx)) > 0 {
		if total, ok := app.characterNetWorth(ctx, ch.CharacterID, balance, typicals); ok {
			netWorth = sql.NullFloat64{Float64: total, Valid: true}
		}
	}

	if err := app.queries.UpsertWalletHistorySample(ctx, db.UpsertWalletHistorySampleParams{
		UserID:      ch.UserID,
		CharacterID: ch.CharacterID,
		Day:         day,
		Balance:     balance,
		NetWorth:    netWorth,
		SampledAt:   now.UTC(),
	}); err != nil {
		logging.Errorf("worker: wallet history sample for character %d: %v", ch.CharacterID, err)
	}
}

// characterNetWorth values one character the way the home Net
// worth widget does: wallet balance, plus assets at house
// prices, plus buy-order escrow. ok=false when the stored data
// can't support the estimate (no asset or order snapshot, or
// assets none of the price sources can value) — the caller then
// leaves the day's net worth unset rather than storing a
// balance wearing a net-worth label.
func (app *Application) characterNetWorth(ctx context.Context, characterID int64, balance float64, typicals map[int64]float64) (float64, bool) {
	total := balance
	valued := false
	// The same price order as the home widget's (buildNetWorth):
	// the swept Forge typical, then the true-value estimated sell.
	trueValues := app.storedTrueValues(ctx)

	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapAssets,
	}); err == nil {
		var assets []esi.Asset
		if json.Unmarshal([]byte(snap.Payload), &assets) == nil {
			for _, a := range assets {
				price, ok := pickValuationPrice(typicals[a.TypeID], estSellOf(trueValues, a.TypeID))
				if !ok {
					continue
				}
				total += float64(a.Quantity) * price
				valued = true
			}
		}
	}

	escrowSeen := false
	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapOrders,
	}); err == nil {
		var orders []esi.CharOrder
		if json.Unmarshal([]byte(snap.Payload), &orders) == nil {
			for _, o := range orders {
				if o.IsBuyOrder {
					total += o.Escrow
					escrowSeen = true
				}
			}
		}
	}

	return total, valued || escrowSeen
}
