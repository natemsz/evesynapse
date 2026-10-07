package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Market worker pass (Phase 5, coverage reworked by the proactive-
// warming build): keeps market_history warm for a maintained
// coverage set, warmed before anyone clicks, and computes
// per-order health from regional order books. Everything here is
// public ESI — no character token. It spends from the market
// pass's own fetch lane (maxMarketFetchesPerCycle), never the
// character pass's budget, and a 420/429 stops it until the
// next cycle.
//
// What gets history, in priority order (the first tiers win the
// per-cycle fetch cap on a cold start; everything eventually
// converges and stays fresh):
//   1. item-page "wants" (a user opened a type with no rows yet —
//      someone is staring at that page, the strongest signal),
//   2. watchlist entries (any user's — the alerts read them),
//   3. types with open orders on file (their regions ride along),
//   4. the liquid core: top types by ISK velocity in The Forge,
//      ranked from our own stored history (with a cold-start
//      supplement from recent killmail details),
//   5. the orbit: everything the deployment's characters touch —
//      asset inventories, industry job products and their
//      blueprint products, saved-fitting types, contract item
//      types — kept at The Forge.
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
	// marketFetchErrorGate is the minimum age of a transiently
	// failed market fetch before it retries. Without it a
	// failed attempt is always due and one poisoned pair
	// retries on every urgent tick forever (seen live
	// 2026-10-04: two bad history pairs refetching every 5s).
	marketFetchErrorGate = 10 * time.Minute
	// maxMarketFetchesPerCycle is the market pass's own fetch
	// lane, separate from the character pass budget: public
	// prices and the region sweeps never queue behind
	// per-character warming.
	maxMarketFetchesPerCycle = 120
	// maxBookFetchesPerCycle bounds regional order-book reads
	// per cycle (each may still paginate in fetchOrderBook).
	maxBookFetchesPerCycle = 10
	// bookRefetchGate is the minimum age of a successful book
	// fetch before that (region, type) is re-read for health.
	bookRefetchGate = 10 * time.Minute
	// historyWantMaxAge is how long an unviewed want keeps its
	// place in the fetch queue.
	historyWantMaxAge = 7 * 24 * time.Hour
	// liquidCoreSize bounds the ISK-velocity core kept warm in
	// The Forge.
	liquidCoreSize = 200
	// liquidCoreKillmailScan caps how many recent killmail
	// detail payloads the cold-start core supplement reads.
	liquidCoreKillmailScan = 100
	// maxCoverageTypeDetailNotesPerCycle bounds the type-detail
	// (item description) wants the coverage pass notes per cycle,
	// so descriptions converge for every type history covers.
	maxCoverageTypeDetailNotesPerCycle = 40
	// lifecyclePrunePerCycle bounds how many old closed lifecycle
	// rows one order-health pass may delete (365-day retention).
	lifecyclePrunePerCycle = 500
)

// marketKey is one (region, type) pair to warm.
type marketKey struct {
	RegionID int64
	TypeID   int64
}

// refreshMarketData runs one market pass, reporting how many
// payloads it stored (history downloads + book reads + region
// sweep pages) and whether ESI's error limit stopped it.
func (app *Application) refreshMarketData(ctx context.Context, characters []db.Character) (stored int, limited bool) {
	// The market pass spends from its own lane, not the
	// character pass's budget: the character pass can spend
	// its whole allowance in a busy cycle, and a sweep
	// starved of fetches logs nothing while it waits (the
	// empty Scanner/Tradefinder stall of 2026-10-04).
	allowance := &fetchBudget{left: maxMarketFetchesPerCycle}
	hStored, ltd := app.warmMarketHistory(ctx, allowance)
	stored += hStored
	if ltd {
		return stored, true
	}
	bStored, ltd := app.refreshOrderHealth(ctx, characters, allowance)
	stored += bStored
	if ltd {
		return stored, true
	}
	// P1 region stats: advance the whole-region book sweep with
	// what is left of the cycle's allowance. Additive -- the
	// passes above are untouched.
	sPages, ltd := app.sweepRegionStats(ctx, allowance)
	stored += sPages
	return stored, ltd
}

// warmMarketHistory downloads due history pairs, best-priority
// first, at most maxHistoryFetchesPerCycle of them. The pass runs
// under the shared fetch lock so the urgent drain never
// double-fetches the same pair in the same moment.
func (app *Application) warmMarketHistory(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()

	candidates, err := app.historyCandidates(ctx)
	if err != nil {
		logging.Errorf("worker: market history: collect candidates: %v", err)
		return 0, false
	}
	app.noteCoverageTypeDetails(ctx, candidates)
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
		fetched, limited := app.fetchAndStoreHistory(ctx, key)
		if limited {
			return stored, true
		}
		if fetched {
			stored++
		}
	}
	return stored, false
}

// fetchAndStoreHistory downloads one pair's daily aggregates,
// upserts them, and settles the fetch state. fetched reports a
// successful read (even an empty one — that still settles the
// pair); limited reports ESI's stop signal. Shared by the cycle's
// history pass and the urgent want drain.
func (app *Application) fetchAndStoreHistory(ctx context.Context, key marketKey) (fetched, limited bool) {
	kind := marketFetchKind("history", key)
	rows, err := app.fetchMarketHistory(ctx, key)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: market history: ESI error limit hit fetching %s; backing off", kind)
			return false, true
		}
		logging.Errorf("worker: market history: fetch %s: %v", kind, err)
		app.recordMarketFetch(ctx, kind, marketFetchFailureState(err), err.Error())
		return false, false
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
			logging.Errorf("worker: market history: store %s day %s: %v", kind, row.Date, err)
		}
	}
	app.recordMarketFetch(ctx, kind, fetchStateOK, "")
	return true, false
}

// historyCandidates builds the prioritized fetch queue: viewed-item
// wants first (someone is staring at that page — the strongest
// signal the app gets), then watchlist pairs, then types with open
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

	wants, err := app.queries.ListMarketHistoryWants(ctx,
		time.Now().UTC().Add(-historyWantMaxAge).Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	for _, wn := range wants {
		add(wn.RegionID, wn.TypeID)
	}

	entries, err := app.queries.ListAllWatchlistEntries(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		add(e.RegionID, e.TypeID)
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

	// The liquid core: the types that actually move in The Forge,
	// kept warm whether or not anyone here trades them yet.
	for _, typeID := range app.liquidCoreTypeIDs(ctx) {
		add(defaultMarketRegion, typeID)
	}

	// The orbit: everything the deployment's characters touch,
	// kept warm in The Forge (assets span every region of space;
	// history per-region for every asset location is unbounded).
	orbit := app.orbitForgeTypeIDs(ctx)
	orbitIDs := make([]int64, 0, len(orbit))
	for typeID := range orbit {
		orbitIDs = append(orbitIDs, typeID)
	}
	sort.Slice(orbitIDs, func(i, j int) bool { return orbitIDs[i] < orbitIDs[j] })
	for _, typeID := range orbitIDs {
		add(defaultMarketRegion, typeID)
	}
	return out, nil
}

// liquidCoreTypeIDs ranks The Forge's most liquid types from our
// own stored history: ISK velocity (volume × average price)
// summed over the region's newest seven stored days, capped at
// liquidCoreSize. Before any history exists the ranking is empty,
// so the tail is supplemented from the types in recent killmail
// details — the ships and modules the deployment actually loses —
// until the stored ranking can stand on its own. Recomputed from
// the database on every candidate build, so the core tracks the
// market as history accumulates.
func (app *Application) liquidCoreTypeIDs(ctx context.Context) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	add := func(typeID int64) {
		if typeID > 0 && !seen[typeID] {
			seen[typeID] = true
			out = append(out, typeID)
		}
	}
	rows, err := app.queries.ListLiquidCoreTypes(ctx, db.ListLiquidCoreTypesParams{
		RegionID:  defaultMarketRegion,
		CoreLimit: liquidCoreSize,
	})
	if err != nil {
		logging.Errorf("worker: market history: liquid core ranking: %v", err)
	} else {
		for _, r := range rows {
			add(r.TypeID)
		}
	}
	if len(out) < liquidCoreSize {
		payloads, err := app.queries.ListRecentKillmailDetails(ctx, liquidCoreKillmailScan)
		if err != nil {
			logging.Errorf("worker: market history: killmail core supplement: %v", err)
			return out
		}
		for _, payload := range payloads {
			var km esi.Killmail
			if err := json.Unmarshal([]byte(payload), &km); err != nil {
				continue
			}
			add(km.Victim.ShipTypeID)
			for _, item := range km.Victim.Items {
				add(item.ItemTypeID)
			}
			if len(out) >= liquidCoreSize {
				break
			}
		}
	}
	return out
}

// orbitForgeTypeIDs enumerates every type the deployment's
// characters touch, for Forge history coverage: industry job
// products, the products of owned blueprints, saved-fitting ships
// and modules, asset inventory types, and contract item types.
// Whatever snapshots exist contribute; a missing snapshot simply
// has nothing to say this cycle.
func (app *Application) orbitForgeTypeIDs(ctx context.Context) map[int64]bool {
	out := make(map[int64]bool)
	add := func(typeID int64) {
		if typeID > 0 {
			out[typeID] = true
		}
	}
	bpProduct := make(map[int64]int64)
	if rows, err := app.queries.ListSDEBlueprintProducts(ctx); err != nil {
		logging.Errorf("worker: market history: orbit blueprint products: %v", err)
	} else {
		for _, r := range rows {
			bpProduct[r.BlueprintTypeID] = r.ProductTypeID
		}
	}
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		logging.Errorf("worker: market history: orbit characters: %v", err)
		return out
	}
	for _, ch := range characters {
		if jobs, ok := loadSnapshot[[]esi.IndustryJob](app, ctx, ch.CharacterID, esi.SnapIndustryJobs); ok {
			for _, j := range jobs {
				add(j.ProductTypeID)
			}
		}
		if owned, ok := loadSnapshot[[]esi.Blueprint](app, ctx, ch.CharacterID, esi.SnapBlueprints); ok {
			for _, bp := range owned {
				if product, ok := bpProduct[bp.TypeID]; ok {
					add(product)
				}
			}
		}
		if fits, ok := loadSnapshot[[]esi.Fitting](app, ctx, ch.CharacterID, esi.SnapFittings); ok {
			for _, f := range fits {
				add(f.ShipTypeID)
				for _, item := range f.Items {
					add(item.TypeID)
				}
			}
		}
		if assets, ok := loadSnapshot[[]esi.Asset](app, ctx, ch.CharacterID, esi.SnapAssets); ok {
			for _, a := range assets {
				add(a.TypeID)
			}
		}
		detailIDs, err := app.queries.ListContractDetailIDsByCharacter(ctx, ch.CharacterID)
		if err != nil {
			logging.Errorf("worker: market history: orbit contract ids %d: %v", ch.CharacterID, err)
			continue
		}
		for _, contractID := range detailIDs {
			rec, err := app.queries.GetContractDetail(ctx, contractID)
			if err != nil {
				continue
			}
			var items []esi.ContractItem
			if err := json.Unmarshal([]byte(rec.Payload), &items); err != nil {
				continue
			}
			for _, item := range items {
				add(item.TypeID)
			}
		}
	}
	return out
}

// noteCoverageTypeDetails records description wants for the types
// in this cycle's coverage, so item pages for covered types fill
// in without waiting for a click. Bounded per cycle; every note
// is an insert-or-ignore against rows the type-details drain
// already settles.
func (app *Application) noteCoverageTypeDetails(ctx context.Context, candidates []marketKey) {
	seen := make(map[int64]bool)
	noted := 0
	for _, key := range candidates {
		if noted >= maxCoverageTypeDetailNotesPerCycle {
			break
		}
		if key.TypeID <= 0 || seen[key.TypeID] {
			continue
		}
		seen[key.TypeID] = true
		if err := app.queries.UpsertTypeDetailWant(ctx, key.TypeID); err != nil {
			logging.Errorf("worker: market history: note type detail %d: %v", key.TypeID, err)
			continue
		}
		noted++
	}
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
// The same pass also maintains the P4 order lifecycle ledger
// (schema 033): every open order in the snapshot is upserted,
// station-level beatings are counted, vanished orders are closed,
// and old closed rows are pruned. Lifecycle work rides the
// snapshots already loaded here -- it never fetches ESI itself.
func (app *Application) refreshOrderHealth(ctx context.Context, characters []db.Character, allowance *fetchBudget) (stored int, limited bool) {
	type orderRef struct {
		char  db.Character
		order esi.CharOrder
	}
	groups := make(map[marketKey][]orderRef)
	openByChar := make(map[int64]map[int64]bool)
	allOpenByChar := make(map[int64]map[int64]esi.CharOrder)
	for _, ch := range characters {
		if !characterSyncs(ch) {
			continue // parked characters' orders are stale news, not alerts
		}
		orders, ok := app.loadOrdersSnapshot(ctx, ch.CharacterID)
		if !ok {
			// No orders snapshot yet: drop any stale health so
			// the pages never show yesterday's verdict.
			if err := app.queries.DeleteOrderHealthForCharacter(ctx, ch.CharacterID); err != nil {
				logging.Errorf("worker: order health: clear %d: %v", ch.CharacterID, err)
			}
			continue
		}
		open := make(map[int64]bool)
		allOpen := make(map[int64]esi.CharOrder)
		for _, o := range orders {
			allOpen[o.OrderID] = o
			if o.IsBuyOrder || o.VolumeRemain <= 0 {
				continue
			}
			open[o.OrderID] = true
			k := marketKey{RegionID: o.RegionID, TypeID: o.TypeID}
			groups[k] = append(groups[k], orderRef{char: ch, order: o})
		}
		openByChar[ch.CharacterID] = open
		allOpenByChar[ch.CharacterID] = allOpen
	}

	// P4 lifecycle: upsert every open order the snapshot shows.
	// first_seen_at is set on insert only (the SQL leaves it alone
	// on conflict); listed price and remaining volume follow the
	// newest observation. Previous beaten state is remembered so
	// the book pass below can count transitions, not polls.
	lifecycleNow := time.Now().UTC().Format(time.RFC3339)
	type lifecycleKey struct {
		characterID int64
		orderID     int64
	}
	prevBeaten := make(map[lifecycleKey]int64)
	prevOutbid := make(map[lifecycleKey]int64)
	for characterID, allOpen := range allOpenByChar {
		for orderID, o := range allOpen {
			key := lifecycleKey{characterID: characterID, orderID: orderID}
			if existing, err := app.queries.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: characterID, OrderID: orderID}); err == nil {
				if existing.ClosedAt != "" {
					continue // already closed history; never resurrect
				}
				prevBeaten[key] = existing.BeatenNow
				prevOutbid[key] = existing.OutbidEvents
			} else {
				prevBeaten[key] = 0
				prevOutbid[key] = 0
			}
			isBuy := int64(0)
			if o.IsBuyOrder {
				isBuy = 1
			}
			if err := app.queries.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
				CharacterID:      characterID,
				OrderID:          orderID,
				TypeID:           o.TypeID,
				LocationID:       o.LocationID,
				RegionID:         o.RegionID,
				IsBuyOrder:       isBuy,
				ListedPrice:      o.Price,
				VolumeTotal:      o.VolumeTotal,
				VolumeRemainLast: o.VolumeRemain,
				FirstSeenAt:      lifecycleNow,
				LastSeenAt:       lifecycleNow,
			}); err != nil {
				logging.Errorf("worker: order lifecycle: upsert %d/%d: %v", characterID, orderID, err)
			}
		}
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
				logging.Warnf("worker: order health: ESI error limit hit reading book %s; backing off until next cycle", kind)
				return stored, true
			}
			logging.Errorf("worker: order health: book %s: %v", kind, err)
			app.recordMarketFetch(ctx, kind, marketFetchFailureState(err), err.Error())
			continue
		}
		books++
		stored++
		app.recordMarketFetch(ctx, kind, fetchStateOK, "")
		// Book locations join the structure-name queue: best
		// buy/sell at a player structure is exactly where the
		// "#<id>" fallback hurts most.
		{
			ids := make([]int64, 0, len(sells))
			for _, o := range sells {
				ids = append(ids, o.LocationID)
			}
			app.noteStructureIDs(ctx, ids...)
		}
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
				logging.Errorf("worker: order health: store order %d: %v", ref.order.OrderID, err)
			}
			// P4 beaten tracking: only a station-level undercut
			// counts as beaten -- someone cheaper at the order's
			// own station. Region-only cheapness
			// (undercut_region, best_region_cheaper) does not.
			// Buy orders carry no health verdict, so they are
			// never marked beaten.
			beaten := int64(0)
			if status == "undercut_station" {
				beaten = 1
			}
			lkey := lifecycleKey{characterID: ref.char.CharacterID, orderID: ref.order.OrderID}
			prevB, prevO := prevBeaten[lkey], prevOutbid[lkey]
			outbid := prevO
			if prevB == 0 && beaten == 1 {
				outbid++
			}
			if beaten != prevB || outbid != prevO {
				if err := app.queries.UpdateOrderLifecycleBeaten(ctx, db.UpdateOrderLifecycleBeatenParams{
					BeatenNow:    beaten,
					OutbidEvents: outbid,
					CharacterID:  ref.char.CharacterID,
					OrderID:      ref.order.OrderID,
				}); err != nil {
					logging.Errorf("worker: order lifecycle: beaten %d/%d: %v", ref.char.CharacterID, ref.order.OrderID, err)
				} else {
					prevBeaten[lkey] = beaten
					prevOutbid[lkey] = outbid
				}
			}
		}
	}

	// Prune verdicts for orders no longer open (filled, expired,
	// or cancelled since the last pass).
	for characterID, open := range openByChar {
		rows, err := app.queries.ListOrderHealthByCharacter(ctx, characterID)
		if err != nil {
			logging.Errorf("worker: order health: list for %d: %v", characterID, err)
			continue
		}
		for _, row := range rows {
			if !open[row.OrderID] {
				if err := app.queries.DeleteOrderHealthEntry(ctx, db.DeleteOrderHealthEntryParams{
					CharacterID: characterID, OrderID: row.OrderID,
				}); err != nil {
					logging.Errorf("worker: order health: prune order %d: %v", row.OrderID, err)
				}
			}
		}
	}

	// P4 lifecycle: close rows whose order has left the snapshot.
	// We only observe fills -- an order that vanishes with stock
	// left is 'ended', never labelled cancelled vs expired.
	for characterID, allOpen := range allOpenByChar {
		openRows, err := app.queries.ListOpenOrderLifecycleByCharacter(ctx, characterID)
		if err != nil {
			logging.Errorf("worker: order lifecycle: list open %d: %v", characterID, err)
			continue
		}
		for _, row := range openRows {
			if _, stillOpen := allOpen[row.OrderID]; stillOpen {
				continue
			}
			kind := "ended"
			if row.VolumeRemainLast <= 0 {
				kind = "filled"
			}
			if err := app.queries.CloseOrderLifecycle(ctx, db.CloseOrderLifecycleParams{
				ClosedAt:    lifecycleNow,
				CloseKind:   kind,
				CharacterID: characterID,
				OrderID:     row.OrderID,
			}); err != nil {
				logging.Errorf("worker: order lifecycle: close %d/%d: %v", characterID, row.OrderID, err)
			}
		}
	}

	// Prune closed rows past the 365-day retention, bounded per
	// cycle so a first cleanup never stalls the pass.
	cutoff := time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	if err := app.queries.PruneOldOrderLifecycle(ctx, db.PruneOldOrderLifecycleParams{
		ClosedAt: cutoff,
		RowLimit: lifecyclePrunePerCycle,
	}); err != nil {
		logging.Errorf("worker: order lifecycle: prune: %v", err)
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
// missing record is always due; a successful or definitively
// failed (dead) attempt waits out the caller's gate; a
// transient failure retries after marketFetchErrorGate
// instead of on every pass.
func (app *Application) marketFetchDue(ctx context.Context, kind string, gate time.Duration) bool {
	state, err := app.queries.GetMarketFetchState(ctx, kind)
	if err != nil {
		return true // no record (or unreadable): try
	}
	attempted, err := time.Parse(time.RFC3339, state.AttemptedAt)
	if err != nil {
		return true
	}
	if state.State == fetchStateError {
		return time.Since(attempted) >= marketFetchErrorGate
	}
	return time.Since(attempted) >= gate
}

// marketFetchFailureState classifies a failed market fetch: a
// definitive client error (400/404 -- the type id is wrong, or
// the type simply has no such data in that region) settles as
// dead and waits out the full refetch gate, because retrying
// sooner can only fail the same way; anything else is a
// transient error that retries after the error gate.
func marketFetchFailureState(err error) string {
	if code, ok := esi.StatusCode(err); ok && (code == http.StatusBadRequest || code == http.StatusNotFound) {
		return fetchStateDead
	}
	return fetchStateError
}

// recordMarketFetch upserts one market fetch outcome (best
// effort, like the snapshot fetch-state log).
func (app *Application) recordMarketFetch(ctx context.Context, kind, state, detail string) {
	if err := app.queries.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: kind, State: state, Detail: detail,
		AttemptedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		logging.Errorf("worker: market fetch-state %s: %v", kind, err)
	}
}
