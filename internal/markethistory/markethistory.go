// Package markethistory turns a type's stored daily price history into
// what the market pages show: the change over a period, summary
// figures, a trader's snapshot, and the price-and-volume chart, which
// is laid out here and sent to the browser as finished SVG. Everything
// is a pure function over stored rows.
//
// The chart's dimensions and tick types are exported because the
// wallet's balance chart is drawn to the same measurements.
package markethistory

import (
	"fmt"
	"math"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Market price history — ESI's daily aggregates, stored
// typed in market_history and charted server-side. Everything in
// this file is a pure function over stored rows: change math,
// summary stats, and the hand-computed SVG chart (no chart
// library, no JavaScript — the page carries the finished SVG).
// ---------------------------------------------------------------------------

// DateLayout is the ESI history day format.
const DateLayout = "2006-01-02"

// ChartRows is how many recent stored rows a history read
// loads: the newest rows by date, not a calendar span, so a
// sparse item's whole recorded history stays visible instead of
// being cut off by an arbitrary date window.
const ChartRows = 90

// StaleAfterDays: when the newest recorded trade is older
// than this, the chart section captions the last trade date so it
// never implies the data is current.
const StaleAfterDays = 14

// The four states of the item view's history section, computed by
// attachHistory. The template renders a deliberate body for each;
// there is no fifth, silent state.
const (
	StatePending = "pending" // nothing fetched yet
	StateEmpty   = "empty"   // fetched: ESI has no trades
	StateFew     = "few"     // a few rows: summary, no chart
	StateChart   = "chart"   // enough rows for the chart
)

// ChangePct computes the percent change of a type's daily
// average between the latest stored day and the day `days` before
// it: (latest − base) / base × 100, where base is the stored day
// nearest the target date. The nearest day must sit within a
// tolerance window around the target (3 days for the 7-day move,
// 5 for the 30-day) — older aggregates have gaps, and bridging a
// two-week hole with a "7-day change" would be a lie. Too little
// history (one row, a zero base, or nothing near the target)
// reports ok=false and the UI shows "—".
func ChangePct(rows []db.MarketHistory, days int) (float64, bool) {
	if len(rows) < 2 {
		return 0, false
	}
	latest := rows[len(rows)-1]
	latestDate, err := time.Parse(DateLayout, latest.Date)
	if err != nil || latest.Average <= 0 {
		return 0, false
	}
	target := latestDate.AddDate(0, 0, -days)
	tolerance := 3 * 24 * time.Hour
	if days > 7 {
		tolerance = 5 * 24 * time.Hour
	}
	var base *db.MarketHistory
	var bestGap time.Duration
	for i := range rows[:len(rows)-1] {
		d, err := time.Parse(DateLayout, rows[i].Date)
		if err != nil || rows[i].Average <= 0 {
			continue
		}
		gap := d.Sub(target)
		if gap < 0 {
			gap = -gap
		}
		if base == nil || gap < bestGap {
			r := rows[i]
			base = &r
			bestGap = gap
		}
	}
	if base == nil || bestGap > tolerance {
		return 0, false
	}
	return (latest.Average - base.Average) / base.Average * 100, true
}

// Stats summarizes stored rows: min/max/mean of the daily
// average over the window, plus the latest day itself.
type Stats struct {
	Days    int
	Min     string // formatted ISK
	Max     string
	Mean    string
	Latest  string
	LastDay string // "2006-01-02"
}

// Sparkline renders stored daily averages (rows ascending, as
// the chart builder receives them) as points for the 100x28 KPI
// sparkline, plus the closed area polygon under the line. Rows
// without a usable average are skipped; fewer than two usable
// rows reports ok=false.
func Sparkline(rows []db.MarketHistory) (points, area string, ok bool) {
	vals := make([]float64, 0, len(rows))
	for _, r := range rows {
		if r.Average > 0 {
			vals = append(vals, r.Average)
		}
	}
	if len(vals) < 2 {
		return "", "", false
	}
	min, max := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	var b strings.Builder
	for i, v := range vals {
		x := float64(i) / float64(len(vals)-1) * 100
		y := 25.0
		if span > 0 {
			y = 25 - (v-min)/span*22
		}
		fmt.Fprintf(&b, "%.1f,%.1f ", x, y)
	}
	points = strings.TrimSpace(b.String())
	return points, points + " 100,28 0,28", true
}

func Summarize(rows []db.MarketHistory) (Stats, bool) {
	if len(rows) == 0 {
		return Stats{}, false
	}
	min, max := math.Inf(1), math.Inf(-1)
	var sum float64
	for _, r := range rows {
		if r.Average < min {
			min = r.Average
		}
		if r.Average > max {
			max = r.Average
		}
		sum += r.Average
	}
	latest := rows[len(rows)-1]
	return Stats{
		Days:    len(rows),
		Min:     esi.FormatISK(min),
		Max:     esi.FormatISK(max),
		Mean:    esi.FormatISK(sum / float64(len(rows))),
		Latest:  esi.FormatISK(latest.Average),
		LastDay: latest.Date,
	}, true
}

// FormatChangePct renders a computed change for display:
// "+8.2%" / "-3.0%".
func FormatChangePct(pct float64) string {
	return fmt.Sprintf("%+.1f%%", pct)
}

// ChangeDirection words a change the way the attention feed
// speaks it: "up 8.2%" / "down 3.0%".
func ChangeDirection(pct float64) string {
	if pct < 0 {
		return fmt.Sprintf("down %.1f%%", -pct)
	}
	return fmt.Sprintf("up %.1f%%", pct)
}

// ---------------------------------------------------------------------------
// The chart. Hand-rolled SVG: the daily-average polyline sits in
// the top band, daily volume bars rise from the baseline band
// below it (a twin axis sharing the x scale), and the price axis
// is labelled with the window's min/max. Everything scales from
// the rows, so flat data renders a midline, one day renders one
// dot, and zero-volume days simply have no bar.
// ---------------------------------------------------------------------------

const (
	ChartWidth     = 720
	ChartHeight    = 240
	ChartPadTop    = 12
	ChartPadBottom = 26 // room for the two date labels
	ChartPriceBand = 0.62
	ChartBarGap    = 10 // vertical gap between the two bands
)

// PriceChart is a fully-computed, template-ready SVG chart.
type PriceChart struct {
	Width       int
	Height      int
	Points      string // polyline points for the average-price line
	Dots        []Dot
	Bars        []Bar
	Recent      []Day // newest first, capped at RecentChartDays
	TopY        int   // y of the max-price line (price band top)
	BaseY       int   // y of the min-price line (price band bottom)
	MaxISK      string
	MinISK      string
	From        string // oldest date label
	To          string // newest date label
	PriceTicks  []AxisTick
	VolumeTicks []AxisTick
	OrderTicks  []AxisTick
	DateTicks   []DateTick

	// The ranged chart's extra layers (BuildPriceChartRange);
	// empty when a layer is off or the legacy builder ran.
	Wicks         []Wick // per-day lowest-to-highest lines
	MA5Points     string // polyline of the 5-day moving average
	MA20Points    string // polyline of the 20-day moving average
	ChannelPoints string // polygon of the trailing 5-day Donchian band
	OrdersPoints  string // polyline of order_count over the volume band
}

// Wick is one displayed day's price extent: a vertical line at X
// from Top (the day's highest) to Bottom (its lowest).
type Wick struct {
	X      int
	Top    int
	Bottom int
}

// ChartOptions selects the ranged chart's optional layers. The
// average line and the recent-days table always render; these
// three are the account's visualization switches.
type ChartOptions struct {
	Wicks       bool
	MAChannel   bool
	VolumeStrip bool
}

// AxisTick is one labelled gridline on a price or volume
// axis. Class carries the responsive tick role (the middle tick
// is dropped first on narrow screens).
type AxisTick struct {
	Label  string
	Y      int
	LabelY int
	Class  string
}

// DateTick is one x-axis date label, anchored so the first
// and last labels stay inside the chart.
type DateTick struct {
	Label  string
	X      int
	Anchor string
	Class  string
}

// RecentChartDays caps the recent-days table under the chart.
const RecentChartDays = 14

// Day is one stored day's figures, display-ready: the
// tooltip fields on a chart point and one row of the recent-days
// table under the chart (which doubles as the no-JavaScript path
// to the same numbers).
type Day struct {
	Date    string
	Average string // esi.FormatISK
	Highest string
	Lowest  string
	Volume  string // esi.FormatInt
	Orders  string // esi.FormatInt: orders that traded that day
	MA5     string // the 5-day average price, esi.FormatISK ("" before 5 days)
	MA20    string // the 20-day average price, as MA5
	Title   string // "<date>: average X ISK · high … · low … · volume …"
}

type Dot struct {
	X, Y int
	Day
}

type Bar struct {
	X, Y, W, H int
	Title      string // "<date>: volume N"
}

// FormatCompactAxisNumber renders an axis value in the short
// form a dense chart can carry: 950 stays 950, 12,345 becomes
// 12.3K, 3,400,000 becomes 3.4M. It labels scales only — the exact
// figures stay in the tooltips and the recent-days table.
func FormatCompactAxisNumber(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	abs := math.Abs(v)
	if abs < 1000 {
		if v == math.Trunc(v) {
			return fmt.Sprintf("%d", int64(v))
		}
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
	}
	units := []struct {
		scale  float64
		suffix string
	}{
		{1e12, "T"},
		{1e9, "B"},
		{1e6, "M"},
		{1e3, "K"},
	}
	for _, u := range units {
		if abs >= u.scale {
			scaled := v / u.scale
			if math.Abs(scaled) >= 100 {
				return fmt.Sprintf("%.0f%s", scaled, u.suffix)
			}
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", scaled), "0"), ".") + u.suffix
		}
	}
	return fmt.Sprintf("%.0f", v)
}

// BuildPriceChart turns stored daily aggregates into SVG
// geometry. ok=false only when there are no rows at all; a single
// row yields a lone dot (no polyline can be drawn through one
// point, and faking one would invent shape).
func BuildPriceChart(rows []db.MarketHistory) (PriceChart, bool) {
	return buildChart(rows, len(rows), ChartOptions{VolumeStrip: true}, true)
}

// BuildPriceChartRange builds the item page chart over the last
// `display` rows of rows (ascending). Rows before the displayed
// window are warmup: the moving averages and the Donchian channel
// read them, so their values are correct from the first displayed
// day, but they draw nothing themselves. The price scale spans
// the displayed days' lowest..highest (the wick extents); the
// averages, both moving averages and the band share that scale.
func BuildPriceChartRange(rows []db.MarketHistory, display int, opts ChartOptions) (PriceChart, bool) {
	return buildChart(rows, display, opts, false)
}

func buildChart(rows []db.MarketHistory, display int, opts ChartOptions, legacy bool) (PriceChart, bool) {
	if len(rows) == 0 {
		return PriceChart{}, false
	}
	if display < 1 || display > len(rows) {
		display = len(rows)
	}
	disp := rows[len(rows)-display:]
	chart := PriceChart{Width: ChartWidth, Height: ChartHeight}
	plotH := ChartHeight - ChartPadTop - ChartPadBottom
	priceH := plotH
	barH := 0
	if opts.VolumeStrip {
		priceH = int(float64(plotH-ChartBarGap) * ChartPriceBand)
		barH = plotH - ChartBarGap - priceH
	}
	priceTop := ChartPadTop
	priceBottom := priceTop + priceH
	barBottom := ChartHeight - ChartPadBottom
	chart.TopY = priceTop
	chart.BaseY = priceBottom

	minP, maxP := math.Inf(1), math.Inf(-1)
	var maxV int64
	var maxOrd int64
	for _, r := range disp {
		lo, hi := r.Average, r.Average
		if !legacy {
			lo, hi = r.Lowest, r.Highest
		}
		if lo < minP {
			minP = lo
		}
		if hi > maxP {
			maxP = hi
		}
		if r.Volume > maxV {
			maxV = r.Volume
		}
		if r.OrderCount > maxOrd {
			maxOrd = r.OrderCount
		}
	}
	// Flat windows (one distinct price, or one row) draw on a
	// padded scale so the line sits mid-band instead of pegged
	// to an edge.
	if maxP <= minP {
		pad := minP * 0.05
		if pad <= 0 {
			pad = 1
		}
		maxP, minP = maxP+pad, minP-pad
	}
	chart.MaxISK = esi.FormatISK(maxP)
	chart.MinISK = esi.FormatISK(minP)
	barTop := barBottom - barH
	priceTick := func(value float64, y int, class string) AxisTick {
		return AxisTick{
			Label:  FormatCompactAxisNumber(value) + " ISK",
			Y:      y,
			LabelY: y + 3,
			Class:  class,
		}
	}
	chart.PriceTicks = []AxisTick{
		priceTick(maxP, priceTop, ""),
		priceTick((minP+maxP)/2, priceTop+priceH/2, "tick-mid"),
		priceTick(minP, priceBottom, ""),
	}
	if opts.VolumeStrip {
		volumeTick := func(value float64, y int, class string) AxisTick {
			return AxisTick{
				Label:  FormatCompactAxisNumber(value),
				Y:      y,
				LabelY: y + 3,
				Class:  class,
			}
		}
		if maxV > 0 {
			chart.VolumeTicks = []AxisTick{
				volumeTick(float64(maxV), barTop, ""),
				volumeTick(float64(maxV)/2, barTop+barH/2, "tick-mid"),
				volumeTick(0, barBottom, ""),
			}
		} else {
			chart.VolumeTicks = []AxisTick{volumeTick(0, barBottom, "")}
		}
		if maxOrd > 0 {
			// The orders line gets its own scale on the opposite
			// side of the strip from the volume scale, so each
			// series reads against its own numbers. Order counts
			// are whole, so its labels round to whole numbers.
			orderTick := func(value float64, y int, class string) AxisTick {
				return AxisTick{
					Label:  FormatCompactAxisNumber(math.Round(value)),
					Y:      y,
					LabelY: y + 3,
					Class:  class,
				}
			}
			chart.OrderTicks = []AxisTick{
				orderTick(float64(maxOrd), barTop, ""),
				orderTick(float64(maxOrd)/2, barTop+barH/2, "tick-mid"),
				orderTick(0, barBottom, ""),
			}
		}
	}

	x := func(i int) int {
		if len(disp) == 1 {
			return ChartWidth / 2
		}
		return i * (ChartWidth - 1) / (len(disp) - 1)
	}
	y := func(avg float64) int {
		frac := (avg - minP) / (maxP - minP)
		return priceBottom - int(frac*float64(priceH))
	}

	prefix := make([]float64, len(rows)+1)
	for i, r := range rows {
		prefix[i+1] = prefix[i] + r.Average
	}
	movingAt := func(g, n int) string {
		if g+1 < n {
			return ""
		}
		return esi.FormatISK((prefix[g+1] - prefix[g+1-n]) / float64(n))
	}
	var pts []string
	for i, r := range disp {
		px, py := x(i), y(r.Average)
		day := dayFigures(r)
		g := len(rows) - display + i // global index of the day
		day.MA5, day.MA20 = movingAt(g, 5), movingAt(g, 20)
		chart.Dots = append(chart.Dots, Dot{X: px, Y: py, Day: day})
		pts = append(pts, fmt.Sprintf("%d,%d", px, py))
		if opts.VolumeStrip && maxV > 0 && r.Volume > 0 {
			h := int(float64(r.Volume) / float64(maxV) * float64(barH))
			if h < 1 {
				h = 1
			}
			chart.Bars = append(chart.Bars, Bar{
				X: px, Y: barBottom - h,
				W: barWidth(len(disp)), H: h,
				Title: fmt.Sprintf("%s: volume %s", r.Date, esi.FormatInt(r.Volume)),
			})
		}
	}
	if len(pts) > 1 {
		chart.Points = strings.Join(pts, " ")
	}
	if !legacy {
		start := len(rows) - display // global index of the first displayed day
		if opts.Wicks {
			for i, r := range disp {
				top, bottom := y(r.Highest), y(r.Lowest)
				if top > bottom {
					top, bottom = bottom, top
				}
				chart.Wicks = append(chart.Wicks, Wick{X: x(i), Top: top, Bottom: bottom})
			}
		}
		if opts.MAChannel {
			chart.MA5Points, chart.MA20Points = movingAverages(rows, start, x, y)
			chart.ChannelPoints = donchianBand(rows, start, x, y)
		}
		if opts.VolumeStrip && maxOrd > 0 {
			var ord []string
			for i, r := range disp {
				oy := barBottom - int(float64(r.OrderCount)/float64(maxOrd)*float64(barH))
				ord = append(ord, fmt.Sprintf("%d,%d", x(i), oy))
			}
			if len(ord) > 1 {
				chart.OrdersPoints = strings.Join(ord, " ")
			}
		}
	}
	// Recent days, newest first, for the table under the chart:
	// from every row read, warmup included, so the table keeps
	// its full span whatever range the chart displays.
	for i := len(rows) - 1; i >= 0 && len(chart.Recent) < RecentChartDays; i-- {
		chart.Recent = append(chart.Recent, dayFigures(rows[i]))
	}
	chart.From = disp[0].Date
	chart.To = disp[len(disp)-1].Date
	if len(disp) == 1 {
		chart.DateTicks = []DateTick{
			{Label: disp[0].Date, X: x(0), Anchor: "middle"},
		}
	} else {
		chart.DateTicks = []DateTick{
			{Label: disp[0].Date, X: x(0), Anchor: "start"},
		}
		if len(disp) > 2 {
			mid := len(disp) / 2
			chart.DateTicks = append(chart.DateTicks, DateTick{
				Label: disp[mid].Date, X: x(mid), Anchor: "middle", Class: "tick-mid",
			})
		}
		chart.DateTicks = append(chart.DateTicks, DateTick{
			Label: disp[len(disp)-1].Date, X: x(len(disp) - 1), Anchor: "end",
		})
	}
	return chart, true
}

// movingAverages computes the 5-day and 20-day moving averages of
// the daily average for the displayed days (global indexes from
// start), reading warmup rows before start so the first displayed
// day is already correct. A day contributes a point only once its
// full window exists; the polyline starts at the first such day.
func movingAverages(rows []db.MarketHistory, start int, x func(int) int, y func(float64) int) (ma5, ma20 string) {
	prefix := make([]float64, len(rows)+1)
	for i, r := range rows {
		prefix[i+1] = prefix[i] + r.Average
	}
	mean := func(g, n int) (float64, bool) {
		if g+1 < n {
			return 0, false
		}
		return (prefix[g+1] - prefix[g+1-n]) / float64(n), true
	}
	var p5, p20 []string
	for g := start; g < len(rows); g++ {
		i := g - start
		if v, ok := mean(g, 5); ok {
			p5 = append(p5, fmt.Sprintf("%d,%d", x(i), y(v)))
		}
		if v, ok := mean(g, 20); ok {
			p20 = append(p20, fmt.Sprintf("%d,%d", x(i), y(v)))
		}
	}
	if len(p5) > 1 {
		ma5 = strings.Join(p5, " ")
	}
	if len(p20) > 1 {
		ma20 = strings.Join(p20, " ")
	}
	return ma5, ma20
}

// donchianBand computes the trailing 5-day Donchian channel for
// the displayed days: for each day, the maximum of Highest and
// the minimum of Lowest over that day and the four stored days
// before it (warmup rows included). Returned as one polygon:
// the upper edge left to right, the lower edge back.
func donchianBand(rows []db.MarketHistory, start int, x func(int) int, y func(float64) int) string {
	var upper, lower []string
	for g := start; g < len(rows); g++ {
		lo, hi := math.Inf(1), math.Inf(-1)
		for k := g; k >= 0 && k > g-5; k-- {
			if rows[k].Lowest < lo {
				lo = rows[k].Lowest
			}
			if rows[k].Highest > hi {
				hi = rows[k].Highest
			}
		}
		i := g - start
		upper = append(upper, fmt.Sprintf("%d,%d", x(i), y(hi)))
		lower = append(lower, fmt.Sprintf("%d,%d", x(i), y(lo)))
	}
	if len(upper) < 2 {
		return ""
	}
	for i := len(lower) - 1; i >= 0; i-- {
		upper = append(upper, lower[i])
	}
	return strings.Join(upper, " ")
}

// dayFigures formats one stored row for the tooltip and the
// recent-days table.
func dayFigures(r db.MarketHistory) Day {
	avg := esi.FormatISK(r.Average)
	high := esi.FormatISK(r.Highest)
	low := esi.FormatISK(r.Lowest)
	vol := esi.FormatInt(r.Volume)
	return Day{
		Date:    r.Date,
		Average: avg,
		Highest: high,
		Lowest:  low,
		Volume:  vol,
		Orders:  esi.FormatInt(r.OrderCount),
		Title: fmt.Sprintf("%s: average %s ISK · high %s · low %s · volume %s",
			r.Date, avg, high, low, vol),
	}
}

// barWidth sizes volume bars to the row count: dense windows get
// hairlines, sparse ones get readable columns.
func barWidth(n int) int {
	if n <= 0 {
		return 1
	}
	w := ChartWidth / (2 * n)
	if w < 1 {
		return 1
	}
	if w > 10 {
		return 10
	}
	return w
}

// ---------------------------------------------------------------------------
// Trader snapshot: windowed averages and daily volume from the
// same stored rows the chart draws, plus the live book's margin
// when both sides exist. Everything derives from stored data —
// the only live inputs are the best buy/sell the item view
// already fetched — and whatever can't be computed renders as
// a dash, never an invented number.
// ---------------------------------------------------------------------------

// TraderStats is the item view's trading snapshot, display-ready.
// Empty fields render as "—".
type TraderStats struct {
	Avg7      string // mean daily average over the latest 7 recorded days
	Avg30     string // mean daily average over the latest 30 recorded days
	AvgVol7   string // mean daily volume over the latest 7 recorded days
	MarginPct string // best-buy → best-sell margin, % of the sell price
}

// Window means the daily average price and daily volume
// over the trailing `days` recorded days (the newest stored day
// and the days-1 days before it). Sparse histories simply have
// fewer rows in the window; ok=false only with no rows at all.
func Window(rows []db.MarketHistory, days int) (priceAvg, volAvg float64, ok bool) {
	if len(rows) == 0 || days < 1 {
		return 0, 0, false
	}
	latest, err := time.Parse(DateLayout, rows[len(rows)-1].Date)
	if err != nil {
		return 0, 0, false
	}
	cutoff := latest.AddDate(0, 0, -(days - 1))
	var priceSum, volSum float64
	var n int
	for _, r := range rows {
		d, err := time.Parse(DateLayout, r.Date)
		if err != nil || d.Before(cutoff) {
			continue
		}
		priceSum += r.Average
		volSum += float64(r.Volume)
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	return priceSum / float64(n), volSum / float64(n), true
}

// BuildTraderStats computes the trading snapshot from stored
// rows plus the live book's bests (0 when a side is absent).
// The margin is what buying at the best buy and selling at the
// best sell returns as a share of the sell price — before
// broker fees and sales tax, which the template says.
func BuildTraderStats(rows []db.MarketHistory, bestSell, bestBuy float64) *TraderStats {
	stats := &TraderStats{}
	if avg, _, ok := Window(rows, 7); ok {
		stats.Avg7 = esi.FormatISK(avg)
	}
	if avg, _, ok := Window(rows, 30); ok {
		stats.Avg30 = esi.FormatISK(avg)
	}
	if _, vol, ok := Window(rows, 7); ok {
		stats.AvgVol7 = esi.FormatInt(int64(math.Round(vol)))
	}
	if bestSell > 0 && bestBuy > 0 {
		stats.MarginPct = fmt.Sprintf("%.1f%%", (bestSell-bestBuy)/bestSell*100)
	}
	return stats
}
