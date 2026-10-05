package app

// P1 region stats platform (Element43 parity plan): the worker
// periodically reads each hub region's WHOLE order book and stores
// per-type statistics -- best and typical (median) prices and the
// 9-in-10 bands on both sides, order counts, remaining volumes --
// in market_region_stats (schema 031). Everything later market
// phases read starts here, and pages only ever SELECT these rows;
// aggregation happens at sweep time, never at render time.
//
// The sweep is incremental by design: one region at a time, at
// most maxRegionSweepPagesPerCycle pages per worker cycle,
// assembling prices in memory and writing only when the book has
// been read end to end. A completed region re-sweeps once its
// data is regionSweepGate old (tracked in market_fetch_state
// under sweep_<region_id>, the same fetched-state bookkeeping the
// other market passes use). Partial sweeps live only in memory
// on the Application; an interrupted sweep simply restarts, so
// readers never see a half-written region.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	// regionSweepGate is how stale a region's stored stats may
	// get before the worker sweeps that region again.
	regionSweepGate = time.Hour
	// maxRegionSweepPagesPerCycle bounds how many whole-book
	// pages one worker cycle may advance a sweep by -- the
	// whole-book cousin of maxBookFetchesPerCycle. At the
	// one-minute cycle cadence even The Forge's book (~a few
	// hundred pages) finishes inside a quarter hour, and no
	// single cycle bursts hundreds of requests at ESI.
	maxRegionSweepPagesPerCycle = 25
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

// regionTypeAccum gathers one type's book during a sweep: just
// the two sides' prices (all the median/band math needs), the
// order counts (the price slices' lengths), and the remaining
// volumes. Whole orders are never kept.
type regionTypeAccum struct {
	SellPrices []float64
	BuyPrices  []float64
	SellVolume int64
	BuyVolume  int64
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

// regionSweepState is the one in-progress sweep: which region,
// how far it has paged, and the per-type accumulators. Pages
// append; nothing is written to the database until NextPage
// passes TotalPages.
type regionSweepState struct {
	RegionID   int64
	NextPage   int
	TotalPages int // 0 until page 1 reports X-Pages
	Types      map[int64]*regionTypeAccum
	Stations   map[stationKey]*stationAccum
}

// sweepRegionStats is the P1 pass, called from refreshMarketData
// after the existing history and order-health passes; it spends
// what is left of the cycle's allowance. It continues the
// in-progress sweep (never two at once), or starts the first
// region whose stored stats are stale. It returns how many pages
// it read, and whether ESI cut the cycle short.
func (app *Application) sweepRegionStats(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.regionSweepMu.Lock()
	defer app.regionSweepMu.Unlock()

	sw := app.regionSweep
	if sw == nil {
		regionID, ok := app.nextRegionToSweep(ctx)
		if !ok {
			return 0, false
		}
		sw = &regionSweepState{
			RegionID: regionID,
			NextPage: 1,
			Types:    make(map[int64]*regionTypeAccum),
			Stations: make(map[stationKey]*stationAccum),
		}
		app.regionSweep = sw
		log.Printf("worker: region sweep: starting %s (%d)", marketRegionLabel(sw.RegionID), sw.RegionID)
	}

	pages := 0
	complete := false
	for pages < maxRegionSweepPagesPerCycle {
		if ctx.Err() != nil {
			break
		}
		if !allowance.take() {
			break
		}
		orders, totalPages, err := app.fetchRegionBookPage(ctx, sw.RegionID, sw.NextPage)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				log.Printf("worker: region sweep: %s cut short by ESI error limit on page %d", marketRegionLabel(sw.RegionID), sw.NextPage)
				return pages, true
			}
			// Transient failure: keep the partial sweep and
			// retry this page next cycle; nothing was stored.
			log.Printf("worker: region sweep: %s page %d unavailable: %v", marketRegionLabel(sw.RegionID), sw.NextPage, err)
			break
		}
		if totalPages > sw.TotalPages {
			sw.TotalPages = totalPages
		}
		for i := range orders {
			o := &orders[i]
			acc := sw.Types[o.TypeID]
			if acc == nil {
				acc = &regionTypeAccum{}
				sw.Types[o.TypeID] = acc
			}
			if o.IsBuyOrder {
				acc.BuyPrices = append(acc.BuyPrices, o.Price)
				acc.BuyVolume += o.VolumeRemain
			} else {
				acc.SellPrices = append(acc.SellPrices, o.Price)
				acc.SellVolume += o.VolumeRemain
			}
			// Station grain (P2): the same order also feeds its
			// own place's best prices and depth, so the spread
			// scanner never re-reads a book.
			if o.LocationID > 0 {
				skey := stationKey{LocationID: o.LocationID, TypeID: o.TypeID}
				sacc := sw.Stations[skey]
				if sacc == nil {
					sacc = &stationAccum{}
					sw.Stations[skey] = sacc
				}
				if o.IsBuyOrder {
					if o.Price > sacc.BestBuy {
						sacc.BestBuy = o.Price
					}
					sacc.BuyOrders++
					sacc.BuyVolume += o.VolumeRemain
				} else {
					if sacc.BestSell == 0 || o.Price < sacc.BestSell {
						sacc.BestSell = o.Price
					}
					sacc.SellOrders++
					sacc.SellVolume += o.VolumeRemain
				}
			}
		}
		pages++
		sw.NextPage++
		if sw.TotalPages > 0 && sw.NextPage > sw.TotalPages {
			complete = true
			break
		}
		if sw.NextPage > maxRegionSweepPagesTotal {
			log.Printf("worker: region sweep: %s passed the %d-page safety stop; storing what was read", marketRegionLabel(sw.RegionID), maxRegionSweepPagesTotal)
			complete = true
			break
		}
	}

	if complete {
		if err := app.storeRegionSweep(ctx, sw); err != nil {
			log.Printf("worker: region sweep: store %s: %v", marketRegionLabel(sw.RegionID), err)
		} else {
			log.Printf("worker: region sweep: %s done: %d types, %d station rows over %d pages", marketRegionLabel(sw.RegionID), len(sw.Types), len(sw.Stations), sw.NextPage-1)
			app.regionSweep = nil // free the sweep's memory
		}
	}
	return pages, false
}

// nextRegionToSweep picks the next region to sweep: the first
// hub (in marketRegions order) whose last completed sweep is at
// least regionSweepGate old -- a fresh region is never
// re-fetched. ok is false when every hub is fresh.
func (app *Application) nextRegionToSweep(ctx context.Context) (int64, bool) {
	for _, region := range marketRegions {
		if app.marketFetchDue(ctx, regionSweepKind(region.ID), regionSweepGate) {
			return region.ID, true
		}
	}
	return 0, false
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

// storeRegionSweep writes a completed sweep: the region's rows
// are replaced wholesale inside one transaction (a type that
// left the book loses its row), the station-grain rows for the
// spread scanner are replaced in the same transaction, today's
// daily snapshot rows are upserted alongside, and the sweep's
// fetch-state row is marked ok so the region waits out
// regionSweepGate before the next sweep. Per-type figures reuse
// summarizePrices -- the exact median / 9-in-10 math the item
// page quotes live.
func (app *Application) storeRegionSweep(ctx context.Context, sw *regionSweepState) error {
	now := time.Now().UTC()
	updatedAt := now.Format(time.RFC3339)
	day := now.Format("2006-01-02")

	typeIDs := make([]int64, 0, len(sw.Types))
	for typeID := range sw.Types {
		typeIDs = append(typeIDs, typeID)
	}
	sort.Slice(typeIDs, func(i, j int) bool { return typeIDs[i] < typeIDs[j] })

	stationKeys := make([]stationKey, 0, len(sw.Stations))
	for k := range sw.Stations {
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
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)

	if err := qtx.DeleteMarketRegionStatsByRegion(ctx, sw.RegionID); err != nil {
		return err
	}
	if err := qtx.DeleteMarketStationStatsByRegion(ctx, sw.RegionID); err != nil {
		return err
	}
	for _, typeID := range typeIDs {
		acc := sw.Types[typeID]
		var bestSell, typicalSell, sellBand float64
		if len(acc.SellPrices) > 0 {
			stats := summarizePrices(acc.SellPrices) // sorts in place
			bestSell = acc.SellPrices[0]
			typicalSell = stats.Median
			sellBand = stats.High90 // 9 in 10 sell orders at or under this
		}
		var bestBuy, typicalBuy, buyBand float64
		if len(acc.BuyPrices) > 0 {
			stats := summarizePrices(acc.BuyPrices) // sorts in place
			bestBuy = acc.BuyPrices[len(acc.BuyPrices)-1]
			typicalBuy = stats.Median
			buyBand = stats.Low90 // 9 in 10 buy orders at or over this
		}
		if err := qtx.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
			RegionID: sw.RegionID, TypeID: typeID,
			BestSell: bestSell, TypicalSell: typicalSell, SellBand: sellBand,
			BestBuy: bestBuy, TypicalBuy: typicalBuy, BuyBand: buyBand,
			SellOrders: int64(len(acc.SellPrices)), BuyOrders: int64(len(acc.BuyPrices)),
			SellVolume: acc.SellVolume, BuyVolume: acc.BuyVolume,
			UpdatedAt: updatedAt,
		}); err != nil {
			return err
		}
		if err := qtx.UpsertMarketRegionStatDaily(ctx, db.UpsertMarketRegionStatDailyParams{
			RegionID: sw.RegionID, TypeID: typeID, Day: day,
			BestSell: bestSell, TypicalSell: typicalSell, SellBand: sellBand,
			BestBuy: bestBuy, TypicalBuy: typicalBuy, BuyBand: buyBand,
			SellOrders: int64(len(acc.SellPrices)), BuyOrders: int64(len(acc.BuyPrices)),
			SellVolume: acc.SellVolume, BuyVolume: acc.BuyVolume,
		}); err != nil {
			return err
		}
	}
	for _, k := range stationKeys {
		sacc := sw.Stations[k]
		if err := qtx.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: k.LocationID, RegionID: sw.RegionID, TypeID: k.TypeID,
			BestSell: sacc.BestSell, BestBuy: sacc.BestBuy,
			SellOrders: sacc.SellOrders, BuyOrders: sacc.BuyOrders,
			SellVolume: sacc.SellVolume, BuyVolume: sacc.BuyVolume,
			UpdatedAt: updatedAt,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	app.recordMarketFetch(ctx, regionSweepKind(sw.RegionID), fetchStateOK, "")
	return nil
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
