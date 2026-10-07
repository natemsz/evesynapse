package app

// The Market page's regions strip: one row per hub region for the
// item being looked at, read from the stats the region sweep stores
// (market_region_stats.go). Reading only; nothing here fetches.

import (
	"context"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

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
		logging.Errorf("market: region stats for type %d: %v", item.TypeID, err)
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

// statsAgeText renders a time as a short end-user age for the
// regions strip ("just now", "12 minutes ago").
func statsAgeText(at time.Time) string {
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
