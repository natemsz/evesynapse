package app

// Reading stored snapshots. A snapshot is a dataset as ESI returned
// it, kept as JSON; these decode one for whoever needs it. Pages and
// worker passes both use them, and none of them fetches anything.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"

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

// ---------------------------------------------------------------------------
// Per-cycle snapshot sharing. The worker's minute cycle runs
// several passes over the same stored snapshots: the market pass
// reads the orders snapshot in two of its passes, the intel orbit
// reads the contracts snapshot the economy pass warmed from, and
// so on. A context carrying a cycle snapshot cache (refreshCycle
// installs one) makes those passes share one stored read and one
// decode per (character, kind). Callers treat decoded snapshots
// as read-only, which every cycle consumer does. Contexts without
// a cache (pages, tests, one-off callers) read and decode exactly
// as a lone caller always has.
// ---------------------------------------------------------------------------

type snapshotKey struct {
	characterID int64
	kind        string
}

type cycleSnapshotCache struct {
	mu       sync.Mutex
	payloads map[snapshotKey]string
	absent   map[snapshotKey]bool // no snapshot row this cycle
	decoded  map[snapshotKey]any  // loadSnapshot's decode of the payload
	bad      map[snapshotKey]bool // payload present but undecodable
}

type cycleSnapshotCtxKey struct{}

func withCycleSnapshotCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, cycleSnapshotCtxKey{}, &cycleSnapshotCache{
		payloads: map[snapshotKey]string{},
		absent:   map[snapshotKey]bool{},
		decoded:  map[snapshotKey]any{},
		bad:      map[snapshotKey]bool{},
	})
}

func cycleSnapshots(ctx context.Context) *cycleSnapshotCache {
	cache, _ := ctx.Value(cycleSnapshotCtxKey{}).(*cycleSnapshotCache)
	return cache
}

// snapshotPayload returns one character's stored snapshot
// payload; ok=false when no snapshot row exists. With a cycle
// cache in ctx the snapshots table is read at most once per
// (character, kind) per cycle and every reader shares the result.
func (app *Application) snapshotPayload(ctx context.Context, characterID int64, kind string) (string, bool) {
	cache := cycleSnapshots(ctx)
	if cache == nil {
		snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
		if err != nil {
			return "", false
		}
		return snap.Payload, true
	}
	key := snapshotKey{characterID: characterID, kind: kind}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if payload, ok := cache.payloads[key]; ok {
		return payload, true
	}
	if cache.absent[key] {
		return "", false
	}
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		cache.absent[key] = true
		return "", false
	}
	cache.payloads[key] = snap.Payload
	return snap.Payload, true
}

// loadSnapshot decodes one character's snapshot payload into T;
// ok=false when no snapshot exists or it will not decode. Shared
// by the orbit derivations, which only ever read stored payloads.
// Under a cycle cache the decode is shared too: the first reader
// of a (character, kind) decodes, later readers get that decode.
func loadSnapshot[T any](app *Application, ctx context.Context, characterID int64, kind string) (T, bool) {
	var out T
	cache := cycleSnapshots(ctx)
	key := snapshotKey{characterID: characterID, kind: kind}
	if cache != nil {
		cache.mu.Lock()
		decoded, shared := cache.decoded[key]
		bad := cache.bad[key]
		cache.mu.Unlock()
		if bad {
			return out, false
		}
		if shared {
			if typed, ok := decoded.(T); ok {
				return typed, true
			}
			// A kind is only ever decoded as one type; a mismatch
			// means that stopped being true — decode afresh below
			// rather than handing back the wrong shape.
		}
	}
	payload, ok := app.snapshotPayload(ctx, characterID, kind)
	if !ok {
		return out, false
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		if cache != nil {
			cache.mu.Lock()
			cache.bad[key] = true
			cache.mu.Unlock()
		}
		return out, false
	}
	if cache != nil {
		cache.mu.Lock()
		cache.decoded[key] = out
		cache.mu.Unlock()
	}
	return out, true
}

// loadOrdersSnapshot decodes one character's open-orders
// snapshot; ok=false when none exists or it will not decode.
func (app *Application) loadOrdersSnapshot(ctx context.Context, characterID int64) ([]esi.CharOrder, bool) {
	return loadSnapshot[[]esi.CharOrder](app, ctx, characterID, esi.SnapOrders)
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

// storedSnapshot decodes one of a character's datasets as the worker
// last stored it. It never fetches: a page shows what is held, and the
// worker keeps the character somebody is looking at as fresh as ESI
// allows (worker_tiers.go). A dataset not stored yet is sql.ErrNoRows,
// and puts the character at the front of the worker's next cycle.
func (app *Application) storedSnapshot(ctx context.Context, ch db.Character, kind string, out any) error {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			app.markCharacterPriority(ch.CharacterID)
		}
		return err
	}
	return json.Unmarshal([]byte(snap.Payload), out)
}
