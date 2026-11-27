package app

import (
	"context"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

func init() {
	regions := make([]string, len(marketRegions))
	for i, r := range marketRegions {
		regions[i] = strconv.FormatInt(r.ID, 10) + ":" + r.Name
	}
	sorts := make([]string, len(moversSortChoices))
	for i, s := range moversSortChoices {
		sorts[i] = s.Value + ":" + s.Label
	}
	iskChoices := make([]string, len(moversISKChoices))
	for i, c := range moversISKChoices {
		iskChoices[i] = c.Value + ":" + c.Label
	}
	tradeChoices := make([]string, len(moversTradeChoices))
	for i, c := range moversTradeChoices {
		tradeChoices[i] = c.Value + ":" + c.Label
	}
	registerHomeWidget(homeWidgetDef{
		ID: widgetMovers, Title: "Market Movers", Category: wcatMarket, Feature: "market_public",
		Href:        "/market/trends/",
		Description: "Volume-confirmed price movers at a trade hub, with settings for ISK move, spread profit, or raw % swings.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The top riser and top faller at the chosen trade hub."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "The top movers, each with its change, hub confirmation, and 30-day line."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Risers and fallers side by side."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Risers, fallers and the most volatile side by side."},
		},
		Options: []widgetOption{
			optChoice("region", "Trade hub", strconv.FormatInt(defaultMarketRegion, 10), regions...),
			optChoice("show", "Show", "both", "both:Risers and fallers", "rising:Risers", "falling:Fallers", "volatile:Most volatile"),
			withHint(optChoice("sort", "Rank by", moversSortConfirmed, sorts...),
				"Volume-confirmed screens out thin spikes. Raw % move turns off all gates and shows warning signs."),
			withHint(optChoice("min_isk", "Minimum ISK traded/day", "auto", iskChoices...),
				"7-day average daily ISK traded from regional history. Ignored for Raw % move."),
			withHint(optChoice("min_trades", "Minimum trades/day", strconv.FormatInt(defaultMoversMinTrades, 10), tradeChoices...),
				"7-day average daily trade count from regional history. Ignored for Raw % move."),
		},
		Build: buildMoversWidget,
	})
}

// moversWidgetRows is the most items each list reads.
const moversWidgetRows = 10

// moverRow is one item on a movers list.
type moverRow struct {
	TypeID   int64
	Name     string
	Href     string
	Price    string // the typical sell, short
	Change   trendFigure
	Swing    string // the 30-day volatility, on the volatile list
	Line     string // the stored 30-day line: polyline points in a 100 by 28 box
	HubBadge string
	HubTitle string
	Meta     string // secondary figure (ISK traded/day, ISK move/day, spread profit/day, or swing)
	Warn     string // raw % warning signs (low trades, widening spread, 1 hub only)
}

// moversWidgetView is what the Market Movers forms draw.
type moversWidgetView struct {
	Region   string
	Rising   []moverRow
	Falling  []moverRow
	Volatile []moverRow
	Moves    []moverRow // tall, risers and fallers: pre-ranked either way
	Show     string
	Note     string
	Empty    wEmpty
}

// Any reports whether a list has an item.
func (v *moversWidgetView) Any() bool {
	return len(v.Rising)+len(v.Falling)+len(v.Volatile)+len(v.Moves) > 0
}

// Lists is how many lists the card draws.
func (v *moversWidgetView) Lists() int {
	n := 0
	for _, l := range [][]moverRow{v.Rising, v.Falling, v.Volatile} {
		if len(l) > 0 {
			n++
		}
	}
	return n
}

func buildMoversWidget(ctx context.Context, app *Application, in *widgetInput) any {
	regionID, _ := strconv.ParseInt(in.Opts.Get("region"), 10, 64)
	region, ok := marketRegionName(regionID)
	if !ok {
		regionID = defaultMarketRegion
		region, _ = marketRegionName(regionID)
	}
	show := in.Opts.Get("show")
	if show == "" {
		show = "both"
	}
	sortBy := normalizeMoversSort(in.Opts.Get("sort"))
	minISK, minISKParam := resolveMoversMinISK(in.Opts.Get("min_isk"), regionID)
	minTrades := int64(defaultMoversMinTrades)
	if raw := in.Opts.Get("min_trades"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			minTrades = n
		}
	}
	rawMode := sortBy == moversSortRawPct
	effectiveMinISK := minISK
	effectiveMinTrades := minTrades
	if rawMode {
		effectiveMinISK = 0
		effectiveMinTrades = 0
	}

	trendsHref := "/market/trends/?region=" + strconv.FormatInt(regionID, 10) +
		"&sort=" + sortBy + "&minisk=" + minISKParam + "&mintrades=" + strconv.FormatInt(minTrades, 10)
	v := &moversWidgetView{
		Region: region,
		Show:   show,
		Empty: wEmpty{
			Text: "No items in " + region + " cleared these filters yet. Adjust the ISK or trade-count floor in settings, or view all items on Market Trends.",
			Step: "Open Market Trends",
			Href: trendsHref,
		},
	}

	useMedian7 := sortBy == moversSortConfirmed || sortBy == moversSortISKMove
	read := func(mode string) []moverRow {
		period := int32(7)
		if mode == "volatile" {
			period = 0
		}
		rows, err := app.queries.ListRankedMarketMovers(ctx, db.ListRankedMarketMoversParams{
			RegionID:  regionID,
			SortBy:    sortBy,
			Mode:      mode,
			Period:    period,
			MinIsk:    float64(effectiveMinISK),
			MinTrades: float64(effectiveMinTrades),
			RowCap:    moversWidgetRows,
		})
		if err != nil {
			logging.Errorf("home: market movers (%s, %s) in region %d: %v", mode, sortBy, regionID, err)
			return nil
		}
		out := make([]moverRow, 0, len(rows))
		for _, r := range rows {
			chgFig := newTrendFigure(r.SellChg7d)
			if useMedian7 && r.MoverChg7v30.Valid {
				chgFig = newTrendFigure(r.MoverChg7v30)
			}
			hubBadge, hubTitle := hubConfirmBadge(r.ConfirmedHubs)
			row := moverRow{
				TypeID:   r.TypeID,
				Name:     app.typeNameOrID(ctx, r.TypeID),
				Href:     "/market/?type=" + strconv.FormatInt(r.TypeID, 10) + "&region=" + strconv.FormatInt(regionID, 10),
				Price:    iskCompact(r.TypicalSell),
				Change:   chgFig,
				Line:     r.SellSpark30d,
				HubBadge: hubBadge,
				HubTitle: hubTitle,
			}
			if r.Volatility30d.Valid {
				row.Swing = strconv.FormatFloat(r.Volatility30d.Float64, 'f', 1, 64) + "%"
			}
			switch sortBy {
			case moversSortConfirmed:
				row.Meta = iskCompact(r.IskTraded7d) + "/d"
			case moversSortISKMove:
				row.Meta = "move " + iskCompact(r.IskMove7d) + "/d"
			case moversSortSpreadProfit:
				row.Meta = "profit " + iskCompact(r.DailySpreadProfit) + "/d"
			default:
				var flags []string
				if r.AvgOrders7d < float64(defaultMoversMinTrades) {
					flags = append(flags, formatTradesPerDay(r.AvgOrders7d)+" trades/d")
				}
				if r.SpreadWidened {
					flags = append(flags, "widening spread")
				}
				if r.ConfirmedHubs == 0 {
					flags = append(flags, "1 hub only")
				}
				if len(flags) > 0 {
					row.Warn = strings.Join(flags, ", ")
				} else {
					row.Meta = iskCompact(r.IskTraded7d) + "/d"
				}
			}
			if mode == "volatile" && row.Swing != "" {
				if row.Meta != "" {
					row.Meta = "swings " + row.Swing + " · " + row.Meta
				} else {
					row.Meta = "swings " + row.Swing
				}
			}
			out = append(out, row)
		}
		return out
	}

	both := show == "both"
	drawsSplit := in.Draws("small") || in.Draws("medium") || in.Draws("large") || len(in.Forms) == 0
	if show == "rising" || (both && drawsSplit) {
		v.Rising = read("rising")
	}
	if show == "falling" || (both && drawsSplit) {
		v.Falling = read("falling")
	}
	if show == "volatile" || (both && in.Draws("large")) {
		v.Volatile = read("volatile")
	}
	if both && in.Draws("tall") {
		v.Moves = read("both")
	}

	switch sortBy {
	case moversSortConfirmed:
		v.Note = "Volume-confirmed 7d vs 30d moves in " + region + " (at least " + iskCompact(float64(effectiveMinISK)) + " ISK/d, " + strconv.FormatInt(effectiveMinTrades, 10) + " trades/d)."
	case moversSortISKMove:
		v.Note = "Ranked by daily ISK move in " + region + " (at least " + iskCompact(float64(effectiveMinISK)) + " ISK/d, " + strconv.FormatInt(effectiveMinTrades, 10) + " trades/d)."
	case moversSortSpreadProfit:
		v.Note = "Ranked by daily spread profit in " + region + " (at least " + iskCompact(float64(effectiveMinISK)) + " ISK/d, " + strconv.FormatInt(effectiveMinTrades, 10) + " trades/d)."
	default:
		v.Note = "Raw 7-day % moves in " + region + " (all gates off; warning signs shown)."
	}
	return v
}
