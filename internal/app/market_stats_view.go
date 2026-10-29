package app

// The Market page's regions strip: one row per hub region for the
// item being looked at, read from the stats the region sweep stores
// (market_region_stats.go). Reading only; nothing here fetches.

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// marketRegionStatRow is one hub row of the item page's regions
// strip, built from stored stats only: our sweep's row, or the
// fallback mirror's where the swap in market_fallback.go picks
// it (Source names it, in that case).
type marketRegionStatRow struct {
	RegionID    int64
	RegionName  string
	Active      bool        // the region the page is currently showing
	HasData     bool        // a stored row speaks for this type in this region
	TypicalSell string      // esi.FormatISK, "" when no sell orders
	TypicalBuy  string      // esi.FormatISK, "" when no buy orders
	Age         string      // "12 minutes ago", "" when HasData is false
	Source      string      // fallbackSourceLabel on fallback rows, "Global PLEX market" on global-book rows, "" on our own
	Hub         bool        // a hub region (its market page exists); the all-regions strip also lists others
	Chg7        trendFigure // 7-day change of the typical sell, from the stat pack (own rows only)
}

// attachRegionStats fills an item view's hub-regions strip from
// the stored region stats, our own and the fallback mirror's,
// picked between per region by the swap (resolvedRegionTypicals).
// Pure database read -- the strip never triggers a fetch;
// regions no stored row speaks for render as "no data yet".
func (app *Application) attachRegionStats(ctx context.Context, item *marketItem) {
	if item == nil {
		return
	}
	ownByRegion := make(map[int64]*db.MarketRegionStat)
	if rows, err := app.queries.ListMarketRegionStatsByType(ctx, item.TypeID); err != nil {
		logging.Errorf("market: region stats for type %d: %v", item.TypeID, err)
	} else {
		for i := range rows {
			ownByRegion[rows[i].RegionID] = &rows[i]
		}
	}
	fallbackByRegion := make(map[int64]*db.MarketStatsFallback)
	if rows, err := app.queries.ListMarketStatsFallbackByType(ctx, item.TypeID); err != nil {
		logging.Errorf("market: fallback stats for type %d: %v", item.TypeID, err)
	} else {
		for i := range rows {
			fallbackByRegion[rows[i].RegionID] = &rows[i]
		}
	}
	now := time.Now()
	item.RegionStats = item.RegionStats[:0]
	for _, region := range marketRegions {
		row := marketRegionStatRow{
			RegionID:   region.ID,
			RegionName: region.Name,
			Hub:        true,
			Active:     region.ID == item.RegionID,
		}
		ownRow := ownByRegion[region.ID]
		fromGlobal := false
		if ownRow == nil {
			// The global market serves where the hub has no
			// stat: a global-market type's book lives there,
			// so the hub row quotes that book (and labels it)
			// instead of reporting no data for the one market
			// the type trades on.
			ownRow = ownByRegion[globalMarketRegion]
			fromGlobal = ownRow != nil
		}
		if res, ok := resolvedRegionTypicals(ownRow, fallbackByRegion[region.ID], now); ok {
			row.HasData = true
			if res.TypicalSell > 0 {
				row.TypicalSell = esi.FormatISK(res.TypicalSell)
			}
			if res.TypicalBuy > 0 {
				row.TypicalBuy = esi.FormatISK(res.TypicalBuy)
			}
			row.Age = ageText(now, res.AsOf)
			if res.Fallback {
				row.Source = fallbackSourceLabel
			} else if fromGlobal {
				row.Source = "Global PLEX market"
			}
			if !res.Fallback {
				row.Chg7 = newTrendFigure(ownRow.SellChg7d)
			}
		}
		item.RegionStats = append(item.RegionStats, row)
	}
}
