package app

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

// historyDateLayout is the ESI history day format.
const historyDateLayout = "2006-01-02"

// historyChartDays is the chart window: the last 90 days of daily
// aggregates.
const historyChartDays = 90

// historyChartRows is how many recent stored rows a history read
// loads: the newest rows by date, not a calendar span, so a
// sparse item's whole recorded history stays visible instead of
// being cut off by an arbitrary date window.
const historyChartRows = 90

// historyStaleAfterDays: when the newest recorded trade is older
// than this, the chart section captions the last trade date so it
// never implies the data is current.
const historyStaleAfterDays = 14

// The four states of the item view's history section, computed by
// attachHistory. The template renders a deliberate body for each;
// there is no fifth, silent state.
const (
	historyStatePending = "pending" // nothing fetched yet
	historyStateEmpty   = "empty"   // fetched: ESI has no trades
	historyStateFew     = "few"     // a few rows: summary, no chart
	historyStateChart   = "chart"   // enough rows for the chart
)

// historyChangePct computes the percent change of a type's daily
// average between the latest stored day and the day `days` before
// it: (latest − base) / base × 100, where base is the stored day
// nearest the target date. The nearest day must sit within a
// tolerance window around the target (3 days for the 7-day move,
// 5 for the 30-day) — older aggregates have gaps, and bridging a
// two-week hole with a "7-day change" would be a lie. Too little
// history (one row, a zero base, or nothing near the target)
// reports ok=false and the UI shows "—".
func historyChangePct(rows []db.MarketHistory, days int) (float64, bool) {
	if len(rows) < 2 {
		return 0, false
	}
	latest := rows[len(rows)-1]
	latestDate, err := time.Parse(historyDateLayout, latest.Date)
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
		d, err := time.Parse(historyDateLayout, rows[i].Date)
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

// historyStats summarizes stored rows: min/max/mean of the daily
// average over the window, plus the latest day itself.
type historyStats struct {
	Days    int
	Min     string // formatted ISK
	Max     string
	Mean    string
	Latest  string
	LastDay string // "2006-01-02"
}

func summarizeHistory(rows []db.MarketHistory) (historyStats, bool) {
	if len(rows) == 0 {
		return historyStats{}, false
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
	return historyStats{
		Days:    len(rows),
		Min:     esi.FormatISK(min),
		Max:     esi.FormatISK(max),
		Mean:    esi.FormatISK(sum / float64(len(rows))),
		Latest:  esi.FormatISK(latest.Average),
		LastDay: latest.Date,
	}, true
}

// formatChangePct renders a computed change for display:
// "+8.2%" / "-3.0%".
func formatChangePct(pct float64) string {
	return fmt.Sprintf("%+.1f%%", pct)
}

// changeDirection words a change the way the attention feed
// speaks it: "up 8.2%" / "down 3.0%".
func changeDirection(pct float64) string {
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
	chartWidth     = 720
	chartHeight    = 240
	chartPadTop    = 12
	chartPadBottom = 26 // room for the two date labels
	chartPriceBand = 0.62
	chartBarGap    = 10 // vertical gap between the two bands
)

// priceChart is a fully-computed, template-ready SVG chart.
type priceChart struct {
	Width  int
	Height int
	Points string // polyline points for the average-price line
	Dots   []chartDot
	Bars   []chartBar
	TopY   int // y of the max-price line (price band top)
	BaseY  int // y of the min-price line (price band bottom)
	MaxISK string
	MinISK string
	From   string // oldest date label
	To     string // newest date label
}

type chartDot struct{ X, Y int }

type chartBar struct{ X, Y, W, H int }

// buildPriceChart turns stored daily aggregates into SVG
// geometry. ok=false only when there are no rows at all; a single
// row yields a lone dot (no polyline can be drawn through one
// point, and faking one would invent shape).
func buildPriceChart(rows []db.MarketHistory) (priceChart, bool) {
	if len(rows) == 0 {
		return priceChart{}, false
	}
	chart := priceChart{Width: chartWidth, Height: chartHeight}
	plotH := chartHeight - chartPadTop - chartPadBottom
	priceH := int(float64(plotH-chartBarGap) * chartPriceBand)
	barH := plotH - chartBarGap - priceH
	priceTop := chartPadTop
	priceBottom := priceTop + priceH
	barBottom := chartHeight - chartPadBottom
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

	x := func(i int) int {
		if len(rows) == 1 {
			return chartWidth / 2
		}
		return i * (chartWidth - 1) / (len(rows) - 1)
	}
	y := func(avg float64) int {
		frac := (avg - minP) / (maxP - minP)
		return priceBottom - int(frac*float64(priceH))
	}

	var pts []string
	for i, r := range rows {
		px, py := x(i), y(r.Average)
		chart.Dots = append(chart.Dots, chartDot{X: px, Y: py})
		pts = append(pts, fmt.Sprintf("%d,%d", px, py))
		if maxV > 0 && r.Volume > 0 {
			h := int(float64(r.Volume) / float64(maxV) * float64(barH))
			if h < 1 {
				h = 1
			}
			chart.Bars = append(chart.Bars, chartBar{
				X: px, Y: barBottom - h,
				W: barWidth(len(rows)), H: h,
			})
		}
	}
	if len(pts) > 1 {
		chart.Points = strings.Join(pts, " ")
	}
	chart.From = rows[0].Date
	chart.To = rows[len(rows)-1].Date
	return chart, true
}

// barWidth sizes volume bars to the row count: dense windows get
// hairlines, sparse ones get readable columns.
func barWidth(n int) int {
	if n <= 0 {
		return 1
	}
	w := chartWidth / (2 * n)
	if w < 1 {
		return 1
	}
	if w > 10 {
		return 10
	}
	return w
}
