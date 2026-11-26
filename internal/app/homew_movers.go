package app

import (
	"context"
	"math"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

func init() {
	regions := make([]string, len(marketRegions))
	for i, r := range marketRegions {
		regions[i] = strconv.FormatInt(r.ID, 10) + ":" + r.Name
	}
	registerHomeWidget(homeWidgetDef{
		ID: widgetMovers, Title: "Market Movers", Category: wcatMarket, Feature: "market_public",
		Href:        "/market/trends/",
		Description: "The items whose price moved most this week at a trade hub, and the most volatile.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The item that rose most this week and the one that fell most."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "The biggest movers, each with its change and 30-day line."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Risers and fallers side by side."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Risers, fallers and the most volatile side by side."},
		},
		Options: []widgetOption{
			optChoice("region", "Trade hub", strconv.FormatInt(defaultMarketRegion, 10), regions...),
			optChoice("show", "Show", "both", "both:Risers and fallers", "rising:Risers", "falling:Fallers", "volatile:Most volatile"),
		},
		Build: buildMoversWidget,
	})
}

// moversWidgetRows is the most items each list reads.
const moversWidgetRows = 10

// The Trends page's floors: a book with some depth and real trade, so
// a one-order swing never tops a list.
const (
	moversMinOrders = 5
	moversMinVolume = 10
)

// moverRow is one item on a movers list.
type moverRow struct {
	TypeID int64
	Name   string
	Href   string
	Price  string // the typical sell, short
	Change trendFigure
	Swing  string // the 30-day volatility, on the volatile list
	Line   string // the stored 30-day line: polyline points in a 100 by 28 box
	chg    float64
}

// moversWidgetView is what the Market Movers forms draw.
type moversWidgetView struct {
	Region   string
	Rising   []moverRow
	Falling  []moverRow
	Volatile []moverRow
	Moves    []moverRow // tall, risers and fallers: the biggest moves either way
	Show     string
	Note     string
	Empty    wEmpty
}

// Any reports whether a list has an item.
func (v *moversWidgetView) Any() bool {
	return len(v.Rising)+len(v.Falling)+len(v.Volatile) > 0
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
	v := &moversWidgetView{Region: region, Show: show,
		Empty: wEmpty{Text: "Price trends for " + region + " have not been worked out yet.", Step: "Open Market Trends", Href: "/market/trends/"}}
	read := func(mode string) []moverRow {
		rows, err := app.queries.ListMarketTrendTypes(ctx, db.ListMarketTrendTypesParams{
			RegionID: regionID, Mode: mode, Period: 7, MinOrders: moversMinOrders, MinVolume: moversMinVolume, RowCap: moversWidgetRows,
		})
		if err != nil {
			logging.Errorf("home: market movers (%s) in region %d: %v", mode, regionID, err)
			return nil
		}
		out := make([]moverRow, 0, len(rows))
		for _, r := range rows {
			row := moverRow{TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID),
				Href:  "/market/?type=" + strconv.FormatInt(r.TypeID, 10) + "&region=" + strconv.FormatInt(regionID, 10),
				Price: iskCompact(r.TypicalSell), Change: newTrendFigure(r.SellChg7d), Line: r.SellSpark30d}
			if r.SellChg7d.Valid {
				row.chg = math.Abs(r.SellChg7d.Float64)
			}
			if r.Volatility30d.Valid {
				row.Swing = strconv.FormatFloat(r.Volatility30d.Float64, 'f', 1, 64) + "%"
			}
			out = append(out, row)
		}
		return out
	}
	both := show == "both"
	if both || show == "rising" {
		v.Rising = read("rising")
	}
	if both || show == "falling" {
		v.Falling = read("falling")
	}
	if show == "volatile" || (both && in.Draws("large")) {
		v.Volatile = read("volatile")
	}
	if both && in.Draws("tall") {
		v.Moves = append(append(v.Moves, v.Rising...), v.Falling...)
		sort.SliceStable(v.Moves, func(i, j int) bool { return v.Moves[i].chg > v.Moves[j].chg })
	}
	v.Note = "Typical sell price in " + region + ", change over 7 days."
	return v
}
