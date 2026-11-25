package app

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Home widget charts: pure builders over bounded inputs.
//
// A chart is drawn in data units on an SVG with
// preserveAspectRatio="none", so it fills whatever box its card gives
// it; lines keep their width through vector-effect. Tick labels are
// HTML spans placed by percent (data-left, data-top), so text never
// stretches with the plot. Each chart carries hit columns for the
// shared chart tooltip (app.js reads data-date and data-tip-rows) and
// the rows of the visually hidden table every chart has as its twin.
// The partials in templates/widgets/_kit.html draw what these return.
// ---------------------------------------------------------------------------

// The data-unit box every line chart is drawn in.
const (
	wChartW = 1000.0
	wChartH = 1000.0
	// wChartMaxPoints caps the days a chart draws: a longer series
	// keeps every nth day and the last, so a year stays light.
	wChartMaxPoints = 92
)

// wTick is one axis label: its text and where it sits, in percent of
// the plot (from the top for values, from the left for dates).
type wTick struct {
	Label  string
	Pos    float64
	Anchor string // dates only: "start", "middle" or "end"
}

// wSeries is one drawn series. Slot is its categorical colour, 1 to 4
// (home.css --w-s1 to --w-s4); Area is the band under its line, down
// to the series below it or the baseline.
type wSeries struct {
	Name string
	Slot int
	Line string
	Area string
}

// wHit is one hover column: X and W in data units, the day it reads
// out, and the tooltip rows as JSON ([[name, value, swatch class]]).
type wHit struct {
	X, W  float64
	Date  string
	Label string
	Rows  string
}

// wTable is a chart's visually hidden twin.
type wTable struct {
	Caption string
	Head    []string
	Rows    [][]string
}

// wChart is a line or stacked area chart ready for the w-chart and
// w-spark partials.
type wChart struct {
	Label  string // the accessible name
	W, H   float64
	Series []wSeries
	Grid   []float64 // gridline heights, data units
	YTicks []wTick
	XTicks []wTick
	Hits   []wHit
	Table  wTable
}

// Legend is true when the chart draws two or more series: a single
// series is named by its card and needs no legend.
func (c *wChart) Legend() bool { return c != nil && len(c.Series) > 1 }

// wDay is one day's value of every layer a chart draws.
type wDay struct {
	At     time.Time
	Values []float64
}

// wLayer names one layer of a stacked chart.
type wLayer struct {
	Name string
	Slot int
}

// wThin keeps every nth day and the last, at most wChartMaxPoints.
func wThin(days []wDay) []wDay {
	if len(days) <= wChartMaxPoints {
		return days
	}
	stride := (len(days) + wChartMaxPoints - 1) / wChartMaxPoints
	out := make([]wDay, 0, wChartMaxPoints+1)
	for i, d := range days {
		if i%stride == 0 || i == len(days)-1 {
			out = append(out, d)
		}
	}
	return out
}

// wNiceStep rounds a raw tick step up to 1, 2, 2.5 or 5 times a power
// of ten.
func wNiceStep(raw float64) float64 {
	if raw <= 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

// wScale is a value axis from lo to hi with round ticks between them.
type wScale struct {
	lo, hi float64
	ticks  []float64
}

// wNiceScale spans lo..hi with about n round ticks; the span widens to
// whole steps, and a flat series gets room around it.
func wNiceScale(lo, hi float64, n int) wScale {
	if hi < lo {
		lo, hi = hi, lo
	}
	if hi == lo {
		pad := math.Abs(hi) * 0.05
		if pad == 0 {
			pad = 1
		}
		lo, hi = lo-pad, hi+pad
	}
	step := wNiceStep((hi - lo) / float64(n-1))
	s := wScale{lo: math.Floor(lo/step) * step, hi: math.Ceil(hi/step) * step}
	for v := s.lo; v <= s.hi+step/2; v += step {
		s.ticks = append(s.ticks, v)
	}
	return s
}

func (s wScale) y(v float64) float64 {
	return wChartH - (v-s.lo)/(s.hi-s.lo)*wChartH
}

// wX is the x of point i of n across the data-unit box.
func wX(i, n int) float64 {
	if n < 2 {
		return wChartW / 2
	}
	return float64(i) * wChartW / float64(n-1)
}

func wPt(x, y float64) string {
	return strconv.FormatFloat(x, 'f', 1, 64) + "," + strconv.FormatFloat(y, 'f', 1, 64)
}

// wAxisISK writes an axis value short: 0, 950K, 12.5B, 15B.
func wAxisISK(v float64) string {
	if v == 0 {
		return "0"
	}
	s := iskCompact(v)
	for _, unit := range []string{"K", "M", "B", "T"} {
		s = strings.Replace(s, ".0"+unit, unit, 1)
	}
	return s
}

// wDateLabel writes a day for an axis: "Nov 24", or "Nov 2026" on a
// chart longer than half a year.
func wDateLabel(t time.Time, long bool) string {
	if long {
		return t.Format("Jan 2006")
	}
	return t.Format("Jan 2")
}

// wStackedChart stacks the layers of each day from a zero baseline,
// the first layer at the bottom, so the top line is each day's total.
// The tooltip and the table list every layer and the total. ok is
// false under two days.
func wStackedChart(label, totalName string, layers []wLayer, days []wDay) (*wChart, bool) {
	if len(days) < 2 || len(layers) == 0 {
		return nil, false
	}
	days = wThin(days)
	top := 0.0
	for _, d := range days {
		sum := 0.0
		for _, v := range d.Values {
			sum += math.Max(v, 0)
		}
		top = math.Max(top, sum)
	}
	sc := wNiceScale(0, top, 4)
	c := &wChart{Label: label, W: wChartW, H: wChartH}
	c.Table = wTable{Caption: label, Head: []string{"Day"}}
	for _, l := range layers {
		c.Table.Head = append(c.Table.Head, l.Name)
	}
	c.Table.Head = append(c.Table.Head, totalName)

	n := len(days)
	lower := make([]float64, n) // each day's running stack height
	for li, l := range layers {
		var line, under []string
		for i, d := range days {
			v := 0.0
			if li < len(d.Values) {
				v = math.Max(d.Values[li], 0)
			}
			line = append(line, wPt(wX(i, n), sc.y(lower[i]+v)))
			under = append(under, wPt(wX(i, n), sc.y(lower[i])))
			lower[i] += v
		}
		area := append([]string{}, line...)
		for i := n - 1; i >= 0; i-- {
			area = append(area, under[i])
		}
		c.Series = append(c.Series, wSeries{Name: l.Name, Slot: l.Slot, Line: strings.Join(line, " "), Area: strings.Join(area, " ")})
	}
	for _, v := range sc.ticks {
		c.Grid = append(c.Grid, sc.y(v))
		c.YTicks = append(c.YTicks, wTick{Label: wAxisISK(v), Pos: sc.y(v) / wChartH * 100})
	}
	long := days[n-1].At.Sub(days[0].At) > 183*24*time.Hour
	c.XTicks = wDateTicks(days, long)
	half := wChartW / float64(n-1) / 2
	for i, d := range days {
		date := d.At.Format("2006-01-02")
		rows := make([][3]string, 0, len(layers)+1)
		row := []string{date}
		sum := 0.0
		for li, l := range layers {
			v := 0.0
			if li < len(d.Values) {
				v = d.Values[li]
			}
			sum += v
			rows = append(rows, [3]string{l.Name, iskCompact(v) + " ISK", "w-s" + strconv.Itoa(l.Slot)})
			row = append(row, esi.FormatISK(v))
		}
		rows = append(rows, [3]string{totalName, iskCompact(sum) + " ISK", ""})
		row = append(row, esi.FormatISK(sum))
		c.Table.Rows = append(c.Table.Rows, row)
		x := math.Max(wX(i, n)-half, 0)
		w := math.Min(wX(i, n)+half, wChartW) - x
		raw, _ := json.Marshal(rows)
		c.Hits = append(c.Hits, wHit{X: x, W: w, Date: date, Rows: string(raw),
			Label: fmt.Sprintf("%s: %s ISK in total", date, iskCompact(sum))})
	}
	return c, true
}

// wLineChart draws one series on a scale fitted to its own range (a
// line need not start at zero), with a light wash under it. ok is
// false under two days.
func wLineChart(label, name string, slot int, days []wDay) (*wChart, bool) {
	if len(days) < 2 {
		return nil, false
	}
	days = wThin(days)
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, d := range days {
		lo, hi = math.Min(lo, d.Values[0]), math.Max(hi, d.Values[0])
	}
	sc := wNiceScale(lo, hi, 3)
	n := len(days)
	c := &wChart{Label: label, W: wChartW, H: wChartH}
	c.Table = wTable{Caption: label, Head: []string{"Day", name}}
	var line []string
	for i, d := range days {
		line = append(line, wPt(wX(i, n), sc.y(d.Values[0])))
		c.Table.Rows = append(c.Table.Rows, []string{d.At.Format("2006-01-02"), esi.FormatISK(d.Values[0])})
	}
	area := append(append([]string{}, line...), wPt(wChartW, wChartH), wPt(0, wChartH))
	c.Series = []wSeries{{Name: name, Slot: slot, Line: strings.Join(line, " "), Area: strings.Join(area, " ")}}
	for _, v := range sc.ticks {
		c.Grid = append(c.Grid, sc.y(v))
		c.YTicks = append(c.YTicks, wTick{Label: wAxisISK(v), Pos: sc.y(v) / wChartH * 100})
	}
	c.XTicks = wDateTicks(days, days[n-1].At.Sub(days[0].At) > 183*24*time.Hour)
	half := wChartW / float64(n-1) / 2
	for i, d := range days {
		date := d.At.Format("2006-01-02")
		raw, _ := json.Marshal([][3]string{{name, iskCompact(d.Values[0]) + " ISK", "w-s" + strconv.Itoa(slot)}})
		x := math.Max(wX(i, n)-half, 0)
		c.Hits = append(c.Hits, wHit{X: x, W: math.Min(wX(i, n)+half, wChartW) - x, Date: date, Rows: string(raw),
			Label: date + ": " + iskCompact(d.Values[0]) + " ISK"})
	}
	return c, true
}

// wDateTicks labels the first, middle and last day.
func wDateTicks(days []wDay, long bool) []wTick {
	n := len(days)
	ticks := []wTick{{Label: wDateLabel(days[0].At, long), Pos: 0, Anchor: "start"}}
	if n > 2 {
		ticks = append(ticks, wTick{Label: wDateLabel(days[n/2].At, long), Pos: wX(n/2, n) / wChartW * 100, Anchor: "middle"})
	}
	return append(ticks, wTick{Label: wDateLabel(days[n-1].At, long), Pos: 100, Anchor: "end"})
}

// wPart is one part of a whole: its name, colour slot, value, the
// figure shown for it and the whole figure for its title.
type wPart struct {
	Name  string
	Slot  int
	Value float64
	Text  string
	Full  string
	Short string // when set, the key shows Short instead of Text on a narrow card ("102M" for "101,883,600.00")
}

// wSegment is a part with its share of the whole, in percent.
type wSegment struct {
	wPart
	Share     float64
	ShareText string
}

// wSegbar is one whole split into up to six parts, for the w-segbar
// partial: widths by share, a part at zero drawing nothing.
type wSegbar struct {
	Label string
	Parts []wSegment
}

// wSegments splits a whole into its parts. Parts past the sixth fold
// into the last; negative values count as zero. ok is false when the
// whole is not positive.
func wSegments(label string, parts []wPart) (*wSegbar, bool) {
	if len(parts) > 6 {
		rest := parts[5]
		rest.Name = "Other"
		for _, p := range parts[6:] {
			rest.Value += p.Value
		}
		rest.Text, rest.Full = iskCompact(rest.Value), esi.FormatISK(rest.Value)
		parts = append(parts[:5:5], rest)
	}
	total := 0.0
	for _, p := range parts {
		total += math.Max(p.Value, 0)
	}
	if total <= 0 {
		return nil, false
	}
	b := &wSegbar{Label: label}
	for _, p := range parts {
		share := math.Max(p.Value, 0) / total * 100
		b.Parts = append(b.Parts, wSegment{wPart: p, Share: share, ShareText: strconv.FormatFloat(share, 'f', 0, 64) + "%"})
	}
	return b, true
}

// wRankItem is one thing a ranked bar list compares: its name (as a
// link when it has a page), its value and the figures shown for it.
type wRankItem struct {
	Name  string
	Link  template.HTML // the name as a link; empty draws Name
	Value float64
	Text  string
	Full  string
}

// wRankRow is an item with its bar length, in percent of the largest.
type wRankRow struct {
	wRankItem
	Pct float64
}

// wRanked is a ranked bar list for the w-rank partial.
type wRanked struct {
	Label string
	Rows  []wRankRow
}

// wRankedBars lengthens each item's bar against the largest. Items
// arrive in their order, largest first; at most max are kept.
func wRankedBars(label string, items []wRankItem, max int) (*wRanked, bool) {
	if len(items) == 0 {
		return nil, false
	}
	if len(items) > max {
		items = items[:max]
	}
	top := 0.0
	for _, it := range items {
		top = math.Max(top, it.Value)
	}
	r := &wRanked{Label: label}
	for _, it := range items {
		pct := 0.0
		if top > 0 {
			pct = math.Max(it.Value, 0) / top * 100
		}
		r.Rows = append(r.Rows, wRankRow{wRankItem: it, Pct: pct})
	}
	return r, true
}

// ---------------------------------------------------------------------------
// Diverging bars: one bar per day from a zero line, above it for a gain
// and below it for a loss (Profit Pulse's kept profit). The same data
// units, grid, ticks, hover columns and table twin as the line charts;
// the w-bars and w-barspark partials draw it.
// ---------------------------------------------------------------------------

// wBar is one day's bar in data units; Down marks a value under zero.
type wBar struct {
	X, Y, W, H float64
	Down       bool
}

// wBars is a diverging bar chart: the chart's frame, its bars and the
// height of its zero line.
type wBars struct {
	wChart
	Bars  []wBar
	ZeroY float64
}

// wBarChart draws each day's first value as a bar from zero, on a
// scale that always holds zero. Days are never thinned (a bar is a
// day's whole figure): only the last wChartMaxPoints are drawn. ok is
// false under two days or when every day is zero.
func wBarChart(label, name string, days []wDay) (*wBars, bool) {
	if len(days) > wChartMaxPoints {
		days = days[len(days)-wChartMaxPoints:]
	}
	if len(days) < 2 {
		return nil, false
	}
	lo, hi := 0.0, 0.0
	for _, d := range days {
		lo, hi = math.Min(lo, d.Values[0]), math.Max(hi, d.Values[0])
	}
	if lo == 0 && hi == 0 {
		return nil, false
	}
	sc := wNiceScale(lo, hi, 3)
	n := len(days)
	b := &wBars{wChart: wChart{Label: label, W: wChartW, H: wChartH}, ZeroY: sc.y(0)}
	b.Table = wTable{Caption: label, Head: []string{"Day", name}}
	band := wChartW / float64(n)
	bw := band * 0.7
	for i, d := range days {
		v := d.Values[0]
		date := d.At.Format("2006-01-02")
		b.Table.Rows = append(b.Table.Rows, []string{date, esi.FormatISK(v)})
		tone := "w-up"
		if v < 0 {
			tone = "w-down"
		}
		if v != 0 {
			top, bot := math.Min(sc.y(v), b.ZeroY), math.Max(sc.y(v), b.ZeroY)
			b.Bars = append(b.Bars, wBar{X: float64(i)*band + (band-bw)/2, Y: top, W: bw, H: math.Max(bot-top, 1), Down: v < 0})
		}
		raw, _ := json.Marshal([][3]string{{name, iskCompact(v) + " ISK", tone}})
		b.Hits = append(b.Hits, wHit{X: float64(i) * band, W: band, Date: date, Rows: string(raw),
			Label: date + ": " + iskCompact(v) + " ISK"})
	}
	for _, v := range sc.ticks {
		b.Grid = append(b.Grid, sc.y(v))
		b.YTicks = append(b.YTicks, wTick{Label: wAxisISK(v), Pos: sc.y(v) / wChartH * 100})
	}
	b.XTicks = wDateTicks(days, days[n-1].At.Sub(days[0].At) > 183*24*time.Hour)
	return b, true
}
