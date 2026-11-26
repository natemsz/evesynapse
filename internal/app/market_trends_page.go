package app

// The Trends screen (/market/trends/): volume-confirmed price movers,
// ISK-move and spread-profit rankings, raw % movers with manipulation
// warning columns, volatility leaders, and the filtered change feed.
// Every figure is precomputed at sweep and history ingest time on
// market_region_stats (schemas 101 and 188) and read in one bounded
// SQL query; the page never scores or filters full tables in Go.

import (
	"context"
	"html/template"
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

const (
	marketTrendsRowCap = 100

	moversSortConfirmed    = "confirmed"
	moversSortISKMove      = "isk_move"
	moversSortSpreadProfit = "spread_profit"
	moversSortRawPct       = "raw_pct"

	defaultMoversMinISKForge = 100_000_000
	defaultMoversMinISKHub   = 20_000_000
	defaultMoversMinTrades   = 10
)

type moversFloorChoice struct {
	Value string
	Label string
}

var moversSortChoices = []moversFloorChoice{
	{moversSortConfirmed, "Volume-confirmed move"},
	{moversSortISKMove, "ISK value of the move"},
	{moversSortSpreadProfit, "Daily spread profit"},
	{moversSortRawPct, "Raw % move (all gates off)"},
}

var moversISKChoices = []moversFloorChoice{
	{"auto", "Hub default (100M Forge, 20M other hubs)"},
	{"0", "Any (0 ISK)"},
	{"5000000", "5M ISK"},
	{"20000000", "20M ISK"},
	{"50000000", "50M ISK"},
	{"100000000", "100M ISK"},
	{"250000000", "250M ISK"},
	{"500000000", "500M ISK"},
	{"1000000000", "1B ISK"},
}

var moversTradeChoices = []moversFloorChoice{
	{"0", "Any (0 trades)"},
	{"5", "5 trades"},
	{"10", "10 trades"},
	{"25", "25 trades"},
	{"50", "50 trades"},
	{"100", "100 trades"},
}

func marketHubRegionIDs() []int64 {
	ids := make([]int64, len(marketRegions))
	for i, r := range marketRegions {
		ids[i] = r.ID
	}
	return ids
}

func defaultMoversMinISK(regionID int64) int64 {
	if regionID == defaultMarketRegion {
		return defaultMoversMinISKForge
	}
	return defaultMoversMinISKHub
}

func normalizeMoversSort(s string) string {
	switch s {
	case moversSortISKMove, moversSortSpreadProfit, moversSortRawPct:
		return s
	default:
		return moversSortConfirmed
	}
}

func moversSortLabel(s string) string {
	for _, c := range moversSortChoices {
		if c.Value == s {
			return c.Label
		}
	}
	return moversSortChoices[0].Label
}

func resolveMoversMinISK(raw string, regionID int64) (int64, string) {
	if raw == "" || raw == "auto" {
		return defaultMoversMinISK(regionID), "auto"
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
		return n, strconv.FormatInt(n, 10)
	}
	return defaultMoversMinISK(regionID), "auto"
}

func formatTradesPerDay(v float64) string {
	if v <= 0 {
		return "0"
	}
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return esi.FormatInt(int64(math.Round(v)))
}

func hubConfirmBadge(confirmedHubs int64) (label, title string) {
	if confirmedHubs <= 0 {
		return "", ""
	}
	if confirmedHubs == 1 {
		return "+1 hub", "Confirmed in 1 other trade hub"
	}
	n := strconv.FormatInt(confirmedHubs, 10)
	return "+" + n + " hubs", "Confirmed in " + n + " other trade hubs"
}

// marketTrendRow is one ranked type, display-ready.
type marketTrendRow struct {
	TypeID      int64
	Name        string
	TypicalSell string
	TypicalBuy  string
	SellShort   string // compact figures for the table (4.82M)
	SellAvg7    string // rolling 7-day average sell price
	// Line is the stored 30-day line for the trend column; Spark the
	// typical sell's hover card, live only where cards drop the column.
	Line          template.HTML
	Spark         template.HTML
	BuyShort      string
	Orders        string
	TradedDay     string
	ISKTradedDay  string
	TradesDay     string
	ISKMoveDay    string
	SpreadProfit  string
	SpreadPct     string
	HoldDays      int64
	Vol7d         string
	Vol30d        string
	Chg1          trendFigure
	Chg7          trendFigure // rolling 7d average change
	Chg7v30       trendFigure // 7d median vs 30d median move
	PrimaryChg7   trendFigure // what the 7-day column displays for the active sort
	Chg30         trendFigure
	Volatility    string
	ConfirmedHubs int64
	HubBadge      string
	HubTitle      string
	LowTrades     bool
	SpreadWidened bool
	SingleHub     bool

	Rank     int
	IconURL  string
	Href     string // this item in the inspector, the view's filters kept
	On       bool
	SwingPct string // the daily swing against the view's widest, 0 to 100
	swing    float64
}

// marketTrendChoice is one mode, period, kind, or select option.
type marketTrendChoice struct {
	Value, Label, Href string
	Active             bool
}

type marketTrendsView struct {
	RegionID      int64
	RegionName    string
	Regions       []marketRegionChoice
	Mode          string // rising | falling | volatile | changes
	Sort          string // confirmed | isk_move | spread_profit | raw_pct
	SortLabel     string
	Sorts         []marketTrendChoice
	Period        int // 1, 7 or 30
	MinISK        int64
	MinISKLabel   string
	MinISKParam   string
	MinISKChoices []marketTrendChoice
	MinTrades     int64
	RawMode       bool
	UseMedian7    bool // true when 7-day column shows 7d median vs 30d median
	Modes         []marketTrendChoice
	Periods       []marketTrendChoice
	HasData       bool // the region's stat pack carries trend figures
	Age           string
	Rows          []marketTrendRow
	// The change feed (mode "changes", schema 106) and its kind filter.
	Changes []marketChangeRow
	Kind    string
	Kinds   []marketTrendChoice

	// The framework console: the ranked items are its main pane and
	// ?type= opens one in the inspector (depth 2).
	ListHref  string
	ModeLabel string
	RankCol   string // column highlighted as the active ranking basis
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
// The view is a pure function of the query over stored rows, so a
// built view is reused briefly.
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
	view := &marketTrendsView{
		RegionID:  defaultMarketRegion,
		Mode:      "rising",
		Sort:      normalizeMoversSort(get("sort")),
		Period:    7,
		MinTrades: defaultMoversMinTrades,
	}
	if id, err := strconv.ParseInt(get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			view.RegionID = id
		}
	}
	view.RegionName, _ = marketRegionName(view.RegionID)
	switch get("mode") {
	case "rising", "falling", "volatile", "changes":
		view.Mode = get("mode")
	}
	switch get("period") {
	case "1":
		view.Period = 1
	case "30":
		view.Period = 30
	}

	view.MinISK, view.MinISKParam = resolveMoversMinISK(get("minisk"), view.RegionID)
	if v, err := strconv.ParseInt(get("mintrades"), 10, 64); err == nil && v >= 0 {
		view.MinTrades = v
	}
	view.RawMode = view.Sort == moversSortRawPct
	view.SortLabel = moversSortLabel(view.Sort)
	view.UseMedian7 = view.Sort == moversSortConfirmed || view.Sort == moversSortISKMove

	effectiveMinISK := view.MinISK
	effectiveMinTrades := view.MinTrades
	if view.RawMode {
		effectiveMinISK = 0
		effectiveMinTrades = 0
	}
	if effectiveMinISK == 0 {
		view.MinISKLabel = "0 ISK"
	} else {
		view.MinISKLabel = iskCompact(float64(effectiveMinISK)) + " ISK"
	}

	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegionChoice{ID: region.ID, Name: region.Name, Active: region.ID == view.RegionID})
	}
	for _, s := range moversSortChoices {
		view.Sorts = append(view.Sorts, marketTrendChoice{Value: s.Value, Label: s.Label, Active: s.Value == view.Sort})
	}
	matchedISK := false
	for _, c := range moversISKChoices {
		active := c.Value == view.MinISKParam
		if active {
			matchedISK = true
		}
		view.MinISKChoices = append(view.MinISKChoices, marketTrendChoice{Value: c.Value, Label: c.Label, Active: active})
	}
	if !matchedISK {
		view.MinISKChoices = append(view.MinISKChoices, marketTrendChoice{
			Value:  view.MinISKParam,
			Label:  iskCompact(float64(view.MinISK)) + " ISK",
			Active: true,
		})
	}

	href := func(mode string, period int) string {
		return "/market/trends/?region=" + strconv.FormatInt(view.RegionID, 10) +
			"&mode=" + mode +
			"&sort=" + view.Sort +
			"&period=" + strconv.Itoa(period) +
			"&minisk=" + view.MinISKParam +
			"&mintrades=" + strconv.FormatInt(view.MinTrades, 10)
	}
	for _, m := range []struct{ key, label string }{
		{"rising", "Rising"},
		{"falling", "Falling"},
		{"volatile", "Most volatile"},
		{"changes", "What changed"},
	} {
		view.Modes = append(view.Modes, marketTrendChoice{Label: m.label, Href: href(m.key, view.Period), Active: m.key == view.Mode})
		if m.key == view.Mode {
			view.ModeLabel = m.label
		}
	}
	view.ListHref = href(view.Mode, view.Period)

	switch view.Sort {
	case moversSortISKMove:
		view.RankCol = "iskmove"
	case moversSortSpreadProfit:
		view.RankCol = "profit"
	default:
		view.RankCol = "swing"
		if view.Mode == "rising" || view.Mode == "falling" {
			view.RankCol = "chg" + strconv.Itoa(view.Period)
		}
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
		view.Changes = app.marketChangesForRegionFiltered(ctx, view.RegionID, view.Kind, view.Period, float64(effectiveMinISK), float64(effectiveMinTrades))
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
		RegionID:  view.RegionID,
		Mode:      view.Mode,
		SortBy:    view.Sort,
		Period:    int32(view.Period),
		MinIsk:    float64(effectiveMinISK),
		MinTrades: float64(effectiveMinTrades),
		RowCap:    marketTrendsRowCap,
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
	sparkOn := app.sparksOn(ctx, app.userID(ctx))
	var widest float64
	for i, r := range rows {
		hubBadge, hubTitle := hubConfirmBadge(r.ConfirmedHubs)
		chg7 := newTrendFigure(r.SellChg7d)
		chg7v30 := newTrendFigure(r.MoverChg7v30)
		primary7 := chg7
		if view.UseMedian7 && chg7v30.Text != "" {
			primary7 = chg7v30
		}
		row := marketTrendRow{
			Rank:          i + 1,
			IconURL:       contracts.TypeCategoryIconURL(r.TypeID, categories[r.TypeID], 32),
			Href:          view.ListHref + "&type=" + strconv.FormatInt(r.TypeID, 10),
			TypeID:        r.TypeID,
			Name:          app.typeNameOrID(ctx, r.TypeID),
			TypicalSell:   esi.FormatISK(r.TypicalSell),
			SellShort:     iskCompact(r.TypicalSell),
			Orders:        esi.FormatInt(r.SellOrders),
			TradedDay:     esi.FormatInt(int64(math.Round(r.AvgDailyVolume30d))),
			ISKTradedDay:  iskCompact(r.IskTraded7d),
			TradesDay:     formatTradesPerDay(r.AvgOrders7d),
			ISKMoveDay:    iskCompact(r.IskMove7d),
			SpreadProfit:  iskCompact(r.DailySpreadProfit),
			HoldDays:      r.MoverHoldDays,
			Vol7d:         esi.FormatInt(int64(math.Round(r.AvgVol7d))),
			Vol30d:        esi.FormatInt(int64(math.Round(r.AvgVol30d))),
			Chg1:          newTrendFigure(r.SellChg1d),
			Chg7:          chg7,
			Chg7v30:       chg7v30,
			PrimaryChg7:   primary7,
			Chg30:         newTrendFigure(r.SellChg30d),
			ConfirmedHubs: r.ConfirmedHubs,
			HubBadge:      hubBadge,
			HubTitle:      hubTitle,
			LowTrades:     r.AvgOrders7d < float64(defaultMoversMinTrades),
			SpreadWidened: r.SpreadWidened,
			SingleHub:     r.ConfirmedHubs == 0,
		}
		if r.SellAvg7d > 0 {
			row.SellAvg7 = iskCompact(r.SellAvg7d)
		}
		if r.SpreadPct.Valid {
			row.SpreadPct = strconv.FormatFloat(r.SpreadPct.Float64, 'f', 1, 64) + "%"
		}
		// The stored 30-day line (schema 121): the trend column's
		// chart, and the typical sell's hover card where cards drop
		// that column.
		if r.SellSpark30d != "" {
			row.Line = template.HTML(`<svg class="tr-line ` + row.Chg30.Dir + `" viewBox="0 0 100 28" preserveAspectRatio="none" aria-hidden="true"><polyline points="` + template.HTMLEscapeString(r.SellSpark30d) + `"/></svg>`)
			if sparkOn {
				row.Spark = storedSparkCard(r.SellSpark30d, r.SellLow30d, r.SellHigh30d, r.SellMean30d, view.RegionName)
			}
		}
		if r.TypicalBuy > 0 {
			row.TypicalBuy = esi.FormatISK(r.TypicalBuy)
			row.BuyShort = iskCompact(r.TypicalBuy)
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
