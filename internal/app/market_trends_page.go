package app

// The Trends screen (/market/trends/): the most volatile types in a
// hub region, and the biggest risers and fallers of the typical sell
// over a day, a week or a month. Ranked in one bounded SQL read over
// the stat pack's trend figures (RefreshMarketStatPackTrends); the
// page never computes a change or a volatility of its own.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const marketTrendsRowCap = 100

// marketTrendRow is one ranked type, display-ready.
type marketTrendRow struct {
	TypeID      int64
	Name        string
	TypicalSell string
	TypicalBuy  string
	Orders      string
	TradedDay   string
	Chg1        trendFigure
	Chg7        trendFigure
	Chg30       trendFigure
	Volatility  string
}

// marketTrendChoice is one mode or period link.
type marketTrendChoice struct {
	Label, Href string
	Active      bool
}

type marketTrendsView struct {
	RegionID   int64
	RegionName string
	Regions    []marketRegionChoice
	Mode       string // volatile | rising | falling
	Period     int    // 1, 7 or 30 (risers and fallers)
	MinVolume  int64
	MinOrders  int64
	Modes      []marketTrendChoice
	Periods    []marketTrendChoice
	HasData    bool // the region's stat pack carries trend figures
	Age        string
	Rows       []marketTrendRow
}

// marketRegionChoice is a hub region as a filter shows it.
type marketRegionChoice struct {
	ID     int64
	Name   string
	Active bool
}

func (app *Application) handleMarketTrends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored market data only
	data.MarketTrends = app.buildMarketTrendsView(ctx, r.URL.Query())
	app.render(ctx, w, http.StatusOK, "market_trends.html", data)
}

func (app *Application) buildMarketTrendsView(ctx context.Context, q map[string][]string) *marketTrendsView {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	view := &marketTrendsView{RegionID: defaultMarketRegion, Mode: "volatile", Period: 7, MinVolume: 10, MinOrders: 5}
	if id, err := strconv.ParseInt(get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			view.RegionID = id
		}
	}
	view.RegionName, _ = marketRegionName(view.RegionID)
	switch get("mode") {
	case "rising", "falling":
		view.Mode = get("mode")
	}
	switch get("period") {
	case "1":
		view.Period = 1
	case "30":
		view.Period = 30
	}
	if v, err := strconv.ParseInt(get("minvol"), 10, 64); err == nil && v >= 0 {
		view.MinVolume = v
	}
	if v, err := strconv.ParseInt(get("minorders"), 10, 64); err == nil && v >= 1 {
		view.MinOrders = v
	}
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegionChoice{ID: region.ID, Name: region.Name, Active: region.ID == view.RegionID})
	}
	href := func(mode string, period int) string {
		return "/market/trends/?region=" + strconv.FormatInt(view.RegionID, 10) + "&mode=" + mode +
			"&period=" + strconv.Itoa(period) + "&minvol=" + strconv.FormatInt(view.MinVolume, 10) +
			"&minorders=" + strconv.FormatInt(view.MinOrders, 10)
	}
	for _, m := range []struct{ key, label string }{{"volatile", "Most volatile"}, {"rising", "Rising"}, {"falling", "Falling"}} {
		view.Modes = append(view.Modes, marketTrendChoice{Label: m.label, Href: href(m.key, view.Period), Active: m.key == view.Mode})
	}
	for _, p := range []struct {
		days  int
		label string
	}{{1, "1 day"}, {7, "7 days"}, {30, "30 days"}} {
		view.Periods = append(view.Periods, marketTrendChoice{Label: p.label, Href: href(view.Mode, p.days), Active: p.days == view.Period})
	}

	stamp, err := app.queries.GetMarketTrendStamp(ctx, view.RegionID)
	if err != nil {
		logging.Errorf("market trends: stamp for region %d: %v", view.RegionID, err)
		return view
	}
	view.HasData = stamp.WithChange > 0 || stamp.WithVolatility > 0
	if t, ok := stamp.Stamp.(time.Time); ok {
		view.Age = ageText(time.Now(), t)
	}
	if !view.HasData {
		return view
	}
	rows, err := app.queries.ListMarketTrendTypes(ctx, db.ListMarketTrendTypesParams{
		RegionID: view.RegionID, Mode: view.Mode, Period: int32(view.Period),
		MinOrders: view.MinOrders, MinVolume: float64(view.MinVolume), RowCap: marketTrendsRowCap,
	})
	if err != nil {
		logging.Errorf("market trends: list for region %d: %v", view.RegionID, err)
		return view
	}
	for _, r := range rows {
		row := marketTrendRow{
			TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID),
			TypicalSell: esi.FormatISK(r.TypicalSell), Orders: esi.FormatInt(r.SellOrders),
			TradedDay: esi.FormatInt(int64(math.Round(r.AvgDailyVolume30d))),
			Chg1:      newTrendFigure(r.SellChg1d), Chg7: newTrendFigure(r.SellChg7d), Chg30: newTrendFigure(r.SellChg30d),
		}
		if r.TypicalBuy > 0 {
			row.TypicalBuy = esi.FormatISK(r.TypicalBuy)
		}
		if r.Volatility30d.Valid {
			row.Volatility = strconv.FormatFloat(r.Volatility30d.Float64, 'f', 1, 64) + "%"
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}
