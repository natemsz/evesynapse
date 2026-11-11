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
// The sweep (market_region_sweep.go) reads a region's whole book
// inside one of ESI's cache windows on its own lane, and the store
// below writes only the items whose orders changed since the
// region's held book (market_hub_book.go), in one transaction, so
// readers never see a half-written region. The first sweep after a
// restart, and the first of each UTC day, writes every item. A
// region re-sweeps once its data is regionSweepGate old (tracked in
// market_fetch_state under sweep_<region_id>, the same fetched-state
// bookkeeping the other market passes use).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const sweepBatchSize = 2000

const (
	// regionSweepGate is how stale a region's stored stats may
	// get before the lane sweeps that region again: one of ESI's
	// five-minute windows on the market order route, so each hub
	// is read once per copy ESI publishes and never twice.
	regionSweepGate = 5 * time.Minute
	// maxRegionSweepPagesTotal is a safety stop far beyond any
	// real book: if a region ever reported more pages than
	// this, the sweep reads only this many.
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
// region pass already walks (see storeRegionSweepState), so no
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

// sweepPageWindow is how many of one region's book pages are read
// at once. Six regions sweep side by side, so a busy cycle has a few
// dozen requests in flight; ESI's rate budgets, seen coming in the
// client (esi/ratelimit.go), still refuse any that would overdraw.
const sweepPageWindow = 6

const (
	// sweepBackoffBase is a region's wait after its first failed
	// page; consecutive failures double it up to sweepBackoffMax.
	sweepBackoffBase = 5 * time.Minute
	sweepBackoffMax  = 30 * time.Minute
)

// sweepRegionPace is one region's in-memory pacing state: its
// failure backoff and its current page window. It lives on the
// Application (sweepPace) in memory only, by design for this
// hotfix: a restart forgets it and grants each region one fresh
// attempt, which is harmless (the staged cursor on disk is what
// protects progress).
type sweepRegionPace struct {
	failures    int       // consecutive failed pages since the last success
	nextAttempt time.Time // no fetch before this while failures > 0
	window      int       // current page window; 0 means sweepPageWindow
}

// sweepBackoffDelay is the wait after failures consecutive failed
// pages: sweepBackoffBase, doubling, capped at sweepBackoffMax.
func sweepBackoffDelay(failures int) time.Duration {
	delay := sweepBackoffBase
	for i := 1; i < failures && delay < sweepBackoffMax; i++ {
		delay *= 2
	}
	if delay > sweepBackoffMax {
		delay = sweepBackoffMax
	}
	return delay
}

// sweepPaceOf returns the region's pace, creating it on first use.
// The caller must hold sweepPaceMu.
func (app *Application) sweepPaceOf(regionID int64) *sweepRegionPace {
	if app.sweepPace == nil {
		app.sweepPace = make(map[int64]*sweepRegionPace)
	}
	pace, ok := app.sweepPace[regionID]
	if !ok {
		pace = &sweepRegionPace{}
		app.sweepPace[regionID] = pace
	}
	return pace
}

// sweepBackedOff reports whether the region is still waiting out
// the backoff from its last failed page at now.
func (app *Application) sweepBackedOff(regionID int64, now time.Time) bool {
	app.sweepPaceMu.Lock()
	defer app.sweepPaceMu.Unlock()
	pace := app.sweepPace[regionID]
	return pace != nil && now.Before(pace.nextAttempt)
}

// noteSweepPageFailure records a failed page for the region: the
// consecutive-failure count grows, the next attempt waits out the
// backoff, and the page window drops to one page until a full
// window lands clean. It returns the wait now in force. ESI
// error-limit stops never reach here; those stay global.
func (app *Application) noteSweepPageFailure(regionID int64, now time.Time) time.Duration {
	app.sweepPaceMu.Lock()
	defer app.sweepPaceMu.Unlock()
	pace := app.sweepPaceOf(regionID)
	pace.failures++
	pace.window = 1
	delay := sweepBackoffDelay(pace.failures)
	pace.nextAttempt = now.Add(delay)
	return delay
}

// noteSweepPageSuccess records a landed page for the region: any
// failure backoff resets, so a route that recovers sweeps at full
// pace again.
func (app *Application) noteSweepPageSuccess(regionID int64) {
	app.sweepPaceMu.Lock()
	defer app.sweepPaceMu.Unlock()
	if pace := app.sweepPace[regionID]; pace != nil {
		pace.failures = 0
		pace.nextAttempt = time.Time{}
	}
}

// sweepWindow is the region's current page window: the full
// sweepPageWindow, or the reduced one a failed page left it on.
func (app *Application) sweepWindow(regionID int64) int {
	app.sweepPaceMu.Lock()
	defer app.sweepPaceMu.Unlock()
	if pace := app.sweepPace[regionID]; pace != nil && pace.window > 0 {
		return pace.window
	}
	return sweepPageWindow
}

// noteSweepWindowClean records a window that landed every page: a
// region on a reduced window steps one page back toward the full
// window.
func (app *Application) noteSweepWindowClean(regionID int64) {
	app.sweepPaceMu.Lock()
	defer app.sweepPaceMu.Unlock()
	if pace := app.sweepPace[regionID]; pace != nil && pace.window > 0 && pace.window < sweepPageWindow {
		pace.window++
	}
}

// marketRegionLabel names a region for logs; hub IDs resolve to
// their marketRegions name, anything else is just the ID.
func marketRegionLabel(regionID int64) string {
	if name, ok := marketRegionName(regionID); ok {
		return name
	}
	return strconv.FormatInt(regionID, 10)
}

// regionStoreResult is what one stored sweep wrote: whether it was a
// full write, the items written and the items removed, and the
// station rows written, which the stat pack takes in place.
type regionStoreResult struct {
	full        bool
	written     int
	stationRows int
	changed     []int64
	gone        []int64
	stations    []db.MarketStationStat
}

// regionTypeStats is one item's region figures from its book.
type regionTypeStats struct {
	sell, buy             sideStats
	sellVolume, buyVolume int64
}

// bookRow is one stored-book row: the top orders per side the sweep
// keeps for the Market page (schema 050), in display order (sells
// price-ascending, buys price-descending).
type bookRow struct {
	isBuy                int64
	rank                 int64
	price                float64
	volumeRemain         int64
	locationID, systemID int64
}

// typeFigures distills one item's book into its region figures and
// its top orders.
func typeFigures(tb *typeBook) (regionTypeStats, []bookRow) {
	var st regionTypeStats
	sellPrices := make([]float64, len(tb.sells))
	for i, o := range tb.sells {
		sellPrices[i] = o.Price
		st.sellVolume += o.VolumeRemain
	}
	buyPrices := make([]float64, len(tb.buys))
	for i, o := range tb.buys {
		buyPrices[i] = o.Price
		st.buyVolume += o.VolumeRemain
	}
	st.sell = summarizeSide(sellPrices, false)
	st.buy = summarizeSide(buyPrices, true)
	var top []bookRow
	for i := 0; i < len(tb.sells) && i < topMarketOrders; i++ {
		o := tb.sells[i]
		top = append(top, bookRow{isBuy: 0, rank: int64(i), price: o.Price, volumeRemain: o.VolumeRemain, locationID: o.LocationID, systemID: o.SystemID})
	}
	for i := 0; i < len(tb.buys) && i < topMarketOrders; i++ {
		o := tb.buys[len(tb.buys)-1-i]
		top = append(top, bookRow{isBuy: 1, rank: int64(i), price: o.Price, volumeRemain: o.VolumeRemain, locationID: o.LocationID, systemID: o.SystemID})
	}
	return st, top
}

// typeStations distills one item's book per place: bests, counts,
// volumes and the typical (median) price on each side, over orders
// with a known place.
func typeStations(tb *typeBook) map[int64]*stationAccum {
	out := make(map[int64]*stationAccum)
	sells := make(map[int64][]float64)
	buys := make(map[int64][]float64)
	for _, o := range tb.sells {
		if o.LocationID <= 0 {
			continue
		}
		acc := out[o.LocationID]
		if acc == nil {
			acc = &stationAccum{}
			out[o.LocationID] = acc
		}
		if acc.SellOrders == 0 || o.Price < acc.BestSell {
			acc.BestSell = o.Price
		}
		acc.SellOrders++
		acc.SellVolume += o.VolumeRemain
		sells[o.LocationID] = append(sells[o.LocationID], o.Price)
	}
	for _, o := range tb.buys {
		if o.LocationID <= 0 {
			continue
		}
		acc := out[o.LocationID]
		if acc == nil {
			acc = &stationAccum{}
			out[o.LocationID] = acc
		}
		if acc.BuyOrders == 0 || o.Price > acc.BestBuy {
			acc.BestBuy = o.Price
		}
		acc.BuyOrders++
		acc.BuyVolume += o.VolumeRemain
		buys[o.LocationID] = append(buys[o.LocationID], o.Price)
	}
	for loc, p := range sells {
		out[loc].TypicalSell = summarizePrices(p).Median
	}
	for loc, p := range buys {
		out[loc].TypicalBuy = summarizePrices(p).Median
	}
	return out
}

// storeRegionBook writes a swept book into the stored stats. With
// the region's previous book (ob) in hand and the same UTC day, it
// writes only the items whose orders or stored sold-per-day figure
// changed and removes the items that left the book; otherwise it
// rewrites the region whole. Every derived set the sweep feeds
// (daily rows, trends, price lines, change feed, LP values, basket
// values, station rows and heat, the stored top orders, the station
// leaderboard) is written in the same transaction, so a reader never
// sees one figure beside another from a different sweep. Per-item
// figures reuse summarizeSide over the book's own prices -- the exact
// median / 9-in-10 math the item page quotes.
func (app *Application) storeRegionBook(ctx context.Context, regionID int64, nb, ob *hubBook) (regionStoreResult, error) {
	var res regionStoreResult
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	nb.sweptAt, nb.day = now, day

	// Sold per day per type, distilled from market_history in one
	// aggregate pass and stored on the region rows -- the 7-day
	// average the scanner and tradefinder read. Types with no
	// recorded history are absent here and store 0.
	avgRows, err := app.queries.ListMarketAvgDailyVolumes(ctx, regionID)
	if err != nil {
		return res, err
	}
	nb.avgVolume = make(map[int64]float64, len(avgRows))
	for _, row := range avgRows {
		nb.avgVolume[row.TypeID] = row.AvgDailyVolume
	}

	res.full = ob == nil || ob.day != day
	if res.full {
		for typeID := range nb.types {
			res.changed = append(res.changed, typeID)
		}
	} else {
		for typeID, tb := range nb.types {
			if !sameTypeBook(ob.types[typeID], tb) || ob.avgVolume[typeID] != nb.avgVolume[typeID] {
				res.changed = append(res.changed, typeID)
			}
		}
		for typeID := range ob.types {
			if nb.types[typeID] == nil {
				res.gone = append(res.gone, typeID)
			}
		}
	}
	sort.Slice(res.changed, func(i, j int) bool { return res.changed[i] < res.changed[j] })
	sort.Slice(res.gone, func(i, j int) bool { return res.gone[i] < res.gone[j] })
	res.written = len(res.changed)
	if !res.full && len(res.changed) == 0 && len(res.gone) == 0 {
		app.recordMarketFetch(ctx, regionSweepKind(regionID), fetchStateOK, "")
		return res, nil
	}
	touched := append(append([]int64(nil), res.changed...), res.gone...)

	statsByType := make(map[int64]regionTypeStats, len(res.changed))
	bookByType := make(map[int64][]bookRow, len(res.changed))
	stations := make(map[stationKey]*stationAccum)
	for _, typeID := range res.changed {
		tb := nb.types[typeID]
		statsByType[typeID], bookByType[typeID] = typeFigures(tb)
		for loc, acc := range typeStations(tb) {
			stations[stationKey{LocationID: loc, TypeID: typeID}] = acc
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

	// Station leaderboard figures (P5), from the whole book: one row
	// per station with the open-order count and the open ISK value
	// per side, so the leaderboard page never aggregates at render.
	type stationLeaderAccum struct {
		sellOrders, buyOrders int64
		sellValue, buyValue   float64
	}
	leaderAcc := make(map[int64]*stationLeaderAccum)
	for _, tb := range nb.types {
		for _, side := range [][]bookOrder{tb.sells, tb.buys} {
			for _, o := range side {
				if o.LocationID <= 0 {
					continue
				}
				acc := leaderAcc[o.LocationID]
				if acc == nil {
					acc = &stationLeaderAccum{}
					leaderAcc[o.LocationID] = acc
				}
				if o.IsBuy {
					acc.buyOrders++
					acc.buyValue += o.Price * float64(o.VolumeRemain)
				} else {
					acc.sellOrders++
					acc.sellValue += o.Price * float64(o.VolumeRemain)
				}
			}
		}
	}
	leaderKeys := make([]int64, 0, len(leaderAcc))
	for loc := range leaderAcc {
		leaderKeys = append(leaderKeys, loc)
	}
	sort.Slice(leaderKeys, func(i, j int) bool { return leaderKeys[i] < leaderKeys[j] })

	// Station heat (schema 120): the station bests this sweep
	// replaces. From the previous book when one is held, otherwise
	// from the stored rows.
	oldBests := make(map[stationKey]stationAccum)
	if ob != nil {
		for _, typeID := range res.changed {
			if tb := ob.types[typeID]; tb != nil {
				for loc, acc := range typeStations(tb) {
					oldBests[stationKey{LocationID: loc, TypeID: typeID}] = *acc
				}
			}
		}
	}

	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	qtx := app.queries.WithTx(tx)

	if ob == nil {
		oldRows, err := tx.Query(ctx, `
			SELECT location_id, type_id, best_sell, best_buy
			FROM market_station_stats
			WHERE region_id = $1
		`, regionID)
		if err != nil {
			return res, err
		}
		for oldRows.Next() {
			var loc, typ int64
			var acc stationAccum
			if err := oldRows.Scan(&loc, &typ, &acc.BestSell, &acc.BestBuy); err != nil {
				oldRows.Close()
				return res, err
			}
			oldBests[stationKey{LocationID: loc, TypeID: typ}] = acc
		}
		oldRows.Close()
		if err := oldRows.Err(); err != nil {
			return res, err
		}
	}

	if res.full {
		if err := qtx.DeleteMarketRegionStatsByRegion(ctx, regionID); err != nil {
			return res, err
		}
		if err := qtx.DeleteMarketStationStatsByRegion(ctx, regionID); err != nil {
			return res, err
		}
		if err := qtx.DeleteMarketBookOrdersByRegion(ctx, regionID); err != nil {
			return res, err
		}
	} else {
		if len(res.gone) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM market_region_stats WHERE region_id = $1 AND type_id = ANY($2::bigint[])`, regionID, res.gone); err != nil {
				return res, err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM market_station_stats WHERE region_id = $1 AND type_id = ANY($2::bigint[])`, regionID, touched); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM market_book_orders WHERE region_id = $1 AND type_id = ANY($2::bigint[])`, regionID, touched); err != nil {
			return res, err
		}
	}

	for start := 0; start < len(res.changed); start += sweepBatchSize {
		end := min(start+sweepBatchSize, len(res.changed))
		chunk := res.changed[start:end]
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
			avgVols[i] = nb.avgVolume[typeID]
			sellAvgs[i] = st.sell.Avg
			sellMaxes[i] = st.sell.Far
			sellP5s[i] = st.sell.P5
			sellDevs[i] = st.sell.StdDev
			buyAvgs[i] = st.buy.Avg
			buyMins[i] = st.buy.Far
			buyP5s[i] = st.buy.P5
			buyDevs[i] = st.buy.StdDev
		}
		if _, err := tx.Exec(ctx, `
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
		`, regionID, chunk, bestSells, typSells, sellBands,
			bestBuys, typBuys, buyBands, sellOrds,
			buyOrds, sellVols, buyVols, avgVols, now,
			sellAvgs, sellMaxes, sellP5s, sellDevs,
			buyAvgs, buyMins, buyP5s, buyDevs); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `
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
		`, regionID, chunk, day, bestSells, typSells, sellBands,
			bestBuys, typBuys, buyBands, sellOrds,
			buyOrds, sellVols, buyVols,
			sellAvgs, sellMaxes, sellP5s, sellDevs,
			buyAvgs, buyMins, buyP5s, buyDevs); err != nil {
			return res, err
		}
	}

	// The figures derived from the daily rows, for the written items
	// (every item on a full write: an empty list means all).
	scope := res.changed
	if res.full {
		scope = []int64{}
	}
	if len(res.changed) > 0 {
		// The stat pack's trend figures (schema 101), so a reader
		// never sees today's prices beside yesterday's trends.
		if _, err := qtx.RefreshMarketStatPackTrends(ctx, db.RefreshMarketStatPackTrendsParams{
			RegionID: regionID, Today: now, TypeIds: scope,
		}); err != nil {
			return res, fmt.Errorf("stat pack trends: %w", err)
		}
		// The 30-day price line every hover card reads (schema 121).
		if _, err := qtx.RefreshMarketStatPackSparks(ctx, db.RefreshMarketStatPackSparksParams{
			RegionID: regionID, Today: now, TypeIds: scope,
		}); err != nil {
			return res, fmt.Errorf("stat pack sparks: %w", err)
		}
		// The change feed (schema 106) compares the rows just
		// written with their last stored day.
		if _, err := qtx.DetectMarketChanges(ctx, db.DetectMarketChangesParams{
			RegionID: regionID, Day: day, Now: now, MinMove: marketChangeMinMove, TypeIds: scope,
		}); err != nil {
			return res, fmt.Errorf("change feed: %w", err)
		}
	}
	if err := qtx.PruneMarketChanges(ctx, db.PruneMarketChangesParams{
		RegionID: regionID, BeforeDay: now.AddDate(0, 0, -marketChangeKeepDays).Format("2006-01-02"),
	}); err != nil {
		return res, fmt.Errorf("change feed prune: %w", err)
	}
	// LP offer values (schema 102) read the stat pack just written,
	// behind a savepoint: the LP rebuild is a secondary surface, and
	// its failure must not roll back the region's whole sweep.
	if _, err := tx.Exec(ctx, "SAVEPOINT lp_values"); err != nil {
		return res, err
	}
	if err := app.refreshLPOfferValues(ctx, qtx, regionID, now); err != nil {
		logging.Errorf("worker: lp offer values in sweep of %s: %v", marketRegionLabel(regionID), err)
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT lp_values"); err != nil {
			return res, fmt.Errorf("lp offer values: %w", err)
		}
	} else if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT lp_values"); err != nil {
		return res, err
	}
	// Basket values (schema 103): today's value of every basket
	// priced in this region, from the same rows.
	if err := refreshBasketValues(ctx, tx, regionID, day); err != nil {
		return res, fmt.Errorf("basket values: %w", err)
	}

	for start := 0; start < len(stationKeys); start += sweepBatchSize {
		end := min(start+sweepBatchSize, len(stationKeys))
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
			res.stations = append(res.stations, db.MarketStationStat{
				LocationID: k.LocationID, RegionID: regionID, TypeID: k.TypeID,
				BestSell: sacc.BestSell, BestBuy: sacc.BestBuy,
				SellOrders: sacc.SellOrders, BuyOrders: sacc.BuyOrders,
				SellVolume: sacc.SellVolume, BuyVolume: sacc.BuyVolume,
				TypicalSell: sacc.TypicalSell, TypicalBuy: sacc.TypicalBuy, UpdatedAt: now,
			})
		}
		if _, err := tx.Exec(ctx, `
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
		`, locIDs, regionID, sTypeIDs, bestSells, bestBuys,
			typSells, typBuys, sellOrds, buyOrds,
			sellVols, buyVols, now); err != nil {
			return res, err
		}
	}
	res.stationRows = len(stationKeys)
	// Station heat (schema 120): a station's fresh best sell below
	// the best it replaces, or its fresh best buy above it, is one
	// beat against the book there. Log this pass's beats, restate
	// the region's trailing 12 hour counts from the log, and prune
	// log rows a day old, all inside the sweep transaction.
	var beatLocs, beatTypes, beatBuys []int64
	for _, k := range stationKeys {
		old, ok := oldBests[k]
		if !ok {
			continue
		}
		acc := stations[k]
		if acc.BestSell > 0 && old.BestSell > 0 && acc.BestSell < old.BestSell {
			beatLocs, beatTypes, beatBuys = append(beatLocs, k.LocationID), append(beatTypes, k.TypeID), append(beatBuys, 0)
		}
		if acc.BestBuy > 0 && old.BestBuy > 0 && acc.BestBuy > old.BestBuy {
			beatLocs, beatTypes, beatBuys = append(beatLocs, k.LocationID), append(beatTypes, k.TypeID), append(beatBuys, 1)
		}
	}
	if len(beatLocs) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_best_beats (location_id, region_id, type_id, is_buy_order, at)
			SELECT unnest($1::bigint[]), $2, unnest($3::bigint[]), unnest($4::bigint[]), $5
		`, beatLocs, regionID, beatTypes, beatBuys, now); err != nil {
			return res, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM market_station_heat WHERE region_id = $1`, regionID); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO market_station_heat (location_id, region_id, type_id, is_buy_order, beats_12h, computed_at)
		SELECT location_id, region_id, type_id, is_buy_order, COUNT(*), $3
		FROM market_best_beats
		WHERE region_id = $1 AND at >= $2
		GROUP BY location_id, region_id, type_id, is_buy_order
	`, regionID, now.Add(-12*time.Hour), now); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM market_best_beats WHERE region_id = $1 AND at < $2`,
		regionID, now.Add(-24*time.Hour)); err != nil {
		return res, err
	}

	// The stored top orders of every written item, beside the stats
	// distilled from the same book.
	type flatBookRow struct {
		typeID int64
		br     bookRow
	}
	var allBookRows []flatBookRow
	for _, typeID := range res.changed {
		for _, br := range bookByType[typeID] {
			allBookRows = append(allBookRows, flatBookRow{typeID: typeID, br: br})
		}
	}
	for start := 0; start < len(allBookRows); start += sweepBatchSize {
		end := min(start+sweepBatchSize, len(allBookRows))
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_book_orders (region_id, type_id, is_buy_order, rank, price, volume_remain, location_id, system_id, updated_at)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::bigint[]), unnest($5::float8[]), unnest($6::bigint[]), unnest($7::bigint[]), unnest($8::bigint[]), $9
		`, regionID, bTypeIDs, bIsBuys, bRanks, bPrices, bVols, bLocIDs, bSysIDs, now); err != nil {
			return res, err
		}
	}

	if err := qtx.DeleteMarketStationLeaderboardByRegion(ctx, regionID); err != nil {
		return res, err
	}
	for start := 0; start < len(leaderKeys); start += sweepBatchSize {
		end := min(start+sweepBatchSize, len(leaderKeys))
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_station_leaderboard (region_id, location_id, sell_orders, buy_orders, sell_value, buy_value, updated_at)
			SELECT $1, unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::bigint[]), unnest($5::float8[]), unnest($6::float8[]), $7
			ON CONFLICT (region_id, location_id) DO UPDATE SET
				sell_orders = excluded.sell_orders,
				buy_orders  = excluded.buy_orders,
				sell_value  = excluded.sell_value,
				buy_value   = excluded.buy_value,
				updated_at  = excluded.updated_at
		`, regionID, locIDs, sellOrds, buyOrds,
			sellVals, buyVals, now); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	app.recordMarketFetch(ctx, regionSweepKind(regionID), fetchStateOK, "")
	return res, nil
}
