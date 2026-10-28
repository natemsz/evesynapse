package app

import (
	"context"
	"encoding/json"
	"errors"
	"evesynapse/internal/app/module"
	"fmt"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"

	"github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Market worker pass: keeps market_history warm for a maintained
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
	maxHistoryFetchesPerCycle = 200
	// historyRefetchGate is the minimum age of a successful
	// history fetch before the pair is fetched again.
	historyRefetchGate = module.HistoryRefetchGate
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
	maxMarketFetchesPerCycle = 1200
	// maxBookFetchesPerCycle bounds regional order-book reads
	// per cycle (each may still paginate in fetchOrderBook).
	maxBookFetchesPerCycle = 100
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
type marketKey = module.MarketKey

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
	stored, limited = app.refreshMarketWarm(ctx, characters, allowance)
	if limited {
		return stored, true
	}
	// Region stats: advance the whole-region book sweep with
	// what is left of the cycle's allowance. Additive -- the
	// passes above are untouched.
	sPages, ltd := app.sweepRegionStats(ctx, allowance)
	stored += sPages
	return stored, ltd
}

// refreshMarketWarm is the market pass ahead of the region
// sweep: history warming, then per-order health from the
// regional books, both drawing on the pass's shared allowance.
// The cycle times it apart from the sweep (worker.go), so it
// is its own step; refreshMarketData is the two steps run
// together.
func (app *Application) refreshMarketWarm(ctx context.Context, characters []db.Character, allowance *fetchBudget) (stored int, limited bool) {
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
	return stored, false
}

// warmMarketHistory downloads due history pairs, best-priority
// first, at most maxHistoryFetchesPerCycle of them. The pass runs
// under the shared fetch lock so the urgent drain never
// double-fetches the same pair in the same moment.
func (app *Application) warmMarketHistory(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.histFetchMu.Lock()
	defer app.histFetchMu.Unlock()

	candidates, err := app.historyCandidates(ctx)
	if err != nil {
		logging.Errorf("worker: market history: collect candidates: %v", err)
		return 0, false
	}
	app.noteCoverageTypeDetails(ctx, candidates)
	// The candidates' fetch states in one read: there are thousands
	// of them, nearly all inside their gate on any one cycle.
	kinds := make([]string, len(candidates))
	for i, key := range candidates {
		kinds[i] = marketFetchKind("history", key)
	}
	states, statesOK := app.marketFetchStates(ctx, kinds)
	for i, key := range candidates {
		if stored >= maxHistoryFetchesPerCycle || ctx.Err() != nil {
			break
		}
		kind := kinds[i]
		if statesOK {
			if state, ok := states[kind]; ok && !marketFetchStateDue(state, historyRefetchGate, time.Now()) {
				continue
			}
		} else if !app.marketFetchDue(ctx, kind, historyRefetchGate) {
			continue
		}
		// A fresh staged entry means the pull already covered
		// this key and a spot decision is moments away, and a
		// dedicated node that warms history gets its grace
		// first: either way this instance's own call waits.
		if app.deferFetch(ctx, module.FetchKindHistory, fmt.Sprintf("%d/%d", key.RegionID, key.TypeID), time.Now().UTC()) {
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
	if len(rows) > 0 {
		if err := app.queries.UpsertMarketHistoryDays(ctx, historyDaysParams(key, rows)); err != nil {
			logging.Errorf("worker: market history: store %s (%d days): %v", kind, len(rows), err)
		}
	}
	// The region stat's sold-per-day figure follows the new history at
	// once rather than at the region's next sweep, so a route waiting
	// on it (the tradefinder's pending list) ranks in right away.
	if err := app.queries.RefreshMarketRegionStatAvgVolume(ctx, db.RefreshMarketRegionStatAvgVolumeParams{
		RegionID: key.RegionID, TypeID: key.TypeID,
	}); err != nil {
		logging.Errorf("worker: market history: refresh sold-per-day for %s: %v", kind, err)
	}
	app.recordMarketFetch(ctx, kind, fetchStateOK, "")
	return true, false
}

// historyDaysParams is one pair's history payload as the columns of
// one upsert. A day ESI repeats is kept once, its last answer, since
// one statement may not write the same row twice.
func historyDaysParams(key marketKey, rows []esi.MarketHistoryDay) db.UpsertMarketHistoryDaysParams {
	p := db.UpsertMarketHistoryDaysParams{RegionID: key.RegionID, TypeID: key.TypeID}
	at := make(map[string]int, len(rows))
	for _, row := range rows {
		if i, seen := at[row.Date]; seen {
			p.Averages[i], p.Highests[i], p.Lowests[i] = row.Average, row.Highest, row.Lowest
			p.Volumes[i], p.OrderCounts[i] = row.Volume, row.OrderCount
			continue
		}
		at[row.Date] = len(p.Dates)
		p.Dates = append(p.Dates, row.Date)
		p.Averages = append(p.Averages, row.Average)
		p.Highests = append(p.Highests, row.Highest)
		p.Lowests = append(p.Lowests, row.Lowest)
		p.Volumes = append(p.Volumes, row.Volume)
		p.OrderCounts = append(p.OrderCounts, row.OrderCount)
	}
	return p
}

// historyFetchableCacheTTL bounds how long the fetchable-type
// set is reused: the SDE changes only on reimport and the
// sweep stats grow slowly, while the urgent drain reads the
// set on every tick.
const historyFetchableCacheTTL = 10 * time.Minute

// historyFetchableTypes loads the set of types ESI market
// history can know -- the published SDE types plus every type
// with stored sweep stats -- cached briefly on the
// application. It fails open: a nil set (the load failed, or
// nothing is stored yet, as on a fresh instance before the
// SDE import) gates nothing, so a candidate is never lost to
// the gate's own state.
func (app *Application) historyFetchableTypes(ctx context.Context) map[int64]bool {
	app.historyTypesMu.Lock()
	if app.historyTypes != nil && time.Since(app.historyTypesAt) < historyFetchableCacheTTL {
		set := app.historyTypes
		app.historyTypesMu.Unlock()
		return set
	}
	app.historyTypesMu.Unlock()
	rows, err := app.queries.ListHistoryFetchableTypeIDs(ctx)
	if err != nil {
		logging.Errorf("worker: market history: fetchable types: %v", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	set := make(map[int64]bool, len(rows))
	for _, id := range rows {
		set[id] = true
	}
	app.historyTypesMu.Lock()
	app.historyTypes, app.historyTypesAt = set, time.Now()
	app.historyTypesMu.Unlock()
	return set
}

// historyCandidates builds the prioritized fetch queue: viewed-item
// wants first (someone is staring at that page — the strongest
// signal the app gets), then watchlist pairs, then types with open
// orders (the region each order sits in). A type outside the
// fetchable set (historyFetchableTypes) is never queued: ESI
// does not know the type at all, so its one fetch could only
// ever be answered 404, however the pair was minted -- a
// killmail item, an asset, a contract line naming a type with
// no market existence.
func (app *Application) historyCandidates(ctx context.Context) ([]marketKey, error) {
	fetchable := app.historyFetchableTypes(ctx)
	seen := make(map[marketKey]bool)
	var out []marketKey
	add := func(regionID, typeID int64) {
		if regionID <= 0 || typeID <= 0 {
			return
		}
		if fetchable != nil && !fetchable[typeID] {
			return
		}
		k := marketKey{RegionID: regionID, TypeID: typeID}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}

	now := time.Now().UTC()
	wants, err := app.queries.ListMarketHistoryWants(ctx, db.ListMarketHistoryWantsParams{
		LastRequestedAt: now.Add(-historyWantMaxAge),
		TypingPriority:  wantTyping,
		TypingAssumed:   typingAssumedHit,
		TypingWeight:    typingAssumedWeight,
		TypingSince:     now.Add(-typingLearnWindow),
		TypingLimit:     int64(app.typingSlice()),
	})
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
		add(marketRegionForType(e.TypeID, e.RegionID), e.TypeID)
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
// until the stored ranking can stand on its own.
//
// The ranking scans the region's whole stored history (seconds,
// and a gigabyte through the buffer cache, on a busy instance),
// while its input moves once a day when ESI publishes a new day.
// A full ranking is reused for liquidCoreTTL; a short one (the
// cold start, still leaning on killmails) is recomputed on every
// candidate build so the core fills in as history accumulates.
func (app *Application) liquidCoreTypeIDs(ctx context.Context) []int64 {
	app.liquidCoreMu.Lock()
	defer app.liquidCoreMu.Unlock()
	if app.liquidCore != nil && time.Since(app.liquidCoreAt) < liquidCoreTTL {
		return app.liquidCore
	}
	out, full := app.rankLiquidCore(ctx)
	if full {
		app.liquidCore, app.liquidCoreAt = out, time.Now()
	}
	return out
}

// liquidCoreTTL is how long a full liquid-core ranking is reused.
const liquidCoreTTL = time.Hour

// rankLiquidCore computes the liquid core; full reports that the
// stored ranking alone filled it.
func (app *Application) rankLiquidCore(ctx context.Context) (out []int64, full bool) {
	seen := make(map[int64]bool)
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
	if len(out) >= liquidCoreSize {
		return out, true
	}
	payloads, err := app.queries.ListRecentKillmailDetails(ctx, liquidCoreKillmailScan)
	if err != nil {
		logging.Errorf("worker: market history: killmail core supplement: %v", err)
		return out, false
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
	return out, false
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
		if err := app.queries.UpsertTypeDetailWant(ctx, db.UpsertTypeDetailWantParams{
			TypeID: key.TypeID, Priority: wantViewed, NotedAt: timeSet(time.Now().UTC()),
		}); err != nil {
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
// The same pass also maintains the order lifecycle ledger
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
		if !app.characterMaySync(ctx, ch) {
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

	// Lifecycle: upsert every open order the snapshot shows.
	// first_seen_at is set on insert only (the SQL leaves it alone
	// on conflict); listed price and remaining volume follow the
	// newest observation. Previous beaten state is remembered so
	// the book pass below can count transitions, not polls.
	lifecycleNow := time.Now().UTC()
	type lifecycleKey struct {
		characterID int64
		orderID     int64
	}
	prevBeaten := make(map[lifecycleKey]int64)
	prevOutbid := make(map[lifecycleKey]int64)
	// What the ledger already holds for these orders, in one read; an
	// order with no row (or an unreadable ledger) starts from zero.
	var lookup db.ListOrderLifecycleStatesParams
	for characterID, allOpen := range allOpenByChar {
		for orderID := range allOpen {
			lookup.CharacterIds = append(lookup.CharacterIds, characterID)
			lookup.OrderIds = append(lookup.OrderIds, orderID)
		}
	}
	closed := make(map[lifecycleKey]bool)
	if len(lookup.OrderIds) > 0 {
		rows, err := app.queries.ListOrderLifecycleStates(ctx, lookup)
		if err != nil {
			logging.Errorf("worker: order lifecycle: read ledger: %v", err)
		}
		for _, row := range rows {
			key := lifecycleKey{characterID: row.CharacterID, orderID: row.OrderID}
			if row.ClosedAt.Valid {
				closed[key] = true
				continue
			}
			prevBeaten[key] = row.BeatenNow
			prevOutbid[key] = row.OutbidEvents
		}
	}
	upsert := db.UpsertOrderLifecyclesParams{SeenAt: lifecycleNow}
	for characterID, allOpen := range allOpenByChar {
		for orderID, o := range allOpen {
			if closed[lifecycleKey{characterID: characterID, orderID: orderID}] {
				continue // already closed history; never resurrect
			}
			isBuy := int64(0)
			if o.IsBuyOrder {
				isBuy = 1
			}
			upsert.CharacterIds = append(upsert.CharacterIds, characterID)
			upsert.OrderIds = append(upsert.OrderIds, orderID)
			upsert.TypeIds = append(upsert.TypeIds, o.TypeID)
			upsert.LocationIds = append(upsert.LocationIds, o.LocationID)
			upsert.RegionIds = append(upsert.RegionIds, o.RegionID)
			upsert.IsBuyOrders = append(upsert.IsBuyOrders, isBuy)
			upsert.ListedPrices = append(upsert.ListedPrices, o.Price)
			upsert.VolumeTotals = append(upsert.VolumeTotals, o.VolumeTotal)
			upsert.VolumeRemains = append(upsert.VolumeRemains, o.VolumeRemain)
		}
	}
	if len(upsert.OrderIds) > 0 {
		if err := app.queries.UpsertOrderLifecycles(ctx, upsert); err != nil {
			logging.Errorf("worker: order lifecycle: upsert %d orders: %v", len(upsert.OrderIds), err)
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
		sells, _, _, err := app.fetchOrderBookFresh(ctx, key, key.RegionID, key.TypeID)
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
		now := time.Now().UTC()
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
			// Beaten tracking: only a station-level undercut
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

	// Lifecycle: close rows whose order has left the snapshot.
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
	if err := app.queries.PruneOldOrderLifecycle(ctx, db.PruneOldOrderLifecycleParams{
		ClosedBefore: time.Now().UTC().Add(-365 * 24 * time.Hour),
		RowLimit:     lifecyclePrunePerCycle,
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

// marketFetchKind names one row of market_fetch_state.
func marketFetchKind(prefix string, key marketKey) string { return module.MarketFetchKind(prefix, key) }

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
	return marketFetchStateDue(state, gate, time.Now())
}

// marketFetchStateDue is marketFetchDue's rule for a record already
// read.
func marketFetchStateDue(state db.MarketFetchState, gate time.Duration, now time.Time) bool {
	if state.State == fetchStateError {
		return now.Sub(state.AttemptedAt) >= marketFetchErrorGate
	}
	return now.Sub(state.AttemptedAt) >= gate
}

// marketFetchStates reads the records of many fetch kinds at once,
// keyed by kind; a kind with no record is absent, which is due. ok
// is false when the read failed, and the caller then asks
// marketFetchDue kind by kind.
func (app *Application) marketFetchStates(ctx context.Context, kinds []string) (states map[string]db.MarketFetchState, ok bool) {
	rows, err := app.queries.ListMarketFetchStatesByKinds(ctx, kinds)
	if err != nil {
		logging.Errorf("worker: market fetch-states: %v", err)
		return nil, false
	}
	states = make(map[string]db.MarketFetchState, len(rows))
	for _, row := range rows {
		states[row.Kind] = row
	}
	return states, true
}

// marketFetchFailureState classifies a failed market fetch: a
// definitive client error settles as dead and waits out the full
// refetch gate, because retrying sooner can only fail the same way;
// anything else is a transient error that retries after the error
// gate. Definitive means 400 or 404 (the id is wrong, or the type
// has no such data in that region), 403 (every caller is a public
// route, where a refusal cannot be a missing role or scope: ESI
// says "not public", which never changes back) and 422 (ESI's
// "invalid id"). ESI's limits are 420 and 429, never these.
func marketFetchFailureState(err error) string {
	if code, ok := esi.StatusCode(err); ok {
		switch code {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
			return fetchStateDead
		}
	}
	return fetchStateError
}

// recordMarketFetch upserts one market fetch outcome (best
// effort, like the snapshot fetch-state log).
func (app *Application) recordMarketFetch(ctx context.Context, kind, state, detail string) {
	app.recordMarketFetchAt(ctx, kind, state, detail, time.Now().UTC())
}

// recordMarketFetchAt is recordMarketFetch with the attempt time
// given, for callers that must know the exact stamp they wrote.
func (app *Application) recordMarketFetchAt(ctx context.Context, kind, state, detail string, at time.Time) {
	if err := app.queries.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: kind, State: state, Detail: detail,
		AttemptedAt: at,
	}); err != nil {
		logging.Errorf("worker: market fetch-state %s: %v", kind, err)
	}
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
	if meta, ok := app.guideMeta(ctx); ok && now.Before(meta.CachedUntil) {
		return false, false // still inside ESI's cache window
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
	prices := make(map[int64]esi.MarketPrice, len(rows))
	var validRows []esi.MarketPrice
	for _, row := range rows {
		if row.TypeID <= 0 {
			continue
		}
		if _, seen := prices[row.TypeID]; !seen {
			validRows = append(validRows, row)
		}
		prices[row.TypeID] = row
	}
	for start := 0; start < len(validRows); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(validRows) {
			end = len(validRows)
		}
		chunk := validRows[start:end]
		n := len(chunk)
		typeIDs := make([]int64, n)
		adjPrices := make([]float64, n)
		avgPrices := make([]float64, n)
		for i, row := range chunk {
			typeIDs[i] = row.TypeID
			adjPrices[i] = row.AdjustedPrice
			avgPrices[i] = row.AveragePrice
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO guide_prices (type_id, adjusted_price, average_price)
			SELECT unnest($1::bigint[]), unnest($2::float8[]), unnest($3::float8[])
			ON CONFLICT (type_id) DO UPDATE SET
				adjusted_price = excluded.adjusted_price,
				average_price  = excluded.average_price
		`, pq.Array(typeIDs), pq.Array(adjPrices), pq.Array(avgPrices)); err != nil {
			logging.Errorf("worker: guide prices: store batch: %v", err)
			return false, false
		}
	}
	if err := qtx.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt:   now.UTC(),
		CachedUntil: cachedUntil.UTC(),
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
	app.pricesMu.Lock()
	app.prices = prices
	app.pricesExpiry = cachedUntil
	app.pricesMu.Unlock()

	logging.Infof("worker: guide prices stored (%d types, fresh until %s)",
		len(rows), cachedUntil.UTC().Format(time.RFC3339))
	// Prices moved: the stored fit prices follow them.
	app.repriceFittings(ctx)
	return true, false
}

// repriceFittings brings every stored fit's total_price up to the
// house price sources (swept typicals, then estimated sells) and
// reports how many changed. A fit is priced when it is saved;
// without this it kept that price for good, and the public
// browser sorted and filtered on it.
func (app *Application) repriceFittings(ctx context.Context) int64 {
	n, err := app.queries.RepriceLocalFittings(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("worker: reprice fits: %v", err)
		}
		return 0
	}
	if n > 0 {
		logging.Infof("worker: repriced %d stored fits from stored prices", n)
	}
	return n
}

// industryCostIndexFresh is the refresh gate when ESI's Expires
// is missing: rows written within the hour count as fresh.
const industryCostIndexFresh = time.Hour

// costIndexSystem is one row of GET /industry/systems/: the
// system's cost indices per ESI activity name.
type costIndexSystem struct {
	SolarSystemID int64 `json:"solar_system_id"`
	CostIndices   []struct {
		Activity  string  `json:"activity"`
		CostIndex float64 `json:"cost_index"`
	} `json:"cost_indices"`
}

// refreshCostIndices mirrors GET /industry/systems/ into the
// industry_cost_indices table. One public call; a fresh table
// (by row stamp, an hour's cadence) is never refetched early,
// and ESI's Expires header only ever extends that rest. The
// planner reads manufacturing fees off the stored rows, so an
// empty table simply leaves fees unknown. Reports whether it
// stored, and whether ESI's error limit stopped it.
func (app *Application) refreshCostIndices(ctx context.Context) (stored bool, limited bool) {
	now := time.Now()
	if stamp, err := app.queries.GetIndustryCostIndicesStamp(ctx); err == nil {
		if t, ok := stamp.Stamp.(time.Time); ok && stamp.RowCount > 0 && now.Before(t.Add(industryCostIndexFresh)) {
			return false, false // still inside the cadence window
		}
	} else {
		logging.Errorf("worker: cost indices stamp: %v", err)
	}

	body, header, err := app.esi.FetchRaw(ctx, "", "/industry/systems/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return false, true
		}
		if ctx.Err() == nil {
			logging.Errorf("worker: cost indices: %v", err)
		}
		return false, false
	}
	var systems []costIndexSystem
	if err := json.Unmarshal(body, &systems); err != nil {
		logging.Errorf("worker: cost indices: decode: %v", err)
		return false, false
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: cost indices: begin tx: %v", err)
		return false, false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	storedRows := 0
	for _, sys := range systems {
		if sys.SolarSystemID <= 0 {
			continue
		}
		for _, ci := range sys.CostIndices {
			if ci.Activity == "" || ci.CostIndex < 0 {
				continue
			}
			if err := qtx.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
				SolarSystemID: sys.SolarSystemID,
				Activity:      ci.Activity,
				CostIndex:     ci.CostIndex,
			}); err != nil {
				logging.Errorf("worker: cost indices: store system %d: %v", sys.SolarSystemID, err)
				return false, false
			}
			storedRows++
		}
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: cost indices: commit: %v", err)
		return false, false
	}

	cachedUntil := now.Add(industryCostIndexFresh)
	if exp := header.Get("Expires"); exp != "" {
		if t, perr := http.ParseTime(exp); perr == nil && t.After(now) {
			cachedUntil = t
		}
	}
	logging.Infof("worker: cost indices stored (%d rows, fresh until %s)",
		storedRows, cachedUntil.UTC().Format(time.RFC3339))
	return true, false
}
