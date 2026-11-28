package leaderboard

// Station leaderboard: a completed region sweep distills one row per
// station into market_station_leaderboard (market_region_stats.go),
// counting only orders inside each item's Low90 to High90 price band
// toward open value. This page reads at most leaderboardRowCap rows in
// SQL with ORDER BY and LIMIT. "?sort=orders" ranks by open-order
// count; the default "value" ranks by ISK on orders. "?region=<id>"
// filters to one hub region; 0 or absent covers all five.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// leaderboardRowCap is the most stations the leaderboard
// ranks: a discovery list, not a full dump of every hub
// structure with an order on it.
const leaderboardRowCap = 100

type leaderboardRow struct {
	Rank       int
	Station    module.PlaceRef
	RegionID   int64
	RegionName string
	Orders     string // esi.FormatInt
	OpenValue  string // esi.FormatISK
	SharePct   string // the ranked figure against the top station's, 0 to 100
	ordersRaw  int64
	valueRaw   float64
}

type leaderboardView struct {
	RegionID int64  // 0 = all regions
	Sort     string // "value" (default) or "orders"
	Regions  []module.RegionChoice
	HasData  bool
	AsOf     string // "Figures last gathered Oct 5, 1:04 PM"
	Rows     []leaderboardRow
}

func (app *page) handleMarketLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)} // public: stored market data only
	view := app.buildLeaderboardView(ctx, r.URL.Query())
	if r.URL.Query().Get("format") == "csv" {
		app.serveLeaderboardCSV(w, view)
		return
	}
	data.MarketLeaderboard = view
	app.deps.Render(ctx, w, http.StatusOK, "market_leaderboard.html", data)
}

// serveLeaderboardCSV writes the leaderboard's station rows as a CSV download.
func (app *page) serveLeaderboardCSV(w http.ResponseWriter, view *leaderboardView) {
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
	app.deps.ServeCSV(w, "top-stations", header, rows)
}

func (app *page) buildLeaderboardView(ctx context.Context, q url.Values) *leaderboardView {
	view := &leaderboardView{Sort: "value"}
	if q.Get("sort") == "orders" {
		view.Sort = "orders"
	}
	if regionID, err := strconv.ParseInt(q.Get("region"), 10, 64); err == nil && regionID != 0 {
		if _, ok := app.deps.MarketRegionName(ctx, regionID); ok {
			view.RegionID = regionID
		}
	}
	regions := []module.RegionChoice{{ID: 0, Name: "All regions", Active: view.RegionID == 0}}
	for _, region := range app.deps.MarketRegions() {
		regions = append(regions, module.RegionChoice{ID: region.ID, Name: region.Name, Active: view.RegionID == region.ID})
	}
	view.Regions = regions

	var regionIDs []int64
	if view.RegionID != 0 {
		regionIDs = []int64{view.RegionID}
	} else {
		for _, region := range app.deps.MarketRegions() {
			regionIDs = append(regionIDs, region.ID)
		}
	}

	var rows []db.MarketStationLeaderboard
	var err error
	if view.Sort == "orders" {
		rows, err = app.queries.ListMarketStationLeaderboardByOrders(ctx, db.ListMarketStationLeaderboardByOrdersParams{
			RegionIds: regionIDs,
			RowLimit:  leaderboardRowCap,
		})
	} else {
		rows, err = app.queries.ListMarketStationLeaderboardByValue(ctx, db.ListMarketStationLeaderboardByValueParams{
			RegionIds: regionIDs,
			RowLimit:  leaderboardRowCap,
		})
	}
	if err != nil {
		logging.Errorf("leaderboard: list rows for region %d: %v", view.RegionID, err)
		return view
	}
	stationMemo := make(map[int64]module.PlaceRef)
	for _, row := range rows {
		regionName, ok := app.deps.MarketRegionName(ctx, row.RegionID)
		if !ok {
			continue
		}
		view.Rows = append(view.Rows, leaderboardRow{
			Station:    app.deps.StationRef(ctx, stationMemo, row.LocationID),
			RegionID:   row.RegionID,
			RegionName: regionName,
			Orders:     esi.FormatInt(row.OpenOrders),
			OpenValue:  esi.FormatISK(row.OpenValue),
			ordersRaw:  row.OpenOrders,
			valueRaw:   row.OpenValue,
		})
	}
	if len(view.Rows) == 0 {
		return view
	}
	stamp, err := app.queries.GetMarketStationLeaderboardStamp(ctx, regionIDs)
	if err != nil {
		logging.Errorf("leaderboard: stamp for region %d: %v", view.RegionID, err)
	} else if at, ok := stamp.(time.Time); ok {
		view.AsOf = "Figures last gathered " + at.UTC().Format("Jan 2, 3:04 PM")
	}
	view.HasData = true
	var top float64
	if len(view.Rows) > 0 {
		top = view.Rows[0].valueRaw
		if view.Sort == "orders" {
			top = float64(view.Rows[0].ordersRaw)
		}
	}
	for i := range view.Rows {
		view.Rows[i].Rank = i + 1
		v := view.Rows[i].valueRaw
		if view.Sort == "orders" {
			v = float64(view.Rows[i].ordersRaw)
		}
		pct := 0.0
		if top > 0 {
			pct = math.Round(v * 100 / top)
		}
		view.Rows[i].SharePct = strconv.FormatFloat(pct, 'f', 0, 64)
	}
	return view
}
