package app

import (
	"evesynapse/internal/balancechart"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/markethistory"
)

// The Net worth chart shows more than the total. The sampler records,
// per character and day, the ISK balance and a net worth that adds
// priced assets and buy-order escrow, so each day's total splits into
// ISK on hand and everything else. The chart stacks the two as bands
// under the total line, on a scale that starts at zero so the bands are
// to scale.

// Theme colours for the bands and the line: the market chart's gold for
// ISK, the ember accent for assets, a lighter gold for the total.
const (
	netWorthISKColour    = "#ffb84d"
	netWorthAssetsColour = "#ff6a1a"
	netWorthTotalColour  = "#ffd27a"
)

// netWorthDay is one sampled day, summed across the user's characters.
type netWorthDay struct {
	At     time.Time
	ISK    float64 // wallet balances
	Assets float64 // total minus ISK: priced assets and buy-order escrow
}

func (d netWorthDay) Total() float64 { return d.ISK + d.Assets }

// netWorthBreakdownDays sums the sampler rows per day, ascending. A
// character contributes to a day only if it has a computed net worth that
// day (the same rule as the total-only series), and its balance joins the
// ISK part only then, so the two parts always add up to the total drawn.
func netWorthBreakdownDays(rows []db.WalletHistory) []netWorthDay {
	type sum struct{ isk, total float64 }
	byDay := make(map[string]*sum)
	for _, r := range rows {
		if !r.NetWorth.Valid {
			continue
		}
		s := byDay[r.Day]
		if s == nil {
			s = &sum{}
			byDay[r.Day] = s
		}
		s.isk += r.Balance
		s.total += r.NetWorth.Float64
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	out := make([]netWorthDay, 0, len(days))
	for _, d := range days {
		t, err := time.Parse(markethistory.DateLayout, d)
		if err != nil {
			continue
		}
		s := byDay[d]
		assets := s.total - s.isk
		if assets < 0 { // a balance read after the net worth was computed
			assets = 0
		}
		out = append(out, netWorthDay{At: t, ISK: s.isk, Assets: assets})
	}
	return out
}

// netWorthSparkTotals is the sparkline series over sampled history
// rows: each day's total net worth summed across the account's
// characters (the breakdown rule), ascending, capped at the last 31
// days. The KPI reads its series through the bounded grouped query
// (ListUserNetWorthDays); this shaping is the same math over rows.
func netWorthSparkTotals(rows []db.WalletHistory) []float64 {
	days := netWorthBreakdownDays(rows)
	totals := make([]float64, 0, len(days))
	for _, d := range days {
		totals = append(totals, d.Total())
	}
	if len(totals) > 31 {
		totals = totals[len(totals)-31:]
	}
	return totals
}

// The KPI sparkline's box: viewBox units, polyline points inset so
// the stroke is never clipped.
const (
	sparkWidth  = 100.0
	sparkHeight = 28.0
	sparkPad    = 2.0
)

// sparklineSVG shapes daily totals into an inline SVG sparkline: a
// polyline and its soft area fill, in viewBox coordinates. ok=false
// under two days — no line is drawn at all, never a placeholder.
func sparklineSVG(totals []float64) (line, area string, ok bool) {
	if len(totals) < 2 {
		return "", "", false
	}
	lo, hi := totals[0], totals[0]
	for _, v := range totals {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	span := hi - lo
	pts := make([]string, 0, len(totals))
	for i, v := range totals {
		x := sparkPad + float64(i)*(sparkWidth-2*sparkPad)/float64(len(totals)-1)
		y := sparkHeight - sparkPad
		if span > 0 {
			y -= (v - lo) / span * (sparkHeight - 2*sparkPad)
		} else {
			y = sparkHeight / 2
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	line = strings.Join(pts, " ")
	area = line + fmt.Sprintf(" %.1f,%.1f %.1f,%.1f", sparkWidth-sparkPad, sparkHeight, sparkPad, sparkHeight)
	return line, area, true
}

// buildNetWorthChart draws the stacked history: ISK from the baseline up,
// assets from there to the total, and the total as the line. ok=false
// under two days, like the single-series chart.
func buildNetWorthChart(days []netWorthDay) (balancechart.BalanceChart, bool) {
	if len(days) < 2 {
		return balancechart.BalanceChart{}, false
	}
	chart := balancechart.BalanceChart{
		Width: markethistory.ChartWidth, Height: markethistory.ChartHeight,
		Label:   "Net worth over time, split into ISK and assets",
		Stacked: true,
		Legend: []balancechart.ChartLegendItem{
			{Class: "legend-isk", Label: "ISK"},
			{Class: "legend-assets", Label: "Assets & orders"},
			{Class: "legend-total", Label: "Total"},
		},
		LegendNote: "Net worth in ISK",
	}
	plotH := markethistory.ChartHeight - markethistory.ChartPadTop - markethistory.ChartPadBottom
	top := markethistory.ChartPadTop
	bottom := top + plotH

	maxTotal := 0.0
	for _, d := range days {
		maxTotal = math.Max(maxTotal, d.Total())
	}
	if maxTotal <= 0 {
		maxTotal = 1
	}
	maxTotal *= 1.05 // headroom: the line should not sit on the frame
	tick := func(value float64, y int, class string) markethistory.AxisTick {
		return markethistory.AxisTick{
			Label: markethistory.FormatCompactAxisNumber(value) + " ISK", Y: y, LabelY: y + 3, Class: class,
		}
	}
	chart.Ticks = []markethistory.AxisTick{
		tick(maxTotal, top, ""),
		tick(maxTotal/2, top+plotH/2, "tick-mid"),
		tick(0, bottom, ""),
	}

	stride := 1
	if len(days) > balancechart.WalletChartMaxDots {
		stride = (len(days) + balancechart.WalletChartMaxDots - 1) / balancechart.WalletChartMaxDots
	}
	var kept []netWorthDay
	for i, d := range days {
		if i%stride == 0 || i == len(days)-1 {
			kept = append(kept, d)
		}
	}
	x := func(i int) int {
		if len(kept) == 1 {
			return markethistory.ChartWidth / 2
		}
		return balancechart.BalanceChartInset + i*(markethistory.ChartWidth-1-2*balancechart.BalanceChartInset)/(len(kept)-1)
	}
	y := func(v float64) int {
		return bottom - int(math.Round(v/maxTotal*float64(plotH)))
	}

	var line, iskTop, totalTop []string
	for i, d := range kept {
		px := x(i)
		iskTop = append(iskTop, fmt.Sprintf("%d,%d", px, y(d.ISK)))
		totalTop = append(totalTop, fmt.Sprintf("%d,%d", px, y(d.Total())))
		line = append(line, fmt.Sprintf("%d,%d", px, y(d.Total())))
		chart.Dots = append(chart.Dots, balancechart.BalanceDot{
			X: px, Y: y(d.Total()),
			Date:    d.At.Format(markethistory.DateLayout),
			Balance: esi.FormatISK(d.Total()),
			ISK:     esi.FormatISK(d.ISK),
			Assets:  esi.FormatISK(d.Assets),
			Title: fmt.Sprintf("%s: %s ISK in total — %s ISK on hand, %s ISK in assets and orders",
				d.At.Format(markethistory.DateLayout), esi.FormatISK(d.Total()), esi.FormatISK(d.ISK), esi.FormatISK(d.Assets)),
		})
	}
	chart.Points = strings.Join(line, " ")

	// Bands are closed polygons: the ISK band sits on the baseline; the
	// assets band runs along the ISK line and back along the total line.
	last := len(kept) - 1
	iskPoly := append(append([]string{}, iskTop...), fmt.Sprintf("%d,%d", x(last), bottom), fmt.Sprintf("%d,%d", x(0), bottom))
	assetsPoly := append([]string{}, totalTop...)
	for i := last; i >= 0; i-- {
		assetsPoly = append(assetsPoly, iskTop[i])
	}
	chart.Bands = []balancechart.BalanceBand{
		{Label: "ISK", Fill: netWorthISKColour, Opacity: "0.55", Points: strings.Join(iskPoly, " ")},
		{Label: "Assets and orders", Fill: netWorthAssetsColour, Opacity: "0.5", Points: strings.Join(assetsPoly, " ")},
	}

	chart.From = days[0].At.Format(markethistory.DateLayout)
	chart.To = days[len(days)-1].At.Format(markethistory.DateLayout)
	chart.DateTicks = []markethistory.DateTick{{Label: chart.From, X: x(0), Anchor: "start"}}
	if len(kept) > 2 {
		mid := len(kept) / 2
		chart.DateTicks = append(chart.DateTicks, markethistory.DateTick{
			Label: kept[mid].At.Format(markethistory.DateLayout), X: x(mid), Anchor: "middle", Class: "tick-mid",
		})
	}
	chart.DateTicks = append(chart.DateTicks, markethistory.DateTick{Label: chart.To, X: x(last), Anchor: "end"})
	return chart, true
}
