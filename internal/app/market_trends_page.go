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
	"net/url"
	"slices"
	"strconv"
	"time"

	"evesynapse/internal/app/contracts"
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
	SellShort   string // compact figures for the table (4.82M)
	BuyShort    string
	Orders      string
	TradedDay   string
	Chg1        trendFigure
	Chg7        trendFigure
	Chg30       trendFigure
	Volatility  string

	Rank     int
	IconURL  string
	Href     string // this item in the inspector, the view's filters kept
	On       bool
	SwingPct string // the daily swing against the view's widest, 0 to 100
	swing    float64
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
	// The change feed (mode "changes", schema 106) and its kind filter.
	Changes []marketChangeRow
	Kind    string
	Kinds   []marketTrendChoice

	// The framework console: the ranked items are its main pane and
	// ?type= opens one in the inspector (depth 2).
	ListHref  string
	ModeLabel string
	RankCol   string // the column the ranking sorts by: swing, chg1, chg7 or chg30
	Depth     int
	Insp      *marketTrendRow
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
	data.MarketTrends = app.buildMarketTrendsView(ctx, r.URL.Query()).withSelection(r.URL.Query().Get("type"))
	app.render(ctx, w, http.StatusOK, "market_trends.html", data)
}

var marketTrendsCache = newViewCache[marketTrendsView](30*time.Second, 128)

// buildMarketTrendsView ranks the region's stat pack for the query.
// The ranking scans the region's whole partition, and the view is a
// pure function of the query over stored rows, so a built view is
// reused for a few seconds.
func (app *Application) buildMarketTrendsView(ctx context.Context, q map[string][]string) *marketTrendsView {
	// The open item is not part of the ranking: the cache keys on
	// the ranking's own query, and the selection lands on a copy.
	vals := url.Values{}
	for k, v := range q {
		if k != "type" {
			vals[k] = v
		}
	}
	key := vals.Encode()
	if view, ok := marketTrendsCache.get(key); ok {
		return view
	}
	view := app.buildMarketTrendsViewFresh(ctx, q)
	if view.HasData {
		marketTrendsCache.put(key, view)
	}
	return view
}

func (app *Application) buildMarketTrendsViewFresh(ctx context.Context, q map[string][]string) *marketTrendsView {
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
	case "rising", "falling", "changes":
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
	for _, m := range []struct{ key, label string }{{"volatile", "Most volatile"}, {"rising", "Rising"}, {"falling", "Falling"}, {"changes", "What changed"}} {
		view.Modes = append(view.Modes, marketTrendChoice{Label: m.label, Href: href(m.key, view.Period), Active: m.key == view.Mode})
		if m.key == view.Mode {
			view.ModeLabel = m.label
		}
	}
	view.ListHref = href(view.Mode, view.Period)
	view.RankCol = "swing"
	if view.Mode == "rising" || view.Mode == "falling" {
		view.RankCol = "chg" + strconv.Itoa(view.Period)
	}
	for _, p := range []struct {
		days  int
		label string
	}{{1, "1 day"}, {7, "7 days"}, {30, "30 days"}} {
		view.Periods = append(view.Periods, marketTrendChoice{Label: p.label, Href: href(view.Mode, p.days), Active: p.days == view.Period})
	}

	if view.Mode == "changes" {
		for _, k := range marketChangeKinds {
			if get("kind") == k.key {
				view.Kind = k.key
			}
		}
		for _, k := range marketChangeKinds {
			h := href("changes", view.Period)
			if k.key != "" {
				h += "&kind=" + k.key
			}
			view.Kinds = append(view.Kinds, marketTrendChoice{Label: k.label, Href: h, Active: k.key == view.Kind})
		}
		view.HasData = true
		view.Changes = app.marketChangesForRegion(ctx, view.RegionID, view.Kind, view.Period)
		return view
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
	categories := map[int64]int64{}
	if len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.TypeID)
		}
		if catRows, err := app.queries.ListSDETypeCategories(ctx, ids); err == nil {
			for _, c := range catRows {
				categories[c.TypeID] = c.CategoryID
			}
		}
	}
	var widest float64
	for i, r := range rows {
		row := marketTrendRow{
			Rank: i + 1, IconURL: contracts.TypeCategoryIconURL(r.TypeID, categories[r.TypeID], 32),
			Href:   view.ListHref + "&type=" + strconv.FormatInt(r.TypeID, 10),
			TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID),
			TypicalSell: esi.FormatISK(r.TypicalSell), Orders: esi.FormatInt(r.SellOrders),
			TradedDay: esi.FormatInt(int64(math.Round(r.AvgDailyVolume30d))),
			Chg1:      newTrendFigure(r.SellChg1d), Chg7: newTrendFigure(r.SellChg7d), Chg30: newTrendFigure(r.SellChg30d),
		}
		row.SellShort = formatISKShort(r.TypicalSell)
		if r.TypicalBuy > 0 {
			row.TypicalBuy = esi.FormatISK(r.TypicalBuy)
			row.BuyShort = formatISKShort(r.TypicalBuy)
		}
		if r.Volatility30d.Valid {
			row.Volatility = strconv.FormatFloat(r.Volatility30d.Float64, 'f', 1, 64) + "%"
			row.swing = r.Volatility30d.Float64
			widest = max(widest, row.swing)
		}
		view.Rows = append(view.Rows, row)
	}
	for i := range view.Rows {
		pct := 0.0
		if widest > 0 {
			pct = math.Round(view.Rows[i].swing * 100 / widest)
		}
		view.Rows[i].SwingPct = strconv.FormatFloat(pct, 'f', 0, 64)
	}
	return view
}

// withSelection is the view with one ranked item open in the
// inspector: a copy, since the ranking itself is cached and shared.
func (v *marketTrendsView) withSelection(typeParam string) *marketTrendsView {
	out := *v
	out.Depth = 1
	id, err := strconv.ParseInt(typeParam, 10, 64)
	if err != nil || id <= 0 || len(v.Rows) == 0 {
		return &out
	}
	out.Rows = slices.Clone(v.Rows)
	for i := range out.Rows {
		if out.Rows[i].TypeID == id {
			out.Rows[i].On = true
			out.Insp = &out.Rows[i]
			out.Depth = 2
		}
	}
	return &out
}
