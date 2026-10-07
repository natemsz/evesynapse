package app

// Reading stored snapshots. A snapshot is a dataset as ESI returned
// it, kept as JSON; these decode one for whoever needs it. Pages and
// worker passes both use them, and none of them fetches anything.

import (
	"context"
	"encoding/json"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// loadCorpSnapshot decodes the stored snapshot payload for
// (character, kind) into out. It reports false when there is no
// snapshot row or the payload doesn't decode — callers then render
// the warming/role states. It never touches the network, unlike
// esi.GetCached (which refetches stale snapshots); the worker keeps
// these snapshots warm.
func (app *Application) loadCorpSnapshot(ctx context.Context, characterID int64, kind string, out any) bool {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return false
	}
	return json.Unmarshal([]byte(snap.Payload), out) == nil
}

// loadSnapshot decodes one character's snapshot payload into T;
// ok=false when no snapshot exists or it will not decode. Shared
// by the orbit derivations, which only ever read stored payloads.
func loadSnapshot[T any](app *Application, ctx context.Context, characterID int64, kind string) (T, bool) {
	var out T
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal([]byte(snap.Payload), &out); err != nil {
		return out, false
	}
	return out, true
}

// loadOrdersSnapshot decodes one character's open-orders
// snapshot; ok=false when none exists or it will not decode.
func (app *Application) loadOrdersSnapshot(ctx context.Context, characterID int64) ([]esi.CharOrder, bool) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: esi.SnapOrders})
	if err != nil {
		return nil, false
	}
	var orders []esi.CharOrder
	if err := json.Unmarshal([]byte(snap.Payload), &orders); err != nil {
		return nil, false
	}
	return orders, true
}

// loadGlobalSnapshot decodes a stored global payload into out. It
// reports false when there is no row or the payload doesn't
// decode. DB only — unlike esi.GetCached it never refetches; the
// worker keeps the global store warm.
func (app *Application) loadGlobalSnapshot(ctx context.Context, kind string, out any) bool {
	snap, err := app.queries.GetGlobalSnapshot(ctx, kind)
	if err != nil {
		return false
	}
	return json.Unmarshal([]byte(snap.Payload), out) == nil
}
