package app

import (
	"context"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

func init() {
	regions := []string{"all:Every region"}
	for _, r := range marketRegions {
		regions = append(regions, strconv.FormatInt(r.ID, 10)+":"+r.Name)
	}
	registerHomeWidget(homeWidgetDef{
		ID: widgetWatchlist, Title: "Market Watchlist", Category: wcatMarket, Feature: "market_public",
		Href:        "/market/",
		Description: "The items you're watching: latest prices, 7- and 30-day moves, and a flag when one crosses your alert line.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The biggest mover and how many items are moving."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The biggest movers side by side, each with its 30-day line."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Each item with its price, 7-day change and 30-day line."},
			{Name: "medium", Sizes: "2x2 2x3 3x2 3x3", Says: "Each item with its region, price, 7- and 30-day changes, status and line."},
		},
		Options: []widgetOption{
			optChoice("order", "Order", "move", "move:Biggest move first", "name:By name"),
			optChoice("region", "Region", "all", regions...),
		},
		Build: buildWatchlistHome,
	})
}

// watchlistHomeRow is one watched item as the widget draws it: the
// Market page's row, its 30-day line for a column of its own, and its
// 7-day move for ordering.
type watchlistHomeRow struct {
	watchlistRow
	Line   template.HTML // the stored 30-day line drawn small (priceLineSVG); empty when none is stored
	Move   float64       // the 7-day change in percent; 0 when unknown
	Known  bool          // a 7-day change is known
	Priced bool          // a current price is stored
}

// watchlistHomeView is what the Watchlist forms draw.
type watchlistHomeView struct {
	Rows   []watchlistHomeRow
	Moving int // items past their alert line
	Lead   *watchlistHomeRow
	Empty  wEmpty
}

// watchlistWideMovers is how many movers the wide form sets side by
// side; the card shows the ones that fit.
const watchlistWideMovers = 3

func buildWatchlistHome(ctx context.Context, app *Application, in *widgetInput) any {
	shared := in.Shared.market
	v := &watchlistHomeView{}
	entries := shared.watchlist(ctx)
	if r := in.Opts.Get("region"); r != "all" {
		id, _ := strconv.ParseInt(r, 10, 64)
		var kept []db.MarketWatchlist
		for _, e := range entries {
			if e.RegionID == id {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if len(entries) == 0 {
		v.Empty = wEmpty{Text: "Nothing watched yet. Add items from the", Step: "Market page.", Href: "/market/"}
		if len(shared.watchlist(ctx)) > 0 {
			v.Empty.Text = "Nothing watched in this region yet. Add items from the"
		}
		return v
	}
	base := app.watchlistRows(ctx, entries, shared.watchMove, false)
	hover := app.vizPrefsFor(ctx, shared.userID).SparkHover
	keys := make([]sparkKey, len(base))
	for i, r := range base {
		keys[i] = sparkKey{RegionID: r.RegionID, TypeID: r.TypeID}
	}
	pairs := app.priceSparkPairs(ctx, keys)
	for i, r := range base {
		row := watchlistHomeRow{watchlistRow: r, Priced: r.Current != "—"}
		if m := shared.watchMove(ctx, entries[i]); m.Has7 {
			row.Move, row.Known = m.Chg7, true
		}
		if p, ok := pairs[keys[i]]; ok && r.Current != "—" {
			row.Line = p.Line
			if hover {
				row.Spark = p.Card
			}
		}
		if row.Moving {
			v.Moving++
		}
		v.Rows = append(v.Rows, row)
	}
	if in.Opts.Get("order") == "name" {
		sort.SliceStable(v.Rows, func(i, j int) bool { return strings.ToLower(v.Rows[i].Name) < strings.ToLower(v.Rows[j].Name) })
	} else {
		sort.SliceStable(v.Rows, func(i, j int) bool {
			if v.Rows[i].Known != v.Rows[j].Known {
				return v.Rows[i].Known
			}
			return math.Abs(v.Rows[i].Move) > math.Abs(v.Rows[j].Move)
		})
	}
	// The lead is the biggest move whatever the order.
	for i := range v.Rows {
		r := &v.Rows[i]
		if r.Known && (v.Lead == nil || math.Abs(r.Move) > math.Abs(v.Lead.Move)) {
			v.Lead = r
		}
	}
	if v.Lead == nil {
		v.Lead = &v.Rows[0]
	}
	return v
}

// Movers is the wide form's items: the biggest moves first.
func (v *watchlistHomeView) Movers() []watchlistHomeRow {
	out := append([]watchlistHomeRow(nil), v.Rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Known != out[j].Known {
			return out[i].Known
		}
		return math.Abs(out[i].Move) > math.Abs(out[j].Move)
	})
	if len(out) > watchlistWideMovers {
		out = out[:watchlistWideMovers]
	}
	return out
}

// Up reports whether the row's 7-day move is a rise.
func (r watchlistHomeRow) Up() bool { return r.Move > 0 }
