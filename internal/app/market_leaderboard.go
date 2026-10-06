package app

// P5 station leaderboard (Element43 parity): the discovery
// surface over the sweeps' per-station totals. A completed
// region sweep distills one row per station into
// market_station_leaderboard (market_region_stats.go); this
// page only ever reads those stored rows and sorts them, so a
// render costs no ESI traffic and no render-time aggregation.
// "?sort=orders" ranks by open-order count; the default "value"
// ranks by ISK on orders. "?region=<id>" filters to one hub
// region, 0 (or absent) covers all five.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

// leaderboardRowCap is the most stations the leaderboard
// ranks: a discovery list, not a full dump of every hub
// structure with an order on it.
const leaderboardRowCap = 100

type leaderboardRow struct {
	Rank       int
	Station    placeRef
	RegionID   int64
	RegionName string
	Orders     string // esi.FormatInt
	OpenValue  string // esi.FormatISK
	locationID int64
	ordersRaw  int64
	valueRaw   float64
}

type leaderboardView struct {
	RegionID int64  // 0 = all regions
	Sort     string // "value" (default) or "orders"
	Regions  []marketRegion
	HasData  bool
	AsOf     string // "Figures last gathered Oct 5, 1:04 PM"
	Rows     []leaderboardRow
}

func (app *Application) handleMarketLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	view := app.buildLeaderboardView(ctx, r.URL.Query())
	if wantCSV(r) {
		serveLeaderboardCSV(w, view)
		return
	}
	data.MarketLeaderboard = view
	app.render(ctx, w, http.StatusOK, "market_leaderboard.html", data)
}

// serveLeaderboardCSV writes the leaderboard's station rows as a CSV download.
func serveLeaderboardCSV(w http.ResponseWriter, view *leaderboardView) {
	header := []string{"Rank", "Station", "Region", "Open orders", "ISK on orders"}
	rows := make([][]string, 0, len(view.Rows))
	for _, row := range view.Rows {
		rows = append(rows, []string{
			fmt.Sprintf("%d", row.Rank),
			row.Station.Name,
			row.RegionName,
			row.Orders,
			row.OpenValue,
		})
	}
	serveCSV(w, "top-stations", header, rows)
}

func (app *Application) buildLeaderboardView(ctx context.Context, q url.Values) *leaderboardView {
	view := &leaderboardView{Sort: "value"}
	if q.Get("sort") == "orders" {
		view.Sort = "orders"
	}
	if regionID, err := strconv.ParseInt(q.Get("region"), 10, 64); err == nil && regionID != 0 {
		if _, ok := marketRegionName(regionID); ok {
			view.RegionID = regionID
		}
	}
	regions := []marketRegion{{ID: 0, Name: "All regions", Active: view.RegionID == 0}}
	for _, region := range marketRegions {
		regions = append(regions, marketRegion{ID: region.ID, Name: region.Name, Active: view.RegionID == region.ID})
	}
	view.Regions = regions

	rows, err := app.queries.ListMarketStationLeaderboard(ctx, view.RegionID)
	if err != nil {
		log.Printf("leaderboard: list rows for region %d: %v", view.RegionID, err)
		return view
	}
	stationMemo := make(map[int64]placeRef)
	for _, row := range rows {
		regionName, ok := marketRegionName(row.RegionID)
		if !ok {
			continue
		}
		view.Rows = append(view.Rows, leaderboardRow{
			Station:    app.scannerStationRef(ctx, stationMemo, row.LocationID),
			RegionID:   row.RegionID,
			RegionName: regionName,
			Orders:     esi.FormatInt(row.SellOrders + row.BuyOrders),
			OpenValue:  esi.FormatISK(row.SellValue + row.BuyValue),
			locationID: row.LocationID,
			ordersRaw:  row.SellOrders + row.BuyOrders,
			valueRaw:   row.SellValue + row.BuyValue,
		})
	}
	if len(view.Rows) == 0 {
		return view
	}
	stamp, err := app.queries.GetMarketStationLeaderboardStamp(ctx, view.RegionID)
	if err != nil {
		log.Printf("leaderboard: stamp for region %d: %v", view.RegionID, err)
	} else if at, perr := time.Parse(time.RFC3339, stamp); perr != nil {
		log.Printf("leaderboard: unparseable stamp %q", stamp)
	} else {
		view.AsOf = "Figures last gathered " + at.Format("Jan 2, 3:04 PM")
	}
	view.HasData = true
	sort.Slice(view.Rows, func(i, j int) bool {
		a, b := view.Rows[i], view.Rows[j]
		if view.Sort == "orders" {
			if a.ordersRaw != b.ordersRaw {
				return a.ordersRaw > b.ordersRaw
			}
			if a.valueRaw != b.valueRaw {
				return a.valueRaw > b.valueRaw
			}
		} else {
			if a.valueRaw != b.valueRaw {
				return a.valueRaw > b.valueRaw
			}
			if a.ordersRaw != b.ordersRaw {
				return a.ordersRaw > b.ordersRaw
			}
		}
		return a.locationID < b.locationID
	})
	if len(view.Rows) > leaderboardRowCap {
		view.Rows = view.Rows[:leaderboardRowCap]
	}
	for i := range view.Rows {
		view.Rows[i].Rank = i + 1
	}
	return view
}
