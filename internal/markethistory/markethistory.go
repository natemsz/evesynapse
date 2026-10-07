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
// Phase 5: market price history — ESI's daily aggregates, stored
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
	DateTicks   []DateTick
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
// 12K, 3,400,000 becomes 3.4M. It labels scales only — the exact
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
	if len(rows) == 0 {
		return PriceChart{}, false
	}
	chart := PriceChart{Width: ChartWidth, Height: ChartHeight}
	plotH := ChartHeight - ChartPadTop - ChartPadBottom
	priceH := int(float64(plotH-ChartBarGap) * ChartPriceBand)
	barH := plotH - ChartBarGap - priceH
	priceTop := ChartPadTop
	priceBottom := priceTop + priceH
	barBottom := ChartHeight - ChartPadBottom
	chart.TopY = priceTop
	chart.BaseY = priceBottom

	minP, maxP := math.Inf(1), math.Inf(-1)
	var maxV int64
	for _, r := range rows {
		if r.Average < minP {
			minP = r.Average
		}
		if r.Average > maxP {
			maxP = r.Average
		}
		if r.Volume > maxV {
			maxV = r.Volume
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

	x := func(i int) int {
		if len(rows) == 1 {
			return ChartWidth / 2
		}
		return i * (ChartWidth - 1) / (len(rows) - 1)
	}
	y := func(avg float64) int {
		frac := (avg - minP) / (maxP - minP)
		return priceBottom - int(frac*float64(priceH))
	}

	var pts []string
	for i, r := range rows {
		px, py := x(i), y(r.Average)
		chart.Dots = append(chart.Dots, Dot{X: px, Y: py, Day: dayFigures(r)})
		pts = append(pts, fmt.Sprintf("%d,%d", px, py))
		if maxV > 0 && r.Volume > 0 {
			h := int(float64(r.Volume) / float64(maxV) * float64(barH))
			if h < 1 {
				h = 1
			}
			chart.Bars = append(chart.Bars, Bar{
				X: px, Y: barBottom - h,
				W: barWidth(len(rows)), H: h,
				Title: fmt.Sprintf("%s: volume %s", r.Date, esi.FormatInt(r.Volume)),
			})
		}
	}
	if len(pts) > 1 {
		chart.Points = strings.Join(pts, " ")
	}
	// Recent days, newest first, for the table under the chart.
	for i := len(rows) - 1; i >= 0 && len(chart.Recent) < RecentChartDays; i-- {
		chart.Recent = append(chart.Recent, dayFigures(rows[i]))
	}
	chart.From = rows[0].Date
	chart.To = rows[len(rows)-1].Date
	if len(rows) == 1 {
		chart.DateTicks = []DateTick{
			{Label: rows[0].Date, X: x(0), Anchor: "middle"},
		}
	} else {
		chart.DateTicks = []DateTick{
			{Label: rows[0].Date, X: x(0), Anchor: "start"},
		}
		if len(rows) > 2 {
			mid := len(rows) / 2
			chart.DateTicks = append(chart.DateTicks, DateTick{
				Label: rows[mid].Date, X: x(mid), Anchor: "middle", Class: "tick-mid",
			})
		}
		chart.DateTicks = append(chart.DateTicks, DateTick{
			Label: rows[len(rows)-1].Date, X: x(len(rows) - 1), Anchor: "end",
		})
	}
	return chart, true
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
// an honest dash, never an invented number.
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
