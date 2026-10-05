package app

// P1 region stats platform (Element43 parity plan): the worker
// periodically reads each hub region's WHOLE order book and stores
// per-type statistics -- best and typical (median) prices and the
// 9-in-10 bands on both sides, order counts, remaining volumes --
// in market_region_stats (schema 031). Everything later market
// phases read starts here, and pages only ever SELECT these rows;
// aggregation happens at sweep time, never at render time.
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
	"log"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	// regionSweepGate is how stale a region's stored stats may
	// get before the worker sweeps that region again.
	regionSweepGate = time.Hour
	// maxRegionSweepPagesPerCycle bounds how many whole-book
	// pages one worker cycle may advance EACH region's sweep
	// by -- the whole-book cousin of maxBookFetchesPerCycle. At
	// the one-minute cycle cadence even The Forge's book (~a
	// few hundred pages) finishes inside a few minutes per
	// region, and no single cycle bursts hundreds of requests
	// at ESI.
	maxRegionSweepPagesPerCycle = 50
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

// stationKey identifies one (place, type) pair during a sweep.
type stationKey struct {
	LocationID int64
	TypeID     int64
}

// stationAccum gathers one place-and-type's book during a sweep:
// just the best price on each side, the order counts, and the
// remaining volumes. Whole orders are never kept, same memory
// discipline as the region accumulators.
type stationAccum struct {
	BestSell   float64 // lowest sell price seen; 0 = none yet
	BestBuy    float64 // highest buy price seen; 0 = none yet
	SellOrders int64
	BuyOrders  int64
	SellVolume int64
	BuyVolume  int64
}

// regionSweepOutcome is one region's advance in a sweep pass:
// how many pages it read and whether ESI's error limit stopped
// it.
type regionSweepOutcome struct {
	pages   int
	limited bool
}

// sweepRegionStats is the P1 pass, called from refreshMarketData
// after the existing history and order-health passes; it spends
// what is left of the cycle's allowance. Every hub region with a
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
	outcomes := make([]regionSweepOutcome, len(marketRegions))
	var wg sync.WaitGroup
	for i, region := range marketRegions {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		now := time.Now().UTC().Format(time.RFC3339)
		if err := app.queries.InsertMarketSweepState(ctx, db.InsertMarketSweepStateParams{
			RegionID: regionID, StartedAt: now, UpdatedAt: now,
		}); err != nil {
			log.Printf("worker: region sweep: start %s: %v", marketRegionLabel(regionID), err)
			return out
		}
		state = db.MarketSweepState{RegionID: regionID, NextPage: 1, StartedAt: now, UpdatedAt: now}
		log.Printf("worker: region sweep: starting %s (%d)", marketRegionLabel(regionID), regionID)
	case err != nil:
		log.Printf("worker: region sweep: read %s sweep state: %v", marketRegionLabel(regionID), err)
		return out
	}

	// The whole book is already staged but its store never
	// landed (crash, or the store failed): finish it before
	// fetching anything new.
	if sweepBookStaged(state) || state.NextPage > maxRegionSweepPagesTotal {
		app.completeRegionSweep(ctx, state)
		return out
	}

	for out.pages < maxRegionSweepPagesPerCycle {
		if ctx.Err() != nil || limitHit.Load() {
			break
		}
		if !allowance.take() {
			break
		}
		orders, totalPages, err := app.fetchRegionBookPage(ctx, regionID, int(state.NextPage))
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				limitHit.Store(true)
				log.Printf("worker: region sweep: %s cut short by ESI error limit on page %d", marketRegionLabel(regionID), state.NextPage)
				out.limited = true
				return out
			}
			// Transient failure: the staged pages and the
			// cursor stay put, and this page is retried next
			// cycle; nothing was stored.
			log.Printf("worker: region sweep: %s page %d unavailable: %v", marketRegionLabel(regionID), state.NextPage, err)
			break
		}
		if int64(totalPages) > state.PagesTotal {
			state.PagesTotal = int64(totalPages)
		}
		state.NextPage++
		if err := app.stageSweepPage(ctx, state, orders); err != nil {
			log.Printf("worker: region sweep: stage %s page %d: %v", marketRegionLabel(regionID), state.NextPage-1, err)
			break
		}
		out.pages++
		if sweepBookStaged(state) {
			break
		}
		if state.NextPage > maxRegionSweepPagesTotal {
			log.Printf("worker: region sweep: %s passed the %d-page safety stop; storing what was read", marketRegionLabel(regionID), maxRegionSweepPagesTotal)
			break
		}
	}
	if sweepBookStaged(state) || state.NextPage > maxRegionSweepPagesTotal {
		app.completeRegionSweep(ctx, state)
	}
	return out
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
	for i := range orders {
		o := &orders[i]
		isBuy := int64(0)
		if o.IsBuyOrder {
			isBuy = 1
		}
		if err := qtx.InsertMarketSweepOrder(ctx, db.InsertMarketSweepOrderParams{
			RegionID: state.RegionID, TypeID: o.TypeID, IsBuyOrder: isBuy,
			Price: o.Price, VolumeRemain: o.VolumeRemain, LocationID: o.LocationID,
		}); err != nil {
			return err
		}
	}
	if err := qtx.UpdateMarketSweepState(ctx, db.UpdateMarketSweepStateParams{
		NextPage: state.NextPage, PagesTotal: state.PagesTotal,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339), RegionID: state.RegionID,
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
		log.Printf("worker: region sweep: store %s: %v", marketRegionLabel(state.RegionID), err)
		return
	}
	log.Printf("worker: region sweep: %s done: %d types, %d station rows over %d pages", marketRegionLabel(state.RegionID), types, stations, state.NextPage-1)
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
	updatedAt := now.Format(time.RFC3339)
	day := now.Format("2006-01-02")

	// Region grain: one type at a time, its staged prices fed
	// to summarizePrices, so the largest slice held at once is
	// one type's orders -- never the whole book.
	typeIDs, err := app.queries.ListMarketSweepTypeIDs(ctx, regionID)
	if err != nil {
		return 0, 0, err
	}
	type regionTypeStats struct {
		bestSell, typicalSell, sellBand float64
		bestBuy, typicalBuy, buyBand    float64
		sellOrders, buyOrders           int64
		sellVolume, buyVolume           int64
	}
	statsByType := make(map[int64]*regionTypeStats, len(typeIDs))
	for _, typeID := range typeIDs {
		rows, err := app.queries.ListMarketSweepTypeOrders(ctx, db.ListMarketSweepTypeOrdersParams{
			RegionID: regionID, TypeID: typeID,
		})
		if err != nil {
			return 0, 0, err
		}
		st := &regionTypeStats{}
		var sellPrices, buyPrices []float64
		for _, row := range rows {
			if row.IsBuyOrder == 1 {
				buyPrices = append(buyPrices, row.Price)
				st.buyVolume += row.VolumeRemain
			} else {
				sellPrices = append(sellPrices, row.Price)
				st.sellVolume += row.VolumeRemain
			}
		}
		st.sellOrders = int64(len(sellPrices))
		st.buyOrders = int64(len(buyPrices))
		if len(sellPrices) > 0 {
			stats := summarizePrices(sellPrices) // sorts in place
			st.bestSell = sellPrices[0]
			st.typicalSell = stats.Median
			st.sellBand = stats.High90 // 9 in 10 sell orders at or under this
		}
		if len(buyPrices) > 0 {
			stats := summarizePrices(buyPrices) // sorts in place
			st.bestBuy = buyPrices[len(buyPrices)-1]
			st.typicalBuy = stats.Median
			st.buyBand = stats.Low90 // 9 in 10 buy orders at or over this
		}
		statsByType[typeID] = st
	}

	// Station grain (P2): the staged book aggregated per place
	// and type, so the spread scanner never re-reads a book.
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

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)

	// Sold per day per type, distilled from market_history in
	// one aggregate pass and stored on the region rows below --
	// the same 7-day average the scanner and tradefinder used
	// to recompute per candidate type on every render. Types
	// with no recorded history are absent here and store 0.
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
	for _, typeID := range typeIDs {
		st := statsByType[typeID]
		if err := qtx.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
			RegionID: regionID, TypeID: typeID,
			BestSell: st.bestSell, TypicalSell: st.typicalSell, SellBand: st.sellBand,
			BestBuy: st.bestBuy, TypicalBuy: st.typicalBuy, BuyBand: st.buyBand,
			SellOrders: st.sellOrders, BuyOrders: st.buyOrders,
			SellVolume: st.sellVolume, BuyVolume: st.buyVolume,
			AvgDailyVolume: avgDailyVolume[typeID],
			UpdatedAt:      updatedAt,
		}); err != nil {
			return 0, 0, err
		}
		if err := qtx.UpsertMarketRegionStatDaily(ctx, db.UpsertMarketRegionStatDailyParams{
			RegionID: regionID, TypeID: typeID, Day: day,
			BestSell: st.bestSell, TypicalSell: st.typicalSell, SellBand: st.sellBand,
			BestBuy: st.bestBuy, TypicalBuy: st.typicalBuy, BuyBand: st.buyBand,
			SellOrders: st.sellOrders, BuyOrders: st.buyOrders,
			SellVolume: st.sellVolume, BuyVolume: st.buyVolume,
		}); err != nil {
			return 0, 0, err
		}
	}
	for _, k := range stationKeys {
		sacc := stations[k]
		if err := qtx.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: k.LocationID, RegionID: regionID, TypeID: k.TypeID,
			BestSell: sacc.BestSell, BestBuy: sacc.BestBuy,
			SellOrders: sacc.SellOrders, BuyOrders: sacc.BuyOrders,
			SellVolume: sacc.SellVolume, BuyVolume: sacc.BuyVolume,
			UpdatedAt: updatedAt,
		}); err != nil {
			return 0, 0, err
		}
	}
	if err := qtx.DeleteMarketSweepOrdersByRegion(ctx, regionID); err != nil {
		return 0, 0, err
	}
	if err := qtx.DeleteMarketSweepState(ctx, regionID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	app.recordMarketFetch(ctx, regionSweepKind(regionID), fetchStateOK, "")
	return len(typeIDs), len(stationKeys), nil
}

// marketRegionStatRow is one hub row of the item page's regions
// strip, built from stored sweep stats only.
type marketRegionStatRow struct {
	RegionID    int64
	RegionName  string
	Active      bool   // the region the page is currently showing
	HasData     bool   // a sweep has covered this type in this region
	TypicalSell string // esi.FormatISK, "" when no sell orders
	TypicalBuy  string // esi.FormatISK, "" when no buy orders
	Age         string // "12 minutes ago", "" when HasData is false
}

// attachRegionStats fills an item view's hub-regions strip from
// market_region_stats. Pure database read -- the strip never
// triggers a fetch; regions the worker has not swept yet render
// as "no data yet".
func (app *Application) attachRegionStats(ctx context.Context, item *marketItem) {
	if item == nil {
		return
	}
	byRegion := make(map[int64]db.MarketRegionStat)
	if rows, err := app.queries.ListMarketRegionStatsByType(ctx, item.TypeID); err != nil {
		log.Printf("market: region stats for type %d: %v", item.TypeID, err)
	} else {
		for _, row := range rows {
			byRegion[row.RegionID] = row
		}
	}
	item.RegionStats = item.RegionStats[:0]
	for _, region := range marketRegions {
		row := marketRegionStatRow{
			RegionID:   region.ID,
			RegionName: region.Name,
			Active:     region.ID == item.RegionID,
		}
		if stat, ok := byRegion[region.ID]; ok {
			row.HasData = true
			if stat.TypicalSell > 0 {
				row.TypicalSell = esi.FormatISK(stat.TypicalSell)
			}
			if stat.TypicalBuy > 0 {
				row.TypicalBuy = esi.FormatISK(stat.TypicalBuy)
			}
			row.Age = statsAgeText(stat.UpdatedAt)
		}
		item.RegionStats = append(item.RegionStats, row)
	}
}

// statsAgeText renders an RFC3339 stamp as a short end-user age
// for the regions strip ("just now", "12 minutes ago").
func statsAgeText(rfc string) string {
	at, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return ""
	}
	d := time.Since(at)
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		if h := int(d.Hours()); h == 1 {
			return "1 hour ago"
		} else {
			return fmt.Sprintf("%d hours ago", h)
		}
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
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
