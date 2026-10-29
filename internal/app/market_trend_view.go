package app

// The item page's stat pack section: the region's stored book
// statistics, its trend figures and the daily book chart, all read
// from stored rows (the stat pack row and up to 90 of its daily
// copies). Nothing here computes a statistic; the sweep did.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// trendFigure is one formatted percent change with its direction
// ("up", "down" or "" for flat or unknown).
type trendFigure struct {
	Text string // "+8.2%", "" when there is no comparison day
	Dir  string
}

func newTrendFigure(v sql.NullFloat64) trendFigure {
	if !v.Valid || math.IsNaN(v.Float64) || math.IsInf(v.Float64, 0) {
		return trendFigure{}
	}
	f := trendFigure{Text: fmt.Sprintf("%+.1f%%", v.Float64)}
	switch rounded := math.Round(v.Float64*10) / 10; {
	case rounded > 0:
		f.Dir = "up"
	case rounded < 0:
		f.Dir = "down"
	}
	return f
}

// statSideRow is one side of the book as the statistics table shows it.
type statSideRow struct {
	Label                                  string
	Typical, Average, Best, Far, Edge, Dev string
	Orders, Volume                         string
	Has                                    bool
	Chg1, Chg7, Chg30                      trendFigure
}

// bookTrendChart is the daily typical sell and buy over up to 90
// stored days, as SVG geometry.
type bookTrendChart struct {
	Width, Height int
	SellPoints    string
	BuyPoints     string
	High, Low     string // axis labels, ISK
	First, Last   string // date labels
	Days          int
}

// statPackView is the item page's stat pack section.
type statPackView struct {
	HasData      bool
	Age          string
	Source       string        // "Global PLEX market" on a global-book row
	Sides        []statSideRow // sell side, then buy side
	Volatility   string        // "3.2% a day", "" under seven daily returns
	HistoryDays  int64
	TradedPerDay string // 30-day average traded volume, "" when none recorded
	Chart        *bookTrendChart
	Changes      []marketChangeRow // the change feed for this type and region (schema 106)
}

const bookTrendDays = 90

// attachStatPack fills the item's stat pack section for its region.
func (app *Application) attachStatPack(ctx context.Context, item *marketItem) {
	if item == nil {
		return
	}
	packs, err := app.loadStatPacks(ctx, item.RegionID, []int64{item.TypeID})
	if err != nil {
		logging.Errorf("market: stat pack for type %d: %v", item.TypeID, err)
		return
	}
	p := packs[item.TypeID]
	view := &statPackView{}
	item.Pack = view
	if p == nil || p.Own == nil {
		return
	}
	own := p.Own
	view.HasData = true
	view.Age = ageText(time.Now(), own.UpdatedAt)
	if p.FromGlobal {
		view.Source = "Global PLEX market"
	}
	sell := statSideRow{
		Label: "Sell orders", Has: own.SellOrders > 0,
		Typical: esi.FormatISK(own.TypicalSell), Average: esi.FormatISK(own.SellAvg),
		Best: esi.FormatISK(own.BestSell), Far: esi.FormatISK(own.SellMax),
		Edge: esi.FormatISK(own.SellP5), Dev: esi.FormatISK(own.SellStddev),
		Orders: esi.FormatInt(own.SellOrders), Volume: esi.FormatInt(own.SellVolume),
		Chg1: newTrendFigure(own.SellChg1d), Chg7: newTrendFigure(own.SellChg7d), Chg30: newTrendFigure(own.SellChg30d),
	}
	buy := statSideRow{
		Label: "Buy orders", Has: own.BuyOrders > 0,
		Typical: esi.FormatISK(own.TypicalBuy), Average: esi.FormatISK(own.BuyAvg),
		Best: esi.FormatISK(own.BestBuy), Far: esi.FormatISK(own.BuyMin),
		Edge: esi.FormatISK(own.BuyP5), Dev: esi.FormatISK(own.BuyStddev),
		Orders: esi.FormatInt(own.BuyOrders), Volume: esi.FormatInt(own.BuyVolume),
		Chg1: newTrendFigure(own.BuyChg1d), Chg7: newTrendFigure(own.BuyChg7d), Chg30: newTrendFigure(own.BuyChg30d),
	}
	view.Sides = []statSideRow{sell, buy}
	if own.Volatility30d.Valid {
		view.Volatility = fmt.Sprintf("%.1f%% a day", own.Volatility30d.Float64)
	}
	view.HistoryDays = own.HistoryDays
	if own.AvgDailyVolume30d > 0 {
		view.TradedPerDay = esi.FormatInt(int64(math.Round(own.AvgDailyVolume30d)))
	}
	rows, err := app.queries.ListMarketRegionStatsDailyRecent(ctx, db.ListMarketRegionStatsDailyRecentParams{
		RegionID: own.RegionID, TypeID: item.TypeID,
		Since: time.Now().UTC().AddDate(0, 0, -bookTrendDays).Format("2006-01-02"),
	})
	if err != nil {
		logging.Errorf("market: book trend for type %d: %v", item.TypeID, err)
		return
	}
	view.Chart = buildBookTrendChart(rows)
}

// buildBookTrendChart shapes the daily rows into two polylines on a
// shared price scale; nil under two days.
func buildBookTrendChart(rows []db.ListMarketRegionStatsDailyRecentRow) *bookTrendChart {
	if len(rows) < 2 {
		return nil
	}
	const w, h, pad = 600, 160, 8
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, r := range rows {
		for _, v := range []float64{r.TypicalSell, r.TypicalBuy} {
			if v > 0 {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	if math.IsInf(lo, 0) {
		return nil
	}
	if hi == lo {
		hi, lo = hi*1.01+1, lo*0.99
	}
	n := len(rows)
	x := func(i int) float64 { return float64(i) * float64(w) / float64(n-1) }
	y := func(v float64) float64 { return pad + (hi-v)/(hi-lo)*float64(h-2*pad) }
	line := func(pick func(db.ListMarketRegionStatsDailyRecentRow) float64) string {
		var b strings.Builder
		for i, r := range rows {
			v := pick(r)
			if v <= 0 {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(x(i), 'f', 1, 64))
			b.WriteByte(',')
			b.WriteString(strconv.FormatFloat(y(v), 'f', 1, 64))
		}
		return b.String()
	}
	return &bookTrendChart{
		Width: w, Height: h, Days: n,
		SellPoints: line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.TypicalSell }),
		BuyPoints:  line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.TypicalBuy }),
		High:       esi.FormatISK(hi), Low: esi.FormatISK(lo),
		First: rows[0].Day, Last: rows[n-1].Day,
	}
}

// allRegionsCap bounds the all-regions strip.
const allRegionsCap = 100

// attachAllRegionStats fills the regions strip with every region a
// stored row speaks for (our sweeps and the labeled fallback),
// cheapest typical sell first, regions with no sell orders last.
func (app *Application) attachAllRegionStats(ctx context.Context, item *marketItem) {
	own := make(map[int64]*db.MarketRegionStat)
	if rows, err := app.queries.ListMarketRegionStatsByType(ctx, item.TypeID); err != nil {
		logging.Errorf("market: region stats for type %d: %v", item.TypeID, err)
	} else {
		for i := range rows {
			if rows[i].RegionID != globalMarketRegion {
				own[rows[i].RegionID] = &rows[i]
			}
		}
	}
	fb := make(map[int64]*db.MarketStatsFallback)
	if rows, err := app.queries.ListMarketStatsFallbackByType(ctx, item.TypeID); err != nil {
		logging.Errorf("market: fallback stats for type %d: %v", item.TypeID, err)
	} else {
		for i := range rows {
			fb[rows[i].RegionID] = &rows[i]
		}
	}
	ids := make([]int64, 0, len(own)+len(fb))
	seen := map[int64]bool{}
	for id := range own {
		ids, seen[id] = append(ids, id), true
	}
	for id := range fb {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	names := map[int64]string{}
	if rows, err := app.queries.ListSDERegionNames(ctx, ids); err == nil {
		for _, r := range rows {
			names[r.RegionID] = r.Name
		}
	}
	type ranked struct {
		row  marketRegionStatRow
		sell float64
	}
	now := time.Now()
	var out []ranked
	for _, id := range ids {
		res, ok := resolvedRegionTypicals(own[id], fb[id], now)
		if !ok {
			continue
		}
		name := names[id]
		if name == "" {
			name = "Region " + strconv.FormatInt(id, 10)
		}
		_, hub := marketRegionName(id)
		row := marketRegionStatRow{
			RegionID: id, RegionName: name, HasData: true, Hub: hub,
			Active: id == item.RegionID, Age: ageText(now, res.AsOf),
		}
		if res.TypicalSell > 0 {
			row.TypicalSell = esi.FormatISK(res.TypicalSell)
		}
		if res.TypicalBuy > 0 {
			row.TypicalBuy = esi.FormatISK(res.TypicalBuy)
		}
		if res.Fallback {
			row.Source = fallbackSourceLabel
		} else if o := own[id]; o != nil {
			row.Chg7 = newTrendFigure(o.SellChg7d)
		}
		out = append(out, ranked{row: row, sell: res.TypicalSell})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.sell > 0) != (b.sell > 0) {
			return a.sell > 0
		}
		if a.sell != b.sell {
			return a.sell < b.sell
		}
		return a.row.RegionName < b.row.RegionName
	})
	if len(out) > allRegionsCap {
		out = out[:allRegionsCap]
	}
	item.RegionStats = item.RegionStats[:0]
	for _, r := range out {
		item.RegionStats = append(item.RegionStats, r.row)
	}
}
