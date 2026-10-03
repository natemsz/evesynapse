package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Market worker pass (Phase 5): keeps market_history warm for the
// (region, type) pairs somebody actually looks at, and computes
// per-order health from regional order books. Everything here is
// public ESI — no character token — but it spends from the
// cycle's shared fetch allowance like every other pass, and a
// 420/429 stops it until the next cycle.
//
// What wants history, in priority order:
//   1. watchlist entries (any user's — the alerts read them),
//   2. item-page "wants" (a user opened a type with no rows yet),
//   3. types with open orders on file (their regions ride along).
// A pair is refetched at most once per 20h; CCP's aggregates are
// daily, so fresher would be waste. Order books gate at 10
// minutes on the same fetch-state table (orders Expires on the
// same order of magnitude).
// ---------------------------------------------------------------------------

const (
	// maxHistoryFetchesPerCycle bounds history downloads per
	// worker cycle; a cold watchlist converges over a few
	// one-minute cycles instead of bursting.
	maxHistoryFetchesPerCycle = 10
	// historyRefetchGate is the minimum age of a successful
	// history fetch before the pair is fetched again.
	historyRefetchGate = 20 * time.Hour
	// maxBookFetchesPerCycle bounds regional order-book reads
	// per cycle (each may still paginate in fetchOrderBook).
	maxBookFetchesPerCycle = 10
	// bookRefetchGate is the minimum age of a successful book
	// fetch before that (region, type) is re-read for health.
	bookRefetchGate = 10 * time.Minute
	// historyWantMaxAge is how long an unviewed want keeps its
	// place in the fetch queue.
	historyWantMaxAge = 7 * 24 * time.Hour
)

// marketKey is one (region, type) pair to warm.
type marketKey struct {
	RegionID int64
	TypeID   int64
}

// refreshMarketData runs one market pass, reporting how many
// payloads it stored (history downloads + book reads) and whether
// ESI's error limit stopped it.
func (app *Application) refreshMarketData(ctx context.Context, characters []db.Character, allowance *fetchBudget) (stored int, limited bool) {
	hStored, ltd := app.warmMarketHistory(ctx, allowance)
	stored += hStored
	if ltd {
		return stored, true
	}
	bStored, ltd := app.refreshOrderHealth(ctx, characters, allowance)
	stored += bStored
	return stored, ltd
}

// warmMarketHistory downloads due history pairs, best-priority
// first, at most maxHistoryFetchesPerCycle of them.
func (app *Application) warmMarketHistory(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	candidates, err := app.historyCandidates(ctx)
	if err != nil {
		log.Printf("worker: market history: collect candidates: %v", err)
		return 0, false
	}
	for _, key := range candidates {
		if stored >= maxHistoryFetchesPerCycle || ctx.Err() != nil {
			break
		}
		kind := marketFetchKind("history", key)
		if !app.marketFetchDue(ctx, kind, historyRefetchGate) {
			continue
		}
		if !allowance.take() {
			break
		}
		rows, err := app.fetchMarketHistory(ctx, key)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: market history: ESI error limit hit fetching %s; backing off until next cycle", kind)
				return stored, true
			}
			log.Printf("worker: market history: fetch %s: %v", kind, err)
			app.recordMarketFetch(ctx, kind, fetchStateError, err.Error())
			continue
		}
		for _, row := range rows {
			if err := app.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
				RegionID:   key.RegionID,
				TypeID:     key.TypeID,
				Date:       row.Date,
				Average:    row.Average,
				Highest:    row.Highest,
				Lowest:     row.Lowest,
				Volume:     row.Volume,
				OrderCount: row.OrderCount,
			}); err != nil {
				log.Printf("worker: market history: store %s day %s: %v", kind, row.Date, err)
			}
		}
		app.recordMarketFetch(ctx, kind, fetchStateOK, "")
		stored++
	}
	return stored, false
}

// historyCandidates builds the prioritized fetch queue: watchlist
// pairs first, then recent item-page wants, then types with open
// orders (the region each order sits in).
func (app *Application) historyCandidates(ctx context.Context) ([]marketKey, error) {
	seen := make(map[marketKey]bool)
	var out []marketKey
	add := func(regionID, typeID int64) {
		if regionID <= 0 || typeID <= 0 {
			return
		}
		k := marketKey{RegionID: regionID, TypeID: typeID}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}

	entries, err := app.queries.ListAllWatchlistEntries(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		add(e.RegionID, e.TypeID)
	}

	wants, err := app.queries.ListMarketHistoryWants(ctx,
		time.Now().UTC().Add(-historyWantMaxAge).Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	for _, wn := range wants {
		add(wn.RegionID, wn.TypeID)
	}

	// Types with open orders ride their orders snapshots; a
	// character whose snapshot has not landed yet simply has
	// nothing to contribute this cycle.
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		return nil, err
	}
	var orderKeys []marketKey
	for _, ch := range characters {
		orders, ok := app.loadOrdersSnapshot(ctx, ch.CharacterID)
		if !ok {
			continue
		}
		for _, o := range orders {
			orderKeys = append(orderKeys, marketKey{RegionID: o.RegionID, TypeID: o.TypeID})
		}
	}
	sort.Slice(orderKeys, func(i, j int) bool {
		if orderKeys[i].RegionID != orderKeys[j].RegionID {
			return orderKeys[i].RegionID < orderKeys[j].RegionID
		}
		return orderKeys[i].TypeID < orderKeys[j].TypeID
	})
	for _, k := range orderKeys {
		add(k.RegionID, k.TypeID)
	}
	return out, nil
}

// fetchMarketHistory downloads one pair's daily aggregates.
func (app *Application) fetchMarketHistory(ctx context.Context, key marketKey) ([]esi.MarketHistoryDay, error) {
	path := fmt.Sprintf("/markets/%d/history/?type_id=%d", key.RegionID, key.TypeID)
	body, _, err := app.esi.FetchRaw(ctx, "", path)
	if err != nil {
		return nil, err
	}
	var rows []esi.MarketHistoryDay
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("market history: decode %s: %w", path, err)
	}
	return rows, nil
}

// refreshOrderHealth recomputes per-order health for every open
// sell order on file. Books are shared across characters and
// orders, so the pass groups by (region, type): one book read
// settles every order of that type in that region. Health rows
// for orders that have closed are pruned.
func (app *Application) refreshOrderHealth(ctx context.Context, characters []db.Character, allowance *fetchBudget) (stored int, limited bool) {
	type orderRef struct {
		char  db.Character
		order esi.CharOrder
	}
	groups := make(map[marketKey][]orderRef)
	openByChar := make(map[int64]map[int64]bool)
	for _, ch := range characters {
		if !characterSyncs(ch) {
			continue // parked characters' orders are stale news, not alerts
		}
		orders, ok := app.loadOrdersSnapshot(ctx, ch.CharacterID)
		if !ok {
			// No orders snapshot yet: drop any stale health so
			// the pages never show yesterday's verdict.
			if err := app.queries.DeleteOrderHealthForCharacter(ctx, ch.CharacterID); err != nil {
				log.Printf("worker: order health: clear %d: %v", ch.CharacterID, err)
			}
			continue
		}
		open := make(map[int64]bool)
		for _, o := range orders {
			if o.IsBuyOrder || o.VolumeRemain <= 0 {
				continue
			}
			open[o.OrderID] = true
			k := marketKey{RegionID: o.RegionID, TypeID: o.TypeID}
			groups[k] = append(groups[k], orderRef{char: ch, order: o})
		}
		openByChar[ch.CharacterID] = open
	}

	keys := make([]marketKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RegionID != keys[j].RegionID {
			return keys[i].RegionID < keys[j].RegionID
		}
		return keys[i].TypeID < keys[j].TypeID
	})

	books := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		kind := marketFetchKind("book", key)
		if !app.marketFetchDue(ctx, kind, bookRefetchGate) {
			continue
		}
		if books >= maxBookFetchesPerCycle || !allowance.take() {
			break
		}
		sells, _, _, err := app.fetchOrderBook(ctx, key.RegionID, key.TypeID)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: order health: ESI error limit hit reading book %s; backing off until next cycle", kind)
				return stored, true
			}
			log.Printf("worker: order health: book %s: %v", kind, err)
			app.recordMarketFetch(ctx, kind, fetchStateError, err.Error())
			continue
		}
		books++
		stored++
		app.recordMarketFetch(ctx, kind, fetchStateOK, "")
		now := time.Now().UTC().Format(time.RFC3339)
		for _, ref := range groups[key] {
			status, stationBest, regionBest := computeOrderHealth(ref.order, sells)
			if err := app.queries.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
				CharacterID: ref.char.CharacterID,
				OrderID:     ref.order.OrderID,
				TypeID:      ref.order.TypeID,
				RegionID:    ref.order.RegionID,
				LocationID:  ref.order.LocationID,
				MyPrice:     ref.order.Price,
				StationBest: stationBest,
				RegionBest:  regionBest,
				Status:      status,
				ComputedAt:  now,
			}); err != nil {
				log.Printf("worker: order health: store order %d: %v", ref.order.OrderID, err)
			}
		}
	}

	// Prune verdicts for orders no longer open (filled, expired,
	// or cancelled since the last pass).
	for characterID, open := range openByChar {
		rows, err := app.queries.ListOrderHealthByCharacter(ctx, characterID)
		if err != nil {
			log.Printf("worker: order health: list for %d: %v", characterID, err)
			continue
		}
		for _, row := range rows {
			if !open[row.OrderID] {
				if err := app.queries.DeleteOrderHealthEntry(ctx, db.DeleteOrderHealthEntryParams{
					CharacterID: characterID, OrderID: row.OrderID,
				}); err != nil {
					log.Printf("worker: order health: prune order %d: %v", row.OrderID, err)
				}
			}
		}
	}
	return stored, false
}

// computeOrderHealth judges one open sell order against a
// regional book's sell side. The book includes the user's own
// orders — ESI doesn't say which are whose — so the semantics are
// deliberately simple: an undercut is any book price at least
// 0.01 ISK below the order's own price. (The user's own orders can
// never sit below the order being judged, so a cheaper entry is
// always somebody else's — possibly another of the user's own
// characters, which still means this order is not the front of
// the queue.)
//
//	undercut_station  a cheaper sell sits at the same location
//	undercut_region   nothing to compare at the location, but the
//	                  region's best beats this order
//	best_region_cheaper  front of the queue here, cheaper elsewhere
//	best              front of the queue, region-wide
func computeOrderHealth(o esi.CharOrder, sells []esi.MarketOrder) (status string, stationBest, regionBest float64) {
	stationCount := 0
	for _, s := range sells {
		if s.IsBuyOrder {
			continue
		}
		if regionBest == 0 || s.Price < regionBest {
			regionBest = s.Price
		}
		if s.LocationID == o.LocationID {
			stationCount++
			if stationBest == 0 || s.Price < stationBest {
				stationBest = s.Price
			}
		}
	}
	cheaper := func(best float64) bool {
		return best > 0 && best < o.Price-0.01
	}
	switch {
	case stationBest > 0 && cheaper(stationBest):
		return "undercut_station", stationBest, regionBest
	case stationCount <= 1 && cheaper(regionBest):
		return "undercut_region", stationBest, regionBest
	case cheaper(regionBest):
		return "best_region_cheaper", stationBest, regionBest
	default:
		return "best", stationBest, regionBest
	}
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

// marketFetchKind names one row of market_fetch_state.
func marketFetchKind(prefix string, key marketKey) string {
	return fmt.Sprintf("%s_%d_%d", prefix, key.RegionID, key.TypeID)
}

// marketFetchDue reports whether a fetch kind may run again: a
// missing record or a failed attempt is always due; a successful
// one waits out its gate.
func (app *Application) marketFetchDue(ctx context.Context, kind string, gate time.Duration) bool {
	state, err := app.queries.GetMarketFetchState(ctx, kind)
	if err != nil {
		return true // no record (or unreadable): try
	}
	if state.State != fetchStateOK {
		return true
	}
	attempted, err := time.Parse(time.RFC3339, state.AttemptedAt)
	return err != nil || time.Since(attempted) >= gate
}

// recordMarketFetch upserts one market fetch outcome (best
// effort, like the snapshot fetch-state log).
func (app *Application) recordMarketFetch(ctx context.Context, kind, state, detail string) {
	if err := app.queries.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: kind, State: state, Detail: detail,
		AttemptedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("worker: market fetch-state %s: %v", kind, err)
	}
}
