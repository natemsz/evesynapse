package app

// The order book: reading a region's open orders for one type and
// summarising their prices (best, typical, and the band most of them
// sit in). The Market page reads a book live for the item being
// looked at; the worker's order-health pass and the region sweep use
// the same code.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"evesynapse/internal/esi"
)

// maxOrderPages caps the order-book pagination so a mega-traded
// type in The Forge can't fan out into hundreds of requests; the
// page notes when the cap bites (best prices are then best-within-
// the-pages-read, not global).
const maxOrderPages = 20

// fetchOrderBook reads one region's orders for a type, following
// X-Pages up to maxOrderPages. Split into sides, unsorted.
func (app *Application) fetchOrderBook(ctx context.Context, regionID, typeID int64) (sells, buys []esi.MarketOrder, truncated bool, err error) {
	path := fmt.Sprintf("/markets/%d/orders/?type_id=%d&order_type=all", regionID, typeID)

	var all []esi.MarketOrder
	totalPages := 1
	for page := 1; page <= totalPages && page <= maxOrderPages; page++ {
		body, header, ferr := app.esi.FetchRaw(ctx, "", fmt.Sprintf("%s&page=%d", path, page))
		if ferr != nil {
			return nil, nil, false, ferr
		}
		if page == 1 {
			if xp := header.Get("X-Pages"); xp != "" {
				if n, aerr := strconv.Atoi(xp); aerr == nil && n > 1 {
					totalPages = n
				}
			}
			truncated = totalPages > maxOrderPages
		}
		var orders []esi.MarketOrder
		if derr := json.Unmarshal(body, &orders); derr != nil {
			return nil, nil, false, fmt.Errorf("market: decode orders page %d: %w", page, derr)
		}
		all = append(all, orders...)
	}

	for _, o := range all {
		if o.IsBuyOrder {
			buys = append(buys, o)
		} else {
			sells = append(sells, o)
		}
	}
	return sells, buys, truncated, nil
}

// bookStats summarizes one side of a regional order book so the
// item page can quote prices a lone joke order cannot poison:
// the median (typical) order and the band the bulk of orders sit
// in. Prices are per order row — each order counts once,
// whatever its volume.
type bookStats struct {
	Median float64 // typical order price
	Low90  float64 // 9 in 10 orders are at or above this
	High90 float64 // 9 in 10 orders are at or below this
}

// summarizeBook computes bookStats over one side's orders. ok is
// false for an empty side.
func summarizeBook(orders []esi.MarketOrder) (bookStats, bool) {
	if len(orders) == 0 {
		return bookStats{}, false
	}
	prices := make([]float64, len(orders))
	for i, o := range orders {
		prices[i] = o.Price
	}
	return summarizePrices(prices), true
}

// summarizePrices is the shared core of summarizeBook: the same
// median / 9-in-10 math over one side's raw order prices, for
// callers that stream a book page by page and never hold whole
// orders (the region sweep, market_region_stats.go). prices is
// sorted in place.
func summarizePrices(prices []float64) bookStats {
	sort.Float64s(prices)
	return bookStats{
		Median: medianPrice(prices),
		Low90:  pricePercentileNR(prices, 10),
		High90: pricePercentileNR(prices, 90),
	}
}

// medianPrice is the middle of ascending prices, averaging the
// two middle values on an even count.
func medianPrice(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// pricePercentileNR returns the nearest-rank p-th percentile of
// ascending prices: the smallest price at least p% of orders are
// at or below, so a "9 in 10 orders" label is exactly true.
func pricePercentileNR(sorted []float64, p float64) float64 {
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
