package app

// The item page's stat pack section: the region's stored book
// statistics, its trend figures and the daily book chart, all read
// from stored rows (the stat pack row and its daily copies).
// Nothing here computes a statistic; the sweep did.

import (
	"context"
	"database/sql"
	"encoding/json"
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

// bookDepthBar is one day's order count for one side of the book,
// as SVG geometry in the depth strip.
type bookDepthBar struct {
	X, Y, W, H string
	Side       string // "sell" or "buy"
	Title      string
}

// bookTrendChart is the daily typical sell and buy over the stored
// days, as SVG geometry. With Extended set (the account's
// book_depth switch), it also carries the daily best sell and buy
// as dashed lines, the band between them, a depth strip of the
// per-day order counts, and the daily spread percent series.
type bookTrendChart struct {
	Width, Height int
	SellPoints    string
	BuyPoints     string
	High, Low     string // axis labels, ISK
	First, Last   string // date labels
	Days          int

	Extended       bool
	BestSellPoints string
	BestBuyPoints  string
	BandPoints     string // polygon between the two best lines
	DepthWidth     int
	DepthHeight    int
	DepthBars      []bookDepthBar
	DepthMax       string // the depth strip's top count, formatted
	SpreadWidth    int
	SpreadHeight   int
	SpreadPoints   string
	SpreadHigh     string // axis labels, percent
	SpreadLow      string

	// The price plot's marks and words (drawn at its own aspect, like
	// the price history chart, so labels stay crisp): a point per day
	// on each typical line, a hover column per day for the site's chart
	// tip (every figure that day), value gridlines and date labels.
	SellDots   []bookDot
	BuyDots    []bookDot
	Hits       []pfHit
	PriceTicks []bookTick
	DateTicks  []bookTick
}

type bookDot struct{ X, Y float64 }

// bookTick is one axis label: its gridline's Y and label baseline for
// values, its X and text anchor for dates.
type bookTick struct {
	X, Y, LabelY  float64
	Label, Anchor string
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
	// The book chart's read window: with the account's book_depth
	// switch on, the chart set follows the item page's shared range
	// parameter; off, the read is the standing 90 days, exactly as
	// before.
	days := bookTrendDays
	extended := item.Viz.BookDepth
	if extended {
		_, days = vizRangeDays(item.ChartRange)
	}
	rows, err := app.queries.ListMarketRegionStatsDailyRecent(ctx, db.ListMarketRegionStatsDailyRecentParams{
		RegionID: own.RegionID, TypeID: item.TypeID,
		Since:    time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02"),
		RowLimit: int32(days),
	})
	if err != nil {
		logging.Errorf("market: book trend for type %d: %v", item.TypeID, err)
		return
	}
	view.Chart = buildBookChart(rows, extended)
}

// buildBookTrendChart shapes the daily rows into two polylines on a
// shared price scale; nil under two days.
func buildBookTrendChart(rows []db.ListMarketRegionStatsDailyRecentRow) *bookTrendChart {
	return buildBookChart(rows, false)
}

// buildBookChart is buildBookTrendChart plus, when extended, the
// best-price lines and band (the price scale widens to the daily
// bests so the band fits), the depth strip and the spread series.
func buildBookChart(rows []db.ListMarketRegionStatsDailyRecentRow, extended bool) *bookTrendChart {
	if len(rows) < 2 {
		return nil
	}
	const w, h, padTop, padBottom, padX = 720, 200, 10, 22, 6
	lo, hi := math.Inf(1), math.Inf(-1)
	consider := func(v float64) {
		if v > 0 {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	for _, r := range rows {
		consider(r.TypicalSell)
		consider(r.TypicalBuy)
		if extended {
			consider(r.BestSell)
			consider(r.BestBuy)
		}
	}
	if math.IsInf(lo, 0) {
		return nil
	}
	if hi == lo {
		hi, lo = hi*1.01+1, lo*0.99
	}
	n := len(rows)
	x := func(i int) float64 { return padX + float64(i)*float64(w-2*padX)/float64(n-1) }
	y := func(v float64) float64 { return padTop + (hi-v)/(hi-lo)*float64(h-padTop-padBottom) }
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
	chart := &bookTrendChart{
		Width: w, Height: h, Days: n,
		SellPoints: line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.TypicalSell }),
		BuyPoints:  line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.TypicalBuy }),
		High:       esi.FormatISK(hi), Low: esi.FormatISK(lo),
		First: rows[0].Day, Last: rows[n-1].Day,
		Extended: extended,
	}
	decorateBookChart(chart, rows, x, y, lo, hi, extended, w, h, padBottom)
	if !extended {
		return chart
	}
	chart.BestSellPoints = line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.BestSell })
	chart.BestBuyPoints = line(func(r db.ListMarketRegionStatsDailyRecentRow) float64 { return r.BestBuy })
	chart.BandPoints = bookBandPoints(rows, x, y)
	chart.DepthWidth, chart.DepthHeight, chart.DepthBars, chart.DepthMax = bookDepthStrip(rows)
	chart.SpreadWidth, chart.SpreadHeight, chart.SpreadPoints, chart.SpreadHigh, chart.SpreadLow = bookSpreadSeries(rows)
	return chart
}

// bookBandPoints is the polygon between the daily best sell and
// best buy lines: forward along the best sell, back along the best
// buy. Only days carrying both bests take part; under two such days
// there is no band.
func bookBandPoints(rows []db.ListMarketRegionStatsDailyRecentRow, x func(int) float64, y func(float64) float64) string {
	var top, bottom []string
	for i, r := range rows {
		if r.BestSell <= 0 || r.BestBuy <= 0 {
			continue
		}
		pt := func(v float64) string {
			return strconv.FormatFloat(x(i), 'f', 1, 64) + "," + strconv.FormatFloat(y(v), 'f', 1, 64)
		}
		top = append(top, pt(r.BestSell))
		bottom = append(bottom, pt(r.BestBuy))
	}
	if len(top) < 2 {
		return ""
	}
	for i, j := 0, len(bottom)-1; i < j; i, j = i+1, j-1 {
		bottom[i], bottom[j] = bottom[j], bottom[i]
	}
	return strings.Join(append(top, bottom...), " ")
}

// bookDepthStrip shapes the per-day order counts into paired bars,
// sell then buy for each day, on one shared count scale.
func bookDepthStrip(rows []db.ListMarketRegionStatsDailyRecentRow) (int, int, []bookDepthBar, string) {
	const w, h, pad = 600, 84, 6
	var maxCount int64
	for _, r := range rows {
		maxCount = max(maxCount, r.SellOrders, r.BuyOrders)
	}
	if maxCount <= 0 {
		return w, h, nil, ""
	}
	n := len(rows)
	slot := float64(w) / float64(n)
	barW := slot * 0.38
	base := float64(h - pad)
	plot := float64(h - 2*pad)
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	bars := make([]bookDepthBar, 0, 2*n)
	for i, r := range rows {
		bar := func(side string, count int64, xOff float64) bookDepthBar {
			bh := float64(count) / float64(maxCount) * plot
			return bookDepthBar{
				X: num(float64(i)*slot + xOff), Y: num(base - bh),
				W: num(barW), H: num(bh),
				Side:  side,
				Title: fmt.Sprintf("%s: %s %s orders", r.Day, esi.FormatInt(count), side),
			}
		}
		if r.SellOrders > 0 {
			bars = append(bars, bar("sell", r.SellOrders, slot*0.08))
		}
		if r.BuyOrders > 0 {
			bars = append(bars, bar("buy", r.BuyOrders, slot*0.54))
		}
	}
	return w, h, bars, esi.FormatInt(maxCount)
}

// bookSpreadSeries is the daily spread percent, (typical sell less
// typical buy) over typical buy, across the days carrying both
// typicals, drawn on its own scale.
func bookSpreadSeries(rows []db.ListMarketRegionStatsDailyRecentRow) (int, int, string, string, string) {
	const w, h, pad = 600, 64, 6
	vals := make([]float64, len(rows))
	ok := make([]bool, len(rows))
	lo, hi := math.Inf(1), math.Inf(-1)
	count := 0
	for i, r := range rows {
		if r.TypicalSell <= 0 || r.TypicalBuy <= 0 {
			continue
		}
		v := (r.TypicalSell - r.TypicalBuy) / r.TypicalBuy * 100
		vals[i], ok[i] = v, true
		lo, hi = math.Min(lo, v), math.Max(hi, v)
		count++
	}
	if count < 2 {
		return w, h, "", "", ""
	}
	// The plot scale spans at least two tenths of a point: a
	// series flatter than that is a flat line at the middle, not
	// float noise stretched over the whole strip. The labels keep
	// the true extremes.
	slo, shi := lo, hi
	if shi-slo < 0.2 {
		mid := (hi + lo) / 2
		shi, slo = mid+0.1, mid-0.1
	}
	n := len(rows)
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	var b strings.Builder
	for i := range rows {
		if !ok[i] {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(num(float64(i) * float64(w) / float64(n-1)))
		b.WriteByte(',')
		b.WriteString(num(pad + (shi-vals[i])/(shi-slo)*float64(h-2*pad)))
	}
	return w, h, b.String(), fmt.Sprintf("%.1f%%", hi), fmt.Sprintf("%.1f%%", lo)
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

// decorateBookChart adds the price plot's points, per-day hover
// columns (the site's chart tip: every figure that day), round value
// gridlines and first, middle and last date labels.
func decorateBookChart(c *bookTrendChart, rows []db.ListMarketRegionStatsDailyRecentRow, x func(int) float64, y func(float64) float64, lo, hi float64, extended bool, w, h, padBottom int) {
	n := len(rows)
	for i, r := range rows {
		xi := x(i)
		if r.TypicalSell > 0 {
			c.SellDots = append(c.SellDots, bookDot{X: xi, Y: y(r.TypicalSell)})
		}
		if r.TypicalBuy > 0 {
			c.BuyDots = append(c.BuyDots, bookDot{X: xi, Y: y(r.TypicalBuy)})
		}
		rowsTip := []pfTipRow{}
		if r.TypicalSell > 0 {
			rowsTip = append(rowsTip, pfTipRow{"Typical sell", esi.FormatISK(r.TypicalSell) + " ISK", "mt-swatch-sell"})
		}
		if r.TypicalBuy > 0 {
			rowsTip = append(rowsTip, pfTipRow{"Typical buy", esi.FormatISK(r.TypicalBuy) + " ISK", "mt-swatch-buy"})
		}
		if r.TypicalSell > 0 && r.TypicalBuy > 0 {
			rowsTip = append(rowsTip, pfTipRow{"Spread", fmt.Sprintf("%.1f%%", (r.TypicalSell-r.TypicalBuy)/r.TypicalBuy*100), "mt-swatch-spread"})
		}
		if extended {
			if r.BestSell > 0 {
				rowsTip = append(rowsTip, pfTipRow{"Best sell", esi.FormatISK(r.BestSell) + " ISK", "mt-swatch-best-sell"})
			}
			if r.BestBuy > 0 {
				rowsTip = append(rowsTip, pfTipRow{"Best buy", esi.FormatISK(r.BestBuy) + " ISK", "mt-swatch-best-buy"})
			}
		}
		rowsTip = append(rowsTip,
			pfTipRow{"Sell orders", esi.FormatInt(r.SellOrders) + " for " + esi.FormatInt(r.SellVolume) + " units", ""},
			pfTipRow{"Buy orders", esi.FormatInt(r.BuyOrders) + " for " + esi.FormatInt(r.BuyVolume) + " units", ""})
		cells := make([][3]string, len(rowsTip))
		for k, t := range rowsTip {
			cells[k] = [3]string{t.Label, t.Value, t.Class}
		}
		js, _ := json.Marshal(cells)
		left, right := 0.0, float64(w)
		if i > 0 {
			left = (x(i-1) + xi) / 2
		}
		if i < n-1 {
			right = (xi + x(i+1)) / 2
		}
		heading := r.Day
		if d, err := time.Parse("2006-01-02", r.Day); err == nil {
			heading = d.Format("Mon, Jan 2 2006")
		}
		c.Hits = append(c.Hits, pfHit{X: left, W: right - left, Date: heading, Label: heading, Rows: string(js)})
	}
	step := pfNiceStep((hi - lo) / 4)
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		yy := y(v)
		c.PriceTicks = append(c.PriceTicks, bookTick{Y: yy, LabelY: yy - 3, Label: pfShortISK(v)})
	}
	for k, i := range []int{0, n / 2, n - 1} {
		if k == 1 && (i == 0 || i == n-1) {
			continue
		}
		label := rows[i].Day
		if d, err := time.Parse("2006-01-02", rows[i].Day); err == nil {
			label = d.Format("Jan 2")
		}
		anchor := []string{"start", "middle", "end"}[k]
		c.DateTicks = append(c.DateTicks, bookTick{X: x(i), LabelY: float64(h - 6), Label: label, Anchor: anchor})
	}
}
