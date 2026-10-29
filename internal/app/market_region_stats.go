package app

// Region stats platform: the worker
// periodically reads each hub region's WHOLE order book and stores
// per-type statistics -- best and typical (median) prices and the
// 9-in-10 bands on both sides, order counts, remaining volumes --
// in market_region_stats (schema 031). Everything later market
// phases read starts here, and pages only ever SELECT these rows;
// aggregation happens at sweep time, never at render time.
//
// That row is the stat pack (schema 101): the full order-book
// statistics set per side (summarizeSide) and the trend figures
// derived from the daily copy (RefreshMarketStatPackTrends) ride
// on it, so the scanner, item pages, baskets, the LP store,
// alerts and trade ideas all read one canonical row per
// (region, type) instead of deriving their own.
//
// The sweep is incremental and disk-staged (schema 034): every
// hub region may sweep at once, each advancing at most
// maxRegionSweepPagesPerCycle pages per worker cycle. Every
// fetched page lands in market_sweep_orders in the same
// transaction that advances the region's resume cursor
// (market_sweep_state), so a process restart resumes a sweep at
// its next page instead of page 1, and no book is ever held in
// memory. When a book is fully staged it is distilled into the
// stored stats and the staging is deleted in the same
// transaction, so readers never see a half-written region. A
// completed region re-sweeps once its data is regionSweepGate
// old (tracked in market_fetch_state under sweep_<region_id>,
// the same fetched-state bookkeeping the other market passes
// use); an in-progress sweep resumes regardless of that gate.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"

	"github.com/lib/pq"
)

const sweepBatchSize = 2000

const (
	// regionSweepGate is how stale a region's stored stats may
	// get before the worker sweeps that region again.
	regionSweepGate = 30 * time.Minute
	// maxRegionSweepPagesPerCycle bounds how many whole-book
	// pages one worker cycle may advance EACH region's sweep
	// by -- the whole-book cousin of maxBookFetchesPerCycle. At
	// the one-minute cycle cadence even The Forge's book (~a
	// few hundred pages) finishes inside a few minutes per
	// region, and no single cycle bursts hundreds of requests
	// at ESI.
	maxRegionSweepPagesPerCycle = 500
	// maxRegionSweepPagesTotal is a safety stop far beyond any
	// real book: if a region ever reported more pages than
	// this, the sweep stores what it has rather than page
	// forever.
	maxRegionSweepPagesTotal = 2000
)

// regionSweepKind names a region's sweep row in
// market_fetch_state.
func regionSweepKind(regionID int64) string {
	return fmt.Sprintf("sweep_%d", regionID)
}

// sweptRegions is the fixed set of regions the region-stats
// sweep covers: the five hub regions, plus ESI's global
// market region (globalMarketRegion), whose book no hub
// region carries -- PLEX trades there, essentially alone.
// A permanent fixture of the worker, never heat- or
// presence-driven: the global book is one type deep, so
// sweeping it costs a page or two per cycle. It is NOT part
// of marketRegions: the global market is not a hub a page
// offers as a place, only a book the sweep reads.
var sweptRegions = func() []marketRegionOption {
	out := make([]marketRegionOption, 0, len(marketRegions)+1)
	out = append(out, marketRegions...)
	out = append(out, marketRegionOption{ID: globalMarketRegion, Name: "Global PLEX market"})
	return out
}()

// stationKey identifies one (place, type) pair during a sweep.
type stationKey struct {
	LocationID int64
	TypeID     int64
}

// stationAccum gathers one place-and-type's book during a sweep:
// the best and typical (median) prices on each side, the order
// counts, and the remaining volumes. The bests, counts and
// volumes come from the staged book's SQL aggregates; the
// typicals are distilled from the per-type price lists the
// region pass already walks (see storeRegionSweep), so no
// second pass over the book is needed.
type stationAccum struct {
	BestSell    float64 // lowest sell price seen; 0 = none yet
	BestBuy     float64 // highest buy price seen; 0 = none yet
	TypicalSell float64 // median sell price; 0 = no sell orders
	TypicalBuy  float64 // median buy price; 0 = no buy orders
	SellOrders  int64
	BuyOrders   int64
	SellVolume  int64
	BuyVolume   int64
}

// regionSweepOutcome is one region's advance in a sweep pass:
// how many pages it read and whether ESI's error limit stopped
// it.
type regionSweepOutcome struct {
	pages   int
	limited bool
}

// sweepRegionStats is the pass, called from refreshMarketData
// after the existing history and order-health passes; it spends
// what is left of the cycle's allowance. Every swept region with a
// sweep in progress resumes it (whatever the freshness gate
// says), and every region whose stored stats went stale starts
// one; all of them advance in parallel, each bounded to
// maxRegionSweepPagesPerCycle pages and all drawing on the one
// shared cycle allowance, so ESI's error-limit backoff stays
// global. It returns how many pages it read, and whether ESI
// cut the cycle short.
func (app *Application) sweepRegionStats(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.sweepMu.Lock()
	defer app.sweepMu.Unlock()

	var limitHit atomic.Bool
	outcomes := make([]regionSweepOutcome, len(sweptRegions))
	var wg sync.WaitGroup
	for i, region := range sweptRegions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverWorkerPanic("region sweep")
			outcomes[i] = app.advanceRegionSweep(ctx, region.ID, allowance, &limitHit)
		}()
	}
	wg.Wait()
	for _, outcome := range outcomes {
		stored += outcome.pages
		if outcome.limited {
			limited = true
		}
	}
	return stored, limited
}

// advanceRegionSweep moves one region's sweep forward under its
// staged cursor. A region with no state row has no sweep under
// way: one starts only when the stored stats are due
// (regionSweepGate), and thereafter the state row's presence is
// what the sweep resumes from -- the gate is not consulted again
// until the sweep completes and the row is deleted.
//
// Fetches stop at the shared error-limit signal, the cycle
// allowance, or the per-region page budget, whichever comes
// first; whatever was staged stays staged for the next cycle.
func (app *Application) advanceRegionSweep(ctx context.Context, regionID int64, allowance *fetchBudget, limitHit *atomic.Bool) regionSweepOutcome {
	var out regionSweepOutcome
	state, err := app.queries.GetMarketSweepState(ctx, regionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !app.marketFetchDue(ctx, regionSweepKind(regionID), regionSweepGate) {
			return out
		}
		now := time.Now().UTC()
		if err := app.queries.InsertMarketSweepState(ctx, db.InsertMarketSweepStateParams{
			RegionID: regionID, StartedAt: now, UpdatedAt: now,
		}); err != nil {
			logging.Errorf("worker: region sweep: start %s: %v", marketRegionLabel(regionID), err)
			return out
		}
		state = db.MarketSweepState{RegionID: regionID, NextPage: 1, StartedAt: now, UpdatedAt: now}
		logging.Infof("worker: region sweep: starting %s (%d)", marketRegionLabel(regionID), regionID)
	case err != nil:
		logging.Errorf("worker: region sweep: read %s sweep state: %v", marketRegionLabel(regionID), err)
		return out
	}

	// The whole book is already staged but its store never
	// landed (crash, or the store failed): finish it before
	// fetching anything new.
	if sweepBookStaged(state) || state.NextPage > maxRegionSweepPagesTotal {
		app.completeRegionSweep(ctx, state)
		return out
	}

pageLoop:
	for out.pages < maxRegionSweepPagesPerCycle {
		if ctx.Err() != nil || limitHit.Load() {
			break
		}
		// The next few pages are read at once and staged in page
		// order, so a book of hundreds of pages is not hundreds of
		// round trips one after another.
		pages := make([]sweepPage, 0, sweepPageWindow)
		for n := 0; n < sweepPageCount(state, maxRegionSweepPagesPerCycle-out.pages) && allowance.take(); n++ {
			pages = append(pages, sweepPage{page: int(state.NextPage) + n})
		}
		if len(pages) == 0 {
			break
		}
		var wg sync.WaitGroup
		for i := range pages {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer recoverWorkerPanic("region sweep page")
				p := &pages[i]
				p.err = errSweepPageLost
				p.orders, p.totalPages, p.err = app.fetchRegionBookPage(ctx, regionID, p.page)
			}()
		}
		wg.Wait()
		for _, p := range pages {
			if p.err != nil {
				if errors.Is(p.err, esi.ErrErrorLimit) {
					limitHit.Store(true)
					logging.Warnf("worker: region sweep: %s cut short by ESI error limit on page %d", marketRegionLabel(regionID), p.page)
					out.limited = true
					return out
				}
				// Transient failure: the pages staged before it
				// stay staged, the cursor stops at it, and it is
				// retried next cycle; nothing after it is stored.
				logging.Warnf("worker: region sweep: %s page %d unavailable: %v", marketRegionLabel(regionID), p.page, p.err)
				break pageLoop
			}
			if int64(p.totalPages) > state.PagesTotal {
				state.PagesTotal = int64(p.totalPages)
			}
			state.NextPage++
			if err := app.stageSweepPage(ctx, state, p.orders); err != nil {
				logging.Errorf("worker: region sweep: stage %s page %d: %v", marketRegionLabel(regionID), p.page, err)
				break pageLoop
			}
			out.pages++
			if sweepBookStaged(state) {
				break pageLoop
			}
			if state.NextPage > maxRegionSweepPagesTotal {
				logging.Warnf("worker: region sweep: %s passed the %d-page safety stop; storing what was read", marketRegionLabel(regionID), maxRegionSweepPagesTotal)
				break pageLoop
			}
		}
	}
	if sweepBookStaged(state) || state.NextPage > maxRegionSweepPagesTotal {
		app.completeRegionSweep(ctx, state)
	}
	return out
}

// sweepPageWindow is how many of one region's book pages are read
// at once. Six regions sweep side by side, so a busy cycle has a few
// dozen requests in flight; ESI's rate budgets, seen coming in the
// client (esi/ratelimit.go), still refuse any that would overdraw.
const sweepPageWindow = 6

// sweepPage is one book page read in a window: the page asked for
// and its answer.
type sweepPage struct {
	page       int
	orders     []esi.MarketOrder
	totalPages int
	err        error
}

// errSweepPageLost stands for a page whose read panicked, so it is
// treated as unavailable rather than staged empty.
var errSweepPageLost = errors.New("page read did not finish")

// sweepPageCount is how many pages the next window asks for: one
// while the book's size is unknown (page 1 tells it), then up to
// sweepPageWindow, never past the book's end, the safety stop, or
// the left pages of this cycle's per-region budget.
func sweepPageCount(state db.MarketSweepState, left int) int {
	if state.PagesTotal == 0 {
		return min(1, left)
	}
	n := min(sweepPageWindow, left, int(state.PagesTotal-state.NextPage+1), int(maxRegionSweepPagesTotal-state.NextPage+1))
	return max(n, 1)
}

// sweepBookStaged reports whether the staged rows cover the
// region's whole book: page 1 reports the book's size, and once
// the cursor has moved past it every page has landed.
func sweepBookStaged(state db.MarketSweepState) bool {
	return state.PagesTotal > 0 && state.NextPage > state.PagesTotal
}

// stageSweepPage lands one fetched page on disk: the page's
// orders and the advanced resume cursor commit in ONE
// transaction, so the staged book always holds exactly the
// pages the cursor says it does. Whole orders are never kept in
// memory beyond the page being written.
func (app *Application) stageSweepPage(ctx context.Context, state db.MarketSweepState, orders []esi.MarketOrder) error {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	for start := 0; start < len(orders); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(orders) {
			end = len(orders)
		}
		chunk := orders[start:end]
		typeIDs := make([]int64, len(chunk))
		isBuys := make([]int64, len(chunk))
		prices := make([]float64, len(chunk))
		vols := make([]int64, len(chunk))
		locIDs := make([]int64, len(chunk))
		sysIDs := make([]int64, len(chunk))
		for i := range chunk {
			o := &chunk[i]
			typeIDs[i] = o.TypeID
			if o.IsBuyOrder {
				isBuys[i] = 1
			}
			prices[i] = o.Price
			vols[i] = o.VolumeRemain
			locIDs[i] = o.LocationID
			sysIDs[i] = o.SystemID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_sweep_orders (region_id, type_id, is_buy_order, price, volume_remain, location_id, system_id)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::float8[]), unnest($5::bigint[]), unnest($6::bigint[]), unnest($7::bigint[])
		`, state.RegionID, pq.Array(typeIDs), pq.Array(isBuys), pq.Array(prices), pq.Array(vols), pq.Array(locIDs), pq.Array(sysIDs)); err != nil {
			return err
		}
	}
	if err := qtx.UpdateMarketSweepState(ctx, db.UpdateMarketSweepStateParams{
		NextPage: state.NextPage, PagesTotal: state.PagesTotal,
		UpdatedAt: time.Now().UTC(), RegionID: state.RegionID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// completeRegionSweep distills one region's fully staged book
// into the stored stats. A failed store leaves the staging and
// the cursor untouched, so the next cycle finishes the same
// sweep without re-reading a page.
func (app *Application) completeRegionSweep(ctx context.Context, state db.MarketSweepState) {
	types, stations, err := app.storeRegionSweep(ctx, state.RegionID)
	if err != nil {
		logging.Errorf("worker: region sweep: store %s: %v", marketRegionLabel(state.RegionID), err)
		return
	}
	logging.Infof("worker: region sweep: %s done: %d types, %d station rows over %d pages", marketRegionLabel(state.RegionID), types, stations, state.NextPage-1)
}

// fetchRegionBookPage reads one page of a region's whole book
// (no type filter) and reports the total page count from the
// X-Pages header. Auth-free public data, like the per-type book
// fetches.
func (app *Application) fetchRegionBookPage(ctx context.Context, regionID int64, page int) ([]esi.MarketOrder, int, error) {
	path := fmt.Sprintf("/markets/%d/orders/?order_type=all&page=%d", regionID, page)
	body, header, err := app.esi.FetchRaw(ctx, "", path)
	if err != nil {
		return nil, 0, err
	}
	totalPages := 1
	if raw := header.Get("X-Pages"); raw != "" {
		if n, perr := strconv.Atoi(raw); perr == nil && n > 0 {
			totalPages = n
		}
	}
	var orders []esi.MarketOrder
	if err := json.Unmarshal(body, &orders); err != nil {
		return nil, 0, fmt.Errorf("region book page: decode: %w", err)
	}
	return orders, totalPages, nil
}

// storeRegionSweep distills a completed sweep from its staged
// book: the region's rows are replaced wholesale inside one
// transaction (a type that left the book loses its row), the
// top orders per side are kept as the stored book the Market
// page serves (market_book_orders, schema 050), the
// station-grain rows for the spread scanner are replaced in the
// same transaction, today's daily snapshot rows are upserted
// alongside, the sweep's staging rows and cursor are deleted,
// and the sweep's fetch-state row is marked ok so the region
// waits out regionSweepGate before the next sweep. Per-type
// figures reuse summarizePrices over the staged price lists --
// the exact median / 9-in-10 math the item page quotes live.
// The same completion also stores each type's 7-day average
// daily traded volume (from market_history) on its region row,
// so the scanner and tradefinder read a stored figure instead
// of recomputing it per render.
func (app *Application) storeRegionSweep(ctx context.Context, regionID int64) (typeCount, stationCount int, err error) {
	now := time.Now().UTC()
	day := now.Format("2006-01-02")

	// Stream all staged orders for the region ordered by type_id,
	// is_buy_order, price so each type's book is summarized in one
	// single sequential read instead of 15,000 per-type queries.
	var typeIDs []int64
	type regionTypeStats struct {
		sell, buy             sideStats
		sellVolume, buyVolume int64
	}
	// bookRow is one stored-book row: the top orders per side the
	// sweep keeps for the Market page (schema 050), in display
	// order (sells price-ascending, buys price-descending).
	type bookRow struct {
		isBuy                int64
		rank                 int64
		price                float64
		volumeRemain         int64
		locationID, systemID int64
	}
	bookByType := make(map[int64][]bookRow)
	statsByType := make(map[int64]*regionTypeStats)
	type stationSidePrices struct {
		sells, buys []float64
	}
	stationPrices := make(map[stationKey]*stationSidePrices)

	orderRows, err := app.db.QueryContext(ctx, `
		SELECT type_id, is_buy_order, price, volume_remain, location_id, system_id
		FROM market_sweep_orders
		WHERE region_id = $1
		ORDER BY type_id, is_buy_order, price
	`, regionID)
	if err != nil {
		return 0, 0, err
	}
	defer orderRows.Close()

	var (
		curTypeID             int64
		hasCurType            bool
		curSt                 *regionTypeStats
		sellPrices, buyPrices []float64
		sells, buys           []bookRow
	)
	flushType := func() {
		if !hasCurType {
			return
		}
		typeIDs = append(typeIDs, curTypeID)
		var top []bookRow
		for i := 0; i < len(sells) && i < topMarketOrders; i++ {
			sells[i].rank = int64(i)
			top = append(top, sells[i])
		}
		for i := 0; i < len(buys) && i < topMarketOrders; i++ {
			b := buys[len(buys)-1-i]
			b.rank = int64(i)
			top = append(top, b)
		}
		bookByType[curTypeID] = top
		curSt.sell = summarizeSide(sellPrices, false) // sorts in place
		curSt.buy = summarizeSide(buyPrices, true)    // sorts in place
		statsByType[curTypeID] = curSt
	}

	for orderRows.Next() {
		var typeID int64
		var row db.ListMarketSweepTypeOrdersRow
		if err := orderRows.Scan(&typeID, &row.IsBuyOrder, &row.Price, &row.VolumeRemain, &row.LocationID, &row.SystemID); err != nil {
			return 0, 0, err
		}
		if !hasCurType || typeID != curTypeID {
			flushType()
			curTypeID = typeID
			hasCurType = true
			curSt = &regionTypeStats{}
			sellPrices = sellPrices[:0]
			buyPrices = buyPrices[:0]
			sells = sells[:0]
			buys = buys[:0]
		}
		br := bookRow{
			isBuy: row.IsBuyOrder, price: row.Price, volumeRemain: row.VolumeRemain,
			locationID: row.LocationID, systemID: row.SystemID,
		}
		if row.IsBuyOrder == 1 {
			buyPrices = append(buyPrices, row.Price)
			curSt.buyVolume += row.VolumeRemain
			buys = append(buys, br)
		} else {
			sellPrices = append(sellPrices, row.Price)
			curSt.sellVolume += row.VolumeRemain
			sells = append(sells, br)
		}
		if row.LocationID > 0 {
			key := stationKey{LocationID: row.LocationID, TypeID: typeID}
			sp := stationPrices[key]
			if sp == nil {
				sp = &stationSidePrices{}
				stationPrices[key] = sp
			}
			if row.IsBuyOrder == 1 {
				sp.buys = append(sp.buys, row.Price)
			} else {
				sp.sells = append(sp.sells, row.Price)
			}
		}
	}
	if err := orderRows.Close(); err != nil {
		return 0, 0, err
	}
	if err := orderRows.Err(); err != nil {
		return 0, 0, err
	}
	flushType()

	// Station grain (P2): the staged book aggregated per place
	// and type, so the spread scanner never re-reads a book. The
	// aggregates carry the bests, counts and volumes; the
	// typicals distilled in the per-type pass above join them
	// here, so the scanner can price a station's book the way
	// the region's is priced -- median against median.
	stationRows, err := app.queries.ListMarketSweepStationAggregates(ctx, regionID)
	if err != nil {
		return 0, 0, err
	}
	stations := make(map[stationKey]*stationAccum)
	for _, row := range stationRows {
		key := stationKey{LocationID: row.LocationID, TypeID: row.TypeID}
		acc := stations[key]
		if acc == nil {
			acc = &stationAccum{}
			stations[key] = acc
		}
		if row.IsBuyOrder == 1 {
			acc.BestBuy = row.MaxPrice
			acc.BuyOrders = row.OrderCount
			acc.BuyVolume = row.TotalVolume
		} else {
			acc.BestSell = row.MinPrice
			acc.SellOrders = row.OrderCount
			acc.SellVolume = row.TotalVolume
		}
	}
	for key, sp := range stationPrices {
		acc := stations[key]
		if acc == nil {
			continue // no aggregate row: not a station book the scanner reads
		}
		if len(sp.sells) > 0 {
			acc.TypicalSell = summarizePrices(sp.sells).Median // sorts in place
		}
		if len(sp.buys) > 0 {
			acc.TypicalBuy = summarizePrices(sp.buys).Median // sorts in place
		}
	}
	stationKeys := make([]stationKey, 0, len(stations))
	for k := range stations {
		stationKeys = append(stationKeys, k)
	}
	sort.Slice(stationKeys, func(i, j int) bool {
		if stationKeys[i].LocationID != stationKeys[j].LocationID {
			return stationKeys[i].LocationID < stationKeys[j].LocationID
		}
		return stationKeys[i].TypeID < stationKeys[j].TypeID
	})

	// Station leaderboard figures (P5) come from the same staged
	// book, distilled here so the leaderboard page never
	// aggregates at render time: one row per station with the
	// open-order count and the open ISK value per side, where
	// the open value needs price times remaining volume per
	// order, which the type-grain aggregates cannot reconstruct.
	type stationLeaderAccum struct {
		sellOrders, buyOrders int64
		sellValue, buyValue   float64
	}
	leaderAcc := make(map[int64]*stationLeaderAccum)
	leaderRows, err := app.queries.ListMarketSweepStationLeaderboardAggregates(ctx, regionID)
	if err != nil {
		return 0, 0, fmt.Errorf("leaderboard aggregates: %w", err)
	}
	for _, row := range leaderRows {
		acc := leaderAcc[row.LocationID]
		if acc == nil {
			acc = &stationLeaderAccum{}
			leaderAcc[row.LocationID] = acc
		}
		if row.IsBuyOrder == 1 {
			acc.buyOrders = row.OrderCount
			acc.buyValue = row.OpenValue
		} else {
			acc.sellOrders = row.OrderCount
			acc.sellValue = row.OpenValue
		}
	}
	leaderKeys := make([]int64, 0, len(leaderAcc))
	for loc := range leaderAcc {
		leaderKeys = append(leaderKeys, loc)
	}
	sort.Slice(leaderKeys, func(i, j int) bool { return leaderKeys[i] < leaderKeys[j] })

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)

	// Sold per day per type, distilled from market_history in one
	// aggregate pass and stored on the region rows below -- the 7-day
	// average the scanner and tradefinder read. Types with no recorded
	// history are absent here and store 0.
	avgRows, err := qtx.ListMarketAvgDailyVolumes(ctx, regionID)
	if err != nil {
		return 0, 0, err
	}
	avgDailyVolume := make(map[int64]float64, len(avgRows))
	for _, row := range avgRows {
		avgDailyVolume[row.TypeID] = row.AvgDailyVolume
	}

	if err := qtx.DeleteMarketRegionStatsByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	if err := qtx.DeleteMarketStationStatsByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	for start := 0; start < len(typeIDs); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(typeIDs) {
			end = len(typeIDs)
		}
		chunk := typeIDs[start:end]
		n := len(chunk)
		bestSells := make([]float64, n)
		typSells := make([]float64, n)
		sellBands := make([]float64, n)
		bestBuys := make([]float64, n)
		typBuys := make([]float64, n)
		buyBands := make([]float64, n)
		sellOrds := make([]int64, n)
		buyOrds := make([]int64, n)
		sellVols := make([]int64, n)
		buyVols := make([]int64, n)
		avgVols := make([]float64, n)
		sellAvgs := make([]float64, n)
		sellMaxes := make([]float64, n)
		sellP5s := make([]float64, n)
		sellDevs := make([]float64, n)
		buyAvgs := make([]float64, n)
		buyMins := make([]float64, n)
		buyP5s := make([]float64, n)
		buyDevs := make([]float64, n)
		for i, typeID := range chunk {
			st := statsByType[typeID]
			bestSells[i] = st.sell.Best
			typSells[i] = st.sell.Median
			sellBands[i] = st.sell.High90
			bestBuys[i] = st.buy.Best
			typBuys[i] = st.buy.Median
			buyBands[i] = st.buy.Low90
			sellOrds[i] = st.sell.Orders
			buyOrds[i] = st.buy.Orders
			sellVols[i] = st.sellVolume
			buyVols[i] = st.buyVolume
			avgVols[i] = avgDailyVolume[typeID]
			sellAvgs[i] = st.sell.Avg
			sellMaxes[i] = st.sell.Far
			sellP5s[i] = st.sell.P5
			sellDevs[i] = st.sell.StdDev
			buyAvgs[i] = st.buy.Avg
			buyMins[i] = st.buy.Far
			buyP5s[i] = st.buy.P5
			buyDevs[i] = st.buy.StdDev
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, avg_daily_volume, updated_at,
				sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev)
			SELECT $1, unnest($2::bigint[]), unnest($3::float8[]), unnest($4::float8[]), unnest($5::float8[]), unnest($6::float8[]), unnest($7::float8[]), unnest($8::float8[]), unnest($9::bigint[]), unnest($10::bigint[]), unnest($11::bigint[]), unnest($12::bigint[]), unnest($13::float8[]), $14,
				unnest($15::float8[]), unnest($16::float8[]), unnest($17::float8[]), unnest($18::float8[]), unnest($19::float8[]), unnest($20::float8[]), unnest($21::float8[]), unnest($22::float8[])
			ON CONFLICT (region_id, type_id) DO UPDATE SET
				best_sell        = excluded.best_sell,
				typical_sell     = excluded.typical_sell,
				sell_band        = excluded.sell_band,
				best_buy         = excluded.best_buy,
				typical_buy      = excluded.typical_buy,
				buy_band         = excluded.buy_band,
				sell_orders      = excluded.sell_orders,
				buy_orders       = excluded.buy_orders,
				sell_volume      = excluded.sell_volume,
				buy_volume       = excluded.buy_volume,
				avg_daily_volume = excluded.avg_daily_volume,
				updated_at       = excluded.updated_at,
				sell_avg         = excluded.sell_avg,
				sell_max         = excluded.sell_max,
				sell_p5          = excluded.sell_p5,
				sell_stddev      = excluded.sell_stddev,
				buy_avg          = excluded.buy_avg,
				buy_min          = excluded.buy_min,
				buy_p5           = excluded.buy_p5,
				buy_stddev       = excluded.buy_stddev
		`, regionID, pq.Array(chunk), pq.Array(bestSells), pq.Array(typSells), pq.Array(sellBands),
			pq.Array(bestBuys), pq.Array(typBuys), pq.Array(buyBands), pq.Array(sellOrds),
			pq.Array(buyOrds), pq.Array(sellVols), pq.Array(buyVols), pq.Array(avgVols), now,
			pq.Array(sellAvgs), pq.Array(sellMaxes), pq.Array(sellP5s), pq.Array(sellDevs),
			pq.Array(buyAvgs), pq.Array(buyMins), pq.Array(buyP5s), pq.Array(buyDevs)); err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_region_stats_daily (region_id, type_id, day, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume,
				sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev)
			SELECT $1, unnest($2::bigint[]), $3, unnest($4::float8[]), unnest($5::float8[]), unnest($6::float8[]), unnest($7::float8[]), unnest($8::float8[]), unnest($9::float8[]), unnest($10::bigint[]), unnest($11::bigint[]), unnest($12::bigint[]), unnest($13::bigint[]),
				unnest($14::float8[]), unnest($15::float8[]), unnest($16::float8[]), unnest($17::float8[]), unnest($18::float8[]), unnest($19::float8[]), unnest($20::float8[]), unnest($21::float8[])
			ON CONFLICT (region_id, type_id, day) DO UPDATE SET
				best_sell    = excluded.best_sell,
				typical_sell = excluded.typical_sell,
				sell_band    = excluded.sell_band,
				best_buy     = excluded.best_buy,
				typical_buy  = excluded.typical_buy,
				buy_band     = excluded.buy_band,
				sell_orders  = excluded.sell_orders,
				buy_orders   = excluded.buy_orders,
				sell_volume  = excluded.sell_volume,
				buy_volume   = excluded.buy_volume,
				sell_avg     = excluded.sell_avg,
				sell_max     = excluded.sell_max,
				sell_p5      = excluded.sell_p5,
				sell_stddev  = excluded.sell_stddev,
				buy_avg      = excluded.buy_avg,
				buy_min      = excluded.buy_min,
				buy_p5       = excluded.buy_p5,
				buy_stddev   = excluded.buy_stddev
		`, regionID, pq.Array(chunk), day, pq.Array(bestSells), pq.Array(typSells), pq.Array(sellBands),
			pq.Array(bestBuys), pq.Array(typBuys), pq.Array(buyBands), pq.Array(sellOrds),
			pq.Array(buyOrds), pq.Array(sellVols), pq.Array(buyVols),
			pq.Array(sellAvgs), pq.Array(sellMaxes), pq.Array(sellP5s), pq.Array(sellDevs),
			pq.Array(buyAvgs), pq.Array(buyMins), pq.Array(buyP5s), pq.Array(buyDevs)); err != nil {
			return 0, 0, err
		}
	}
	// The stat pack's trend figures (schema 101) derive from the
	// daily rows just written, inside the same transaction, so a
	// reader never sees today's prices beside yesterday's trends.
	if _, err := qtx.RefreshMarketStatPackTrends(ctx, db.RefreshMarketStatPackTrendsParams{
		RegionID: regionID, Today: now,
	}); err != nil {
		return 0, 0, fmt.Errorf("stat pack trends: %w", err)
	}
	for start := 0; start < len(stationKeys); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(stationKeys) {
			end = len(stationKeys)
		}
		chunk := stationKeys[start:end]
		n := len(chunk)
		locIDs := make([]int64, n)
		sTypeIDs := make([]int64, n)
		bestSells := make([]float64, n)
		bestBuys := make([]float64, n)
		typSells := make([]float64, n)
		typBuys := make([]float64, n)
		sellOrds := make([]int64, n)
		buyOrds := make([]int64, n)
		sellVols := make([]int64, n)
		buyVols := make([]int64, n)
		for i, k := range chunk {
			sacc := stations[k]
			locIDs[i] = k.LocationID
			sTypeIDs[i] = k.TypeID
			bestSells[i] = sacc.BestSell
			bestBuys[i] = sacc.BestBuy
			typSells[i] = sacc.TypicalSell
			typBuys[i] = sacc.TypicalBuy
			sellOrds[i] = sacc.SellOrders
			buyOrds[i] = sacc.BuyOrders
			sellVols[i] = sacc.SellVolume
			buyVols[i] = sacc.BuyVolume
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_station_stats (location_id, region_id, type_id, best_sell, best_buy, typical_sell, typical_buy, sell_orders, buy_orders, sell_volume, buy_volume, updated_at)
			SELECT unnest($1::bigint[]), $2, unnest($3::bigint[]), unnest($4::float8[]), unnest($5::float8[]), unnest($6::float8[]), unnest($7::float8[]), unnest($8::bigint[]), unnest($9::bigint[]), unnest($10::bigint[]), unnest($11::bigint[]), $12
			ON CONFLICT (location_id, type_id) DO UPDATE SET
				region_id    = excluded.region_id,
				best_sell    = excluded.best_sell,
				best_buy     = excluded.best_buy,
				typical_sell = excluded.typical_sell,
				typical_buy  = excluded.typical_buy,
				sell_orders  = excluded.sell_orders,
				buy_orders   = excluded.buy_orders,
				sell_volume  = excluded.sell_volume,
				buy_volume   = excluded.buy_volume,
				updated_at   = excluded.updated_at
		`, pq.Array(locIDs), regionID, pq.Array(sTypeIDs), pq.Array(bestSells), pq.Array(bestBuys),
			pq.Array(typSells), pq.Array(typBuys), pq.Array(sellOrds), pq.Array(buyOrds),
			pq.Array(sellVols), pq.Array(buyVols), now); err != nil {
			return 0, 0, err
		}
	}
	// The stored book replaces wholesale in the same transaction:
	// the region's snapshot rows go, then each type's top orders
	// land beside the stats distilled from the same staged book.
	if err := qtx.DeleteMarketBookOrdersByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	type flatBookRow struct {
		typeID int64
		br     bookRow
	}
	var allBookRows []flatBookRow
	for _, typeID := range typeIDs {
		for _, br := range bookByType[typeID] {
			allBookRows = append(allBookRows, flatBookRow{typeID: typeID, br: br})
		}
	}
	for start := 0; start < len(allBookRows); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(allBookRows) {
			end = len(allBookRows)
		}
		chunk := allBookRows[start:end]
		n := len(chunk)
		bTypeIDs := make([]int64, n)
		bIsBuys := make([]int64, n)
		bRanks := make([]int64, n)
		bPrices := make([]float64, n)
		bVols := make([]int64, n)
		bLocIDs := make([]int64, n)
		bSysIDs := make([]int64, n)
		for i, fb := range chunk {
			bTypeIDs[i] = fb.typeID
			bIsBuys[i] = fb.br.isBuy
			bRanks[i] = fb.br.rank
			bPrices[i] = fb.br.price
			bVols[i] = fb.br.volumeRemain
			bLocIDs[i] = fb.br.locationID
			bSysIDs[i] = fb.br.systemID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_book_orders (region_id, type_id, is_buy_order, rank, price, volume_remain, location_id, system_id, updated_at)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::bigint[]), unnest($5::float8[]), unnest($6::bigint[]), unnest($7::bigint[]), unnest($8::bigint[]), $9
		`, regionID, pq.Array(bTypeIDs), pq.Array(bIsBuys), pq.Array(bRanks), pq.Array(bPrices), pq.Array(bVols), pq.Array(bLocIDs), pq.Array(bSysIDs), now); err != nil {
			return 0, 0, err
		}
	}
	if err := qtx.DeleteMarketSweepOrdersByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	if err := qtx.DeleteMarketSweepState(ctx, regionID); err != nil {
		return 0, 0, err
	}
	if err := qtx.DeleteMarketStationLeaderboardByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	for start := 0; start < len(leaderKeys); start += sweepBatchSize {
		end := start + sweepBatchSize
		if end > len(leaderKeys) {
			end = len(leaderKeys)
		}
		chunk := leaderKeys[start:end]
		n := len(chunk)
		locIDs := make([]int64, n)
		sellOrds := make([]int64, n)
		buyOrds := make([]int64, n)
		sellVals := make([]float64, n)
		buyVals := make([]float64, n)
		for i, loc := range chunk {
			acc := leaderAcc[loc]
			locIDs[i] = loc
			sellOrds[i] = acc.sellOrders
			buyOrds[i] = acc.buyOrders
			sellVals[i] = acc.sellValue
			buyVals[i] = acc.buyValue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_station_leaderboard (region_id, location_id, sell_orders, buy_orders, sell_value, buy_value, updated_at)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::bigint[]), unnest($5::float8[]), unnest($6::float8[]), $7
			ON CONFLICT (region_id, location_id) DO UPDATE SET
				sell_orders = excluded.sell_orders,
				buy_orders  = excluded.buy_orders,
				sell_value  = excluded.sell_value,
				buy_value   = excluded.buy_value,
				updated_at  = excluded.updated_at
		`, regionID, pq.Array(locIDs), pq.Array(sellOrds), pq.Array(buyOrds),
			pq.Array(sellVals), pq.Array(buyVals), now); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	app.recordMarketFetch(ctx, regionSweepKind(regionID), fetchStateOK, "")
	return len(typeIDs), len(stationKeys), nil
}

// marketRegionLabel names a region for logs; hub IDs resolve to
// their marketRegions name, anything else is just the ID.
func marketRegionLabel(regionID int64) string {
	if name, ok := marketRegionName(regionID); ok {
		return name
	}
	return strconv.FormatInt(regionID, 10)
}
