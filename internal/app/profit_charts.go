package app

// The Profit page's charts: server-rendered SVG geometry the
// template only places. Colours come from classes on theme
// tokens (the semantic --recycled, --kept, --lost and
// --unverified among them), words from HTML around the marks.
// The hero merges the old running-total panels into one: earned
// as a filled area, net worth change as a line over it, and the
// gap between them where ISK sits in stock and assets. Below it,
// a waterfall walks gross in down to net earned, and the trading
// efficiency panel stacks each bucket's sales into what the
// goods cost, what fees and tax took, and what was kept, with
// the 30-day rolling margin over it. A bucket in which sales
// have no known cost or tax line is hatched on every chart, and
// the margin line dashes there rather than pretending.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	pfChartW = 600
	pfChartH = 160
	pfPad    = 8
)

// pfChart is one chart's geometry and words.
type pfChart struct {
	Name, Title, Sub string // the plain name; the finding, with its number; what it covers
	Info             string // the explainer, carried by the info tip beside the name
	AxisL, AxisR     string // captions under the left and right scales, where a chart has them
	Label            string // aria-label
	W, H             int
	ZeroY            float64
	Bars             []pfBar
	Lines            []pfLine
	Areas            []pfLine // stacked bands, drawn under the lines
	Gaps             []pfBar
	Legend           []pfLegend
	Low, High        string
	First, Last      string
	HasGaps          bool
	ShowZeroLine     bool
	Hits             []pfHit    // one hover column per bucket, for the site's chart tip
	YTicks           []pfTick   // value gridlines and their labels
	YTicksR          []pfTick   // a second value scale, labelled on the right
	XTicks           []pfTick   // date labels under the plot
	Strip            []coverSeg // the coverage strip under the x-axis
}

// pfHit is one bucket's hover column: the tip's heading and its
// rows (JSON: [label, value, swatch class]) for app.js's chart
// tip. Bucket is the bucket's start day, the key the page's
// crosshair sync matches across panels.
type pfHit struct {
	X, W        float64
	Date, Label string
	Rows        string
	Bucket      string
}

// pfTick is one axis label: Y (plot units) for its gridline, Pos
// (a whole percent of the plot, a pf-y-N or pf-x-N class: the CSP
// allows no inline style) for the HTML label laid over it, so the
// words stay crisp however the plot stretches.
type pfTick struct {
	Y          float64
	Pos, Label string
	Class      string
}

// pfTipRow is one line of a chart tip.
type pfTipRow struct {
	Label, Value, Class string
}

type pfBar struct {
	X, Y, W, H float64
	Class, Tip string
}

type pfLine struct {
	Points, Class string
}

type pfLegend struct {
	Class, Label, Value string
}

// pfCharts is the page's chart set.
type pfCharts struct {
	Hero       *profitHero
	Waterfall  *pfWaterfall
	Efficiency *pfChart
	Bucket     string // "day" or "week"
}

// pfActivityClass names each activity's colour class; the same
// activity takes the same colour on every chart.
var pfActivityClass = map[string]string{
	actTrading: "pf-a-trading", actIndustry: "pf-a-industry", actBounties: "pf-a-bounties",
	actPI: "pf-a-pi", actContracts: "pf-a-contracts", actSales: "pf-a-sales", actLP: "pf-a-lp",
	actOther: "pf-a-other", actUpkeep: "pf-a-upkeep", actTransfers: "pf-a-transfers",
	actUnattributed: "pf-a-unattributed",
}

// ---------------------------------------------------------------------------
// Shared chart helpers: small multiples and readouts other pages'
// charts can adopt. They speak in buckets and values, never in
// profit-page concepts.
// ---------------------------------------------------------------------------

// rhythmSpark is one row of diverging spark-bars: a bucket's net
// above or below a zero line, heights on a scale shared with its
// sibling rows so the rows compare.
type rhythmSpark struct {
	W, H  int
	ZeroY float64
	Bars  []rhythmBar
}

type rhythmBar struct {
	X, Y, W, H float64
	Class      string
}

// buildRhythmSpark shapes per-bucket nets into spark-bars: up
// bars take the given series class, down bars the lost token.
func buildRhythmSpark(nets []float64, upClass string, maxAbs float64) *rhythmSpark {
	if len(nets) == 0 || maxAbs <= 0 {
		return nil
	}
	const w, h = 104, 24
	s := &rhythmSpark{W: w, H: h, ZeroY: h / 2}
	band := float64(w) / float64(len(nets))
	bw := math.Max(band*0.62, 1)
	for i, v := range nets {
		if v == 0 {
			continue
		}
		bh := math.Max(math.Abs(v)/maxAbs*(h/2-1), 0.75)
		bar := rhythmBar{X: float64(i)*band + (band-bw)/2, W: bw, H: bh, Class: upClass}
		if v > 0 {
			bar.Y = h/2 - bh
		} else {
			bar.Y, bar.Class = h/2, "sem-lost"
		}
		s.Bars = append(s.Bars, bar)
	}
	if len(s.Bars) == 0 {
		return nil
	}
	return s
}

// bulletBar is one turnover bullet: a track whose width is the
// gross the series moved, and a bright bar for the net it kept
// (the lost token when the net is negative).
type bulletBar struct {
	Label    string
	W, H     int
	TrackW   float64
	NetW     float64
	NetClass string // sem-kept | sem-lost
	Readout  string // the kept figure the mode asks for
	Moved    string // gross moved, short form
}

// buildBullets shapes the By activity totals into turnover
// bullets, widest mover first. Mode "pct" reads the readout as
// the retention share; anything else reads ISK kept.
func buildBullets(byAct map[string]db.ListProfitByActivityRow, mode string) []bulletBar {
	type kv struct {
		label        string
		income, cost float64
	}
	var rows []kv
	maxMoved := 0.0
	for _, l := range profitActivityLabels {
		a, ok := byAct[l.key]
		if !ok || a.Income+a.Costs <= 0 {
			continue
		}
		rows = append(rows, kv{l.label, a.Income, a.Costs})
		maxMoved = math.Max(maxMoved, a.Income+a.Costs)
	}
	if maxMoved <= 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].income+rows[i].cost > rows[j].income+rows[j].cost })
	out := make([]bulletBar, 0, len(rows))
	for _, r := range rows {
		moved, net := r.income+r.cost, r.income-r.cost
		b := bulletBar{Label: r.label, W: 200, H: 12, NetClass: "sem-kept", Moved: pfShortISK(moved)}
		b.TrackW = math.Max(moved/maxMoved*200, 3)
		b.NetW = math.Abs(net) / moved * b.TrackW
		if net < 0 {
			b.NetClass = "sem-lost"
		}
		if mode == "pct" {
			if r.income > 0 {
				b.Readout = fmt.Sprintf("%.1f%% kept", net/r.income*100)
			} else {
				b.Readout = "nothing in"
			}
		} else {
			b.Readout = signedISK(net)
		}
		out = append(out, b)
	}
	return out
}

// coverSeg is one segment of a coverage strip: a kept segment
// for a fully covered bucket, an unverified segment for one
// with sales lacking a cost, linked where it leads somewhere.
type coverSeg struct {
	Label string // title text
	Href  string // "" renders a span, not a link
	Class string // sem-kept | sem-unverified
}

// pfBuckets lists every bucket start from the first day to today.
func pfBuckets(fromDay string, unit string, now time.Time) []string {
	start, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		return nil
	}
	if unit == "week" {
		// Postgres weeks start on Monday.
		start = start.AddDate(0, 0, -((int(start.Weekday()) + 6) % 7))
	}
	var out []string
	for d := start; !d.After(now.UTC()); {
		out = append(out, d.Format("2006-01-02"))
		if unit == "week" {
			d = d.AddDate(0, 0, 7)
		} else {
			d = d.AddDate(0, 0, 1)
		}
	}
	return out
}

// pfScale maps a signed value range onto the plot height.
type pfScale struct{ lo, hi float64 }

func newPFScale(values ...float64) pfScale {
	s := pfScale{0, 0}
	for _, v := range values {
		s.lo, s.hi = math.Min(s.lo, v), math.Max(s.hi, v)
	}
	if s.hi == s.lo {
		s.hi = s.lo + 1
	}
	return s
}

func (s pfScale) y(v float64) float64 {
	return pfPad + (s.hi-v)/(s.hi-s.lo)*float64(pfChartH-2*pfPad)
}

func pfBand(n int) (band, width float64) {
	band = float64(pfChartW) / float64(max(n, 1))
	return band, math.Max(band*0.7, 1)
}

// pfBar spans a value from base to base+v on a scale.
func (s pfScale) bar(x, w, base, v float64, class, tip string) pfBar {
	y0, y1 := s.y(base), s.y(base+v)
	return pfBar{X: x, Y: math.Min(y0, y1), W: w, H: math.Max(math.Abs(y1-y0), 0.5), Class: class, Tip: tip}
}

func pfPoints(xs, ys []float64) string {
	parts := make([]string, len(xs))
	for i := range xs {
		parts[i] = strconv.FormatFloat(xs[i], 'f', 1, 64) + "," + strconv.FormatFloat(ys[i], 'f', 1, 64)
	}
	return strings.Join(parts, " ")
}

// pfBrokenLines draws a series as line segments, breaking at
// missing values (NaN) rather than drawing through them.
func pfBrokenLines(vs []float64, band float64, y func(float64) float64, class string) []pfLine {
	var out []pfLine
	var xs, ys []float64
	flush := func() {
		if len(xs) > 1 {
			out = append(out, pfLine{Points: pfPoints(xs, ys), Class: class})
		}
		xs, ys = nil, nil
	}
	for i, v := range vs {
		if math.IsNaN(v) {
			flush()
			continue
		}
		xs, ys = append(xs, float64(i)*band+band/2), append(ys, y(v))
	}
	flush()
	return out
}

// pfShortISK writes an axis amount short: "60M", "-130.5M",
// "2.5B".
func pfShortISK(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= u.div {
			return sign + strconv.FormatFloat(math.Round(v/u.div*100)/100, 'f', -1, 64) + u.suffix
		}
	}
	return sign + strconv.FormatFloat(math.Round(v), 'f', -1, 64)
}

// pfNiceStep rounds a raw step up to 1, 2 or 5 times a power of
// ten.
func pfNiceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*p {
			return m * p
		}
	}
	return 10 * p
}

// pfTicksFor labels four or five round values across a scale. y
// maps a value to plot units; h is the plot's height in those
// units.
func pfTicksFor(lo, hi float64, y func(float64) float64, h int, format func(float64) string) []pfTick {
	var out []pfTick
	step := pfNiceStep((hi - lo) / 4)
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		yy := y(v)
		out = append(out, pfTick{Y: yy, Pos: strconv.Itoa(int(math.Round(yy / float64(h) * 100))), Label: format(v)})
	}
	return out
}

// setYTicks labels the chart's left scale.
func (c *pfChart) setYTicks(lo, hi float64, y func(float64) float64, h int, format func(float64) string) {
	c.YTicks = pfTicksFor(lo, hi, y, h, format)
}

// setYTicksRight labels a second value scale on the plot's right
// edge (no gridlines: the left scale owns those).
func (c *pfChart) setYTicksRight(lo, hi float64, y func(float64) float64, h int, format func(float64) string) {
	c.YTicksR = pfTicksFor(lo, hi, y, h, format)
}

// pfXTicksFor labels the first, middle and last bucket by date.
func pfXTicksFor(buckets []string, band float64, w int) []pfTick {
	if len(buckets) == 0 {
		return nil
	}
	var out []pfTick
	idx := []int{0, len(buckets) / 2, len(buckets) - 1}
	classes := []string{"pf-x-start", "pf-x-mid", "pf-x-end"}
	for k, i := range idx {
		if k == 1 && (i == 0 || i == len(buckets)-1) {
			continue
		}
		label := buckets[i]
		if d, err := time.Parse("2006-01-02", buckets[i]); err == nil {
			label = d.Format("Jan 2")
		}
		x := float64(i)*band + band/2
		out = append(out, pfTick{Pos: strconv.Itoa(int(math.Round(x / float64(w) * 100))), Label: label, Class: classes[k]})
	}
	return out
}

// setXTicks labels the chart's buckets by date.
func (c *pfChart) setXTicks(buckets []string, band float64) {
	c.XTicks = pfXTicksFor(buckets, band, c.W)
}

// pfHitFor builds one bucket's hover column: the tip heading,
// its rows as JSON, and a row saying when the bucket's trading
// is partly covered.
func pfHitFor(b string, i int, band float64, label func(string) string, rows []pfTipRow, uncovered bool) pfHit {
	r := rows
	if uncovered {
		r = append(append([]pfTipRow{}, r...), pfTipRow{Label: "Partly covered", Value: "some sales lack a cost or tax line", Class: "pf-gap"})
	}
	cells := make([][3]string, len(r))
	for k, row := range r {
		cells[k] = [3]string{row.Label, row.Value, row.Class}
	}
	js, _ := json.Marshal(cells)
	heading := label(b)
	if d, err := time.Parse("2006-01-02", b); err == nil {
		heading = d.Format("Mon, Jan 2 2006")
		if strings.HasPrefix(label(b), "week") {
			heading = "Week of " + d.Format("Jan 2 2006")
		}
	}
	return pfHit{X: float64(i) * band, W: band, Date: heading, Label: heading, Rows: string(js), Bucket: b}
}

// setHits gives every bucket its hover column.
func (c *pfChart) setHits(buckets []string, band float64, label func(string) string, rows [][]pfTipRow, uncovered []bool) {
	for i, b := range buckets {
		unc := i < len(uncovered) && uncovered[i]
		c.Hits = append(c.Hits, pfHitFor(b, i, band, label, rows[i], unc))
	}
}

// ---------------------------------------------------------------------------
// The hero: one panel for earned and net worth.
// ---------------------------------------------------------------------------

// profitHero is the merged running-total panel: earned as a
// filled area (split three ways), net worth change as a line
// over it on a shared scale (or its own, indexed), the gap band
// between the curves, or, in daily mode, the diverging net by
// activity.
type profitHero struct {
	Name, Title, Sub string
	Label            string
	W, H             int
	Daily            bool
	Areas            []pfLine
	Lines            []pfLine
	GapQuads         []string // polygon points, one per bucket interval inside the gap
	Bars             []pfBar
	Gaps             []pfBar
	ZeroY            float64
	ShowZero         bool
	YTicks           []pfTick
	YTicksR          []pfTick
	XTicks           []pfTick
	Hits             []pfHit
	Legend           []pfLegend
	Ends             []heroEnd
	GapChip          string // "Open positions N in the gap", "" hides the chip
	Bucket           string
}

// heroEnd is one end label at a series' last point: its text and
// its vertical place as a pf-y-N percent class.
type heroEnd struct {
	Text, Pos, Class string
}

const pfHeroH = 176

// heroScale maps values onto the hero's taller plot.
type heroScale struct {
	lo, hi float64
	h      int
}

func newHeroScale(h int, values ...float64) heroScale {
	s := heroScale{0, 0, h}
	for _, v := range values {
		s.lo, s.hi = math.Min(s.lo, v), math.Max(s.hi, v)
	}
	if s.hi == s.lo {
		s.hi = s.lo + 1
	}
	return s
}

func (s heroScale) y(v float64) float64 {
	return pfPad + (s.hi-v)/(s.hi-s.lo)*float64(s.h-2*pfPad)
}

// ---------------------------------------------------------------------------
// The waterfall.
// ---------------------------------------------------------------------------

// pfWaterfall walks gross in down to net earned in floating
// steps. The step definitions are pinned to the figures the
// page already totals, so the steps sum to Earned exactly:
//
//   - Gross in: the sum of gross-in over the By activity table's
//     activities (the same profit_daily rows the table totals,
//     grouped by bucket; unattributed excluded: the remainder
//     is not an activity).
//   - Cost of goods sold: the trade fold's matched cost of
//     covered sales over the span (ListTradeCOGSBuckets cov_cost).
//   - Industry costs: the By activity table's industry costs
//     (delivered jobs' materials and the industry journal fees).
//   - Fees, taxes and other outflows: the residual, every other
//     cost in the table (sales tax and broker fees inside
//     trading, contract fees, running costs, the rest), defined
//     as gross in less the two named steps less Earned.
//   - Net earned: the page's Earned.
//
// The net bar's hatched cap is the span's uncovered sales total
// (sales revenue with no covered cost, from the same COGS
// buckets): part of the net whose cost is not verified.
type pfWaterfall struct {
	Name, Title, Sub string
	Info             string // the explainer, carried by the info tip beside the name
	Label            string
	W, H             int
	Bars             []pfWfBar
	Cap              *pfWfBar // the net bar's unverified cap, a link
	CapHref          string
	Conns            []pfWfConn
	Values           []pfWfLabel // value at each bar's head
	Names            []pfTick    // step names under the bars
	Hits             []pfHit
	Legend           []pfLegend
}

type pfWfBar struct {
	X, Y, W, H float64
	Class      string
}

type pfWfConn struct {
	X1, X2, Y float64
}

// pfWfLabel is one HTML label over the plot: XPos a pf-x-N
// percent class, YPos a pf-y-N percent class.
type pfWfLabel struct {
	Text, XPos, YPos string
}

// ---------------------------------------------------------------------------
// The chart set builder.
// ---------------------------------------------------------------------------

func (app *Application) buildProfitCharts(ctx context.Context, view *profitView, kind string, ids []int64, names map[int64]string, fromDay string, brokerInMargin bool) (*pfCharts, error) {
	now := time.Now().UTC()
	unit := view.Grain
	if unit != "day" && unit != "week" {
		unit = "day"
		if view.Days > 90 {
			unit = "week"
		}
	}
	buckets := pfBuckets(fromDay, unit, now)
	if len(buckets) < 2 {
		return nil, nil
	}
	index := make(map[string]int, len(buckets))
	for i, b := range buckets {
		index[b] = i
	}
	// For a week bucket, the bucket a day falls in.
	bucketOf := func(day string) (int, bool) {
		if unit == "day" {
			i, ok := index[day]
			return i, ok
		}
		d, err := time.Parse("2006-01-02", day)
		if err != nil {
			return 0, false
		}
		i, ok := index[d.AddDate(0, 0, -((int(d.Weekday())+6)%7)).Format("2006-01-02")]
		return i, ok
	}
	set := &pfCharts{Bucket: unit}
	band, bw := pfBand(len(buckets))
	first, last := buckets[0], buckets[len(buckets)-1]
	label := func(s string) string {
		if unit == "week" {
			return "week of " + s
		}
		return s
	}

	// Coverage by day, from 29 days before the span (the rolling
	// margin's window) on: buckets where some sales have no known
	// cost or tax, and the uncovered units behind the strip.
	start, _ := time.Parse("2006-01-02", fromDay)
	cov, err := app.queries.ListTradeCoverageDaily(ctx, db.ListTradeCoverageDailyParams{OwnerKind: kind, OwnerIds: ids, FromDay: start.AddDate(0, 0, -29).Format("2006-01-02")})
	if err != nil {
		return nil, err
	}
	uncovered := make([]bool, len(buckets))
	uncUnits := make([]int64, len(buckets))
	for _, c := range cov {
		if c.Units > c.CovUnits {
			if i, ok := bucketOf(c.Day); ok {
				uncovered[i] = true
				uncUnits[i] += c.Units - c.CovUnits
			}
		}
	}
	gaps := func() []pfBar {
		var out []pfBar
		for i, u := range uncovered {
			if u {
				out = append(out, pfBar{X: float64(i) * band, Y: 0, W: band, H: pfChartH, Class: "pf-gap",
					Tip: label(buckets[i]) + ": some sales have no known cost or tax line"})
			}
		}
		return out
	}
	anyGap := false
	for _, u := range uncovered {
		anyGap = anyGap || u
	}
	// The coverage strip: one segment per bucket, kept where the
	// bucket is fully covered, unverified and linked to that
	// bucket's uncovered item rows where it is not.
	strip := make([]coverSeg, len(buckets))
	for i, b := range buckets {
		if uncovered[i] {
			m := view.Modes
			m.Uncovered = b
			strip[i] = coverSeg{Class: "sem-unverified", Href: view.modeHref(m, "#items"),
				Label: fmt.Sprintf("%s: %s units sold with no known cost, see them in the item table", label(b), esi.FormatInt(uncUnits[i]))}
		} else {
			strip[i] = coverSeg{Class: "sem-kept", Label: label(b) + ": every sale has a known cost"}
		}
	}

	rows, err := app.queries.ListProfitBuckets(ctx, db.ListProfitBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}
	owners, err := app.queries.ListProfitOwnerBuckets(ctx, db.ListProfitOwnerBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}

	// Per-activity and per-owner bucket nets, and the running
	// totals the hero and the KPI sparklines draw.
	actNet := map[string][]float64{}
	actInc := map[string][]float64{}
	actCost := map[string][]float64{}
	actPresent := map[string]bool{}
	unatt := make([]float64, len(buckets))
	for _, r := range rows {
		i, ok := index[r.Bucket]
		if !ok {
			continue
		}
		if actNet[r.Activity] == nil {
			actNet[r.Activity] = make([]float64, len(buckets))
			actInc[r.Activity] = make([]float64, len(buckets))
			actCost[r.Activity] = make([]float64, len(buckets))
		}
		actNet[r.Activity][i] += r.Income - r.Costs
		actInc[r.Activity][i] += r.Income
		actCost[r.Activity][i] += r.Costs
		actPresent[r.Activity] = true
		if r.Activity == actUnattributed {
			unatt[i] += r.Income - r.Costs
		}
	}
	earnedCum := make([]float64, len(buckets))
	nwCum := make([]float64, len(buckets))
	{
		e, n := 0.0, 0.0
		for i := range buckets {
			for k, s := range actNet {
				if k != actUnattributed {
					e += s[i]
				}
			}
			n = e + cumSum(unatt, i)
			earnedCum[i], nwCum[i] = e, n
		}
	}
	if line, area, ok := sparklineSVG(earnedCum); ok {
		view.EarnedSparkLine, view.EarnedSparkArea = line, area
	}
	if view.Change != "" {
		if line, area, ok := sparklineSVG(nwCum); ok {
			view.ChangeSparkLine, view.ChangeSparkArea = line, area
		}
	}

	// The By activity table's rhythm: each activity's bucket nets
	// as spark-bars on that activity's own scale, so every row's
	// shape reads; the In and Net columns carry the magnitudes.
	for ri := range view.Activities {
		k := view.Activities[ri].Key
		if s, ok := actNet[k]; ok {
			maxAbs := 0.0
			for _, v := range s {
				maxAbs = math.Max(maxAbs, math.Abs(v))
			}
			view.Activities[ri].Rhythm = buildRhythmSpark(s, pfActivityClass[k], maxAbs)
		}
	}

	// The COGS buckets feed both the efficiency panel (per
	// bucket) and the waterfall (span totals: matched cost and
	// uncovered revenue).
	cogs, err := app.queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}
	var covCost, uncovRev float64
	for _, r := range cogs {
		if _, ok := index[r.Bucket]; ok {
			covCost += r.CovCost
			uncovRev += math.Max(r.Revenue-r.CovRevenue, 0)
		}
	}

	set.Hero = buildProfitHero(view, buckets, band, unit, label, actNet, actInc, actCost, actPresent, owners, index, names, earnedCum, nwCum, uncovered, gaps())
	set.Efficiency, err = app.buildEfficiencyChart(ctx, view, buckets, band, bw, unit, label, first, last, cov, cogs, index, bucketOf, uncovered, anyGap, gaps(), strip, kind, ids, start, brokerInMargin)
	if err != nil {
		return nil, err
	}
	// The waterfall's anchors come from the same bucket rows:
	// gross in and industry costs are the By activity table's
	// own sums over the span (the buckets group the same
	// profit_daily rows the table totals), earned its net.
	var wfGross, wfIndustry, wfEarned float64
	for k, inc := range actInc {
		if k == actUnattributed {
			continue
		}
		for i := range buckets {
			wfGross += inc[i]
			wfEarned += actNet[k][i]
			if k == actIndustry {
				wfIndustry += actCost[k][i]
			}
		}
	}
	set.Waterfall = buildWaterfall(view, wfGross, wfIndustry, wfEarned, covCost, uncovRev)
	return set, nil
}

// cumSum sums s[0..i].
func cumSum(s []float64, i int) float64 {
	t := 0.0
	for k := 0; k <= i && k < len(s); k++ {
		t += s[k]
	}
	return t
}

// stackSeries is one cumulative series in the hero's stack.
type stackSeries struct {
	label, areaClass, legendClass string
	cum                           []float64
}

// stackAreas stacks ordered cumulative series into area
// polygons: positive totals stack up from zero, negative ones
// down, as the old by-character chart drew them. It returns the
// polygons and the stack's upper and lower edges per bucket.
func stackAreas(series []stackSeries, x func(int) float64, sc heroScale) (areas []pfLine, pos, neg []float64) {
	n := 0
	if len(series) > 0 {
		n = len(series[0].cum)
	}
	pos, neg = make([]float64, n), make([]float64, n)
	for _, s := range series {
		lo, hi := make([]float64, n), make([]float64, n)
		for i, v := range s.cum {
			if v >= 0 {
				lo[i], hi[i] = pos[i], pos[i]+v
				pos[i] = hi[i]
			} else {
				lo[i], hi[i] = neg[i]+v, neg[i]
				neg[i] = lo[i]
			}
		}
		var pts []string
		for i := range s.cum {
			pts = append(pts, strconv.FormatFloat(x(i), 'f', 1, 64)+","+strconv.FormatFloat(sc.y(hi[i]), 'f', 1, 64))
		}
		for i := len(s.cum) - 1; i >= 0; i-- {
			pts = append(pts, strconv.FormatFloat(x(i), 'f', 1, 64)+","+strconv.FormatFloat(sc.y(lo[i]), 'f', 1, 64))
		}
		areas = append(areas, pfLine{Points: strings.Join(pts, " "), Class: s.areaClass})
	}
	return areas, pos, neg
}

// buildProfitHero draws the merged panel. In cumulative trend
// the earned series is a filled area (the total, or stacks by
// character or activity, or the filtered activity alone) and
// net worth change a line over it; the band between the curves
// is the gap. In daily trend the panel carries the diverging
// net by activity instead.
func buildProfitHero(view *profitView, buckets []string, band float64, unit string, label func(string) string,
	actNet, actInc, actCost map[string][]float64, actPresent map[string]bool, owners []db.ListProfitOwnerBucketsRow,
	index map[string]int, names map[int64]string, earnedCum, nwCum []float64, uncovered []bool, gapBars []pfBar) *profitHero {

	n := len(buckets)
	modes := view.Modes
	hasChange := !view.Corp && view.Change != ""
	x := func(i int) float64 { return float64(i)*band + band/2 }
	hero := &profitHero{W: pfChartW, H: pfHeroH, Bucket: unit}
	hero.XTicks = pfXTicksFor(buckets, band, pfChartW)
	actLabel := func(key string) string {
		for _, l := range profitActivityLabels {
			if l.key == key {
				return l.label
			}
		}
		return key
	}

	if modes.Trend == "daily" {
		hero.Daily = true
		hero.Name = "Net by activity, per " + unit
		hero.Label = "Income and costs per " + unit + " by activity"
		hero.Gaps = gapBars
		for i := range hero.Gaps {
			hero.Gaps[i].H = pfHeroH
		}
		type stack struct{ up, down float64 }
		stacks := make([]stack, n)
		for k, inc := range actInc {
			if k == actUnattributed {
				continue
			}
			for i := range buckets {
				stacks[i].up += inc[i]
				stacks[i].down -= actCost[k][i]
			}
		}
		var extent []float64
		for _, s := range stacks {
			extent = append(extent, s.up, s.down)
		}
		sc := newHeroScale(pfHeroH, extent...)
		hero.ZeroY, hero.ShowZero = sc.y(0), true
		hero.YTicks = pfTicksFor(sc.lo, sc.hi, sc.y, pfHeroH, pfShortISK)
		bw := math.Max(band*0.7, 1)
		ups, downs := make([]float64, n), make([]float64, n)
		rows := make([][]pfTipRow, n)
		netBy := map[string]float64{}
		for _, l := range profitActivityLabels {
			inc, ok := actInc[l.key]
			if !ok {
				continue
			}
			cost := actCost[l.key]
			cls := pfActivityClass[l.key]
			dim := modes.Activity != "" && modes.Activity != l.key
			for i := range buckets {
				bx := float64(i)*band + (band-bw)/2
				if inc[i] > 0 {
					b := pfBar{X: bx, Y: sc.y(ups[i] + inc[i]), W: bw, H: math.Max(sc.y(ups[i])-sc.y(ups[i]+inc[i]), 0.5), Class: cls}
					if dim {
						b.Class += " pf-dim"
					}
					hero.Bars = append(hero.Bars, b)
					ups[i] += inc[i]
				}
				if cost[i] > 0 {
					b := pfBar{X: bx, Y: sc.y(downs[i]), W: bw, H: math.Max(sc.y(downs[i]-cost[i])-sc.y(downs[i]), 0.5), Class: cls + " pf-cost"}
					if dim {
						b.Class += " pf-dim"
					}
					hero.Bars = append(hero.Bars, b)
					downs[i] -= cost[i]
				}
				if v := inc[i] - cost[i]; v != 0 {
					rows[i] = append(rows[i], pfTipRow{actLabel(l.key), signedISK(v) + " ISK", cls})
				}
				netBy[l.key] += inc[i] - cost[i]
			}
			hero.Legend = append(hero.Legend, pfLegend{Class: cls, Label: l.label, Value: signedISK(netBy[l.key])})
		}
		for i := range buckets {
			if len(rows[i]) > 0 {
				rows[i] = append(rows[i], pfTipRow{"Net", signedISK(ups[i]+downs[i]) + " ISK", ""})
			}
			hero.Hits = append(hero.Hits, pfHitFor(buckets[i], i, band, label, rows[i], uncovered[i]))
		}
		top, topNet := "", math.Inf(-1)
		for _, l := range profitActivityLabels {
			if actPresent[l.key] && netBy[l.key] > topNet {
				top, topNet = l.label, netBy[l.key]
			}
		}
		hero.Sub = "Income above the line, costs below."
		if top != "" {
			hero.Sub = fmt.Sprintf("%s brought the most: %s ISK net. ", top, signedISK(topNet)) + hero.Sub
		}
		if modes.Activity != "" {
			hero.Sub += " Showing " + actLabel(modes.Activity) + " in full colour."
		}
		if len(hero.Gaps) > 0 {
			hero.Legend = append(hero.Legend, pfLegend{Class: "pf-gap", Label: "Partly covered"})
		}
		return hero
	}

	// Cumulative trend. The earned stack first, in the split the
	// mode asks for; a filter isolates its activity in any split.
	var series []stackSeries
	edge := earnedCum
	switch {
	case modes.Activity != "":
		cum := make([]float64, n)
		run := 0.0
		for i, v := range actNet[modes.Activity] {
			run += v
			cum[i] = run
		}
		series = []stackSeries{{label: actLabel(modes.Activity), areaClass: "pf-area-a " + pfActivityClass[modes.Activity], legendClass: pfActivityClass[modes.Activity], cum: cum}}
		edge = cum
	case modes.Split == "character":
		per := map[int64][]float64{}
		for _, o := range owners {
			i, ok := index[o.Bucket]
			if !ok {
				continue
			}
			if per[o.OwnerID] == nil {
				per[o.OwnerID] = make([]float64, n)
			}
			per[o.OwnerID][i] += o.Net
		}
		order := make([]int64, 0, len(per))
		for id, s := range per {
			run := 0.0
			for i := range s {
				run += s[i]
				s[i] = run
			}
			order = append(order, id)
		}
		sort.Slice(order, func(a, b int) bool {
			if per[order[a]][n-1] != per[order[b]][n-1] {
				return per[order[a]][n-1] > per[order[b]][n-1]
			}
			return order[a] < order[b]
		})
		for k, id := range order {
			name := names[id]
			if name == "" {
				name = view.OwnerLabel
			}
			cls := fmt.Sprintf("pf-ch-%d", k%6+1)
			series = append(series, stackSeries{label: name, areaClass: "pf-area " + cls, legendClass: cls, cum: per[id]})
		}
		if len(series) == 0 {
			series = []stackSeries{{label: "Earned", areaClass: "pf-hero-area", legendClass: "pf-hero-sw-e", cum: earnedCum}}
		}
	case modes.Split == "activity":
		var keys []string
		for _, l := range profitActivityLabels {
			if actPresent[l.key] {
				keys = append(keys, l.key)
			}
		}
		cums := map[string][]float64{}
		for _, k := range keys {
			cum := make([]float64, n)
			run := 0.0
			for i, v := range actNet[k] {
				run += v
				cum[i] = run
			}
			cums[k] = cum
		}
		sort.SliceStable(keys, func(a, b int) bool { return cums[keys[a]][n-1] > cums[keys[b]][n-1] })
		for _, k := range keys {
			series = append(series, stackSeries{label: actLabel(k), areaClass: "pf-area-a " + pfActivityClass[k], legendClass: pfActivityClass[k], cum: cums[k]})
		}
		if len(series) == 0 {
			series = []stackSeries{{label: "Earned", areaClass: "pf-hero-area", legendClass: "pf-hero-sw-e", cum: earnedCum}}
		}
	default:
		series = []stackSeries{{label: "Earned (realized)", areaClass: "pf-hero-area", legendClass: "pf-hero-sw-e", cum: earnedCum}}
	}

	// Scales: one shared scale, or the earned stack and the net
	// worth line each on their own (indexed).
	var extent []float64
	{
		pos, neg := make([]float64, n), make([]float64, n)
		for _, s := range series {
			for i, v := range s.cum {
				if v >= 0 {
					pos[i] += v
				} else {
					neg[i] += v
				}
			}
		}
		for i := range buckets {
			extent = append(extent, pos[i], neg[i])
		}
	}
	extent = append(extent, earnedCum...)
	shared := modes.Scale != "indexed"
	earnScale := newHeroScale(pfHeroH, extent...)
	nwScale := earnScale
	if hasChange {
		if shared {
			earnScale = newHeroScale(pfHeroH, append(append([]float64{}, extent...), nwCum...)...)
			nwScale = earnScale
		} else {
			nwScale = newHeroScale(pfHeroH, nwCum...)
		}
	}
	hero.Areas, _, _ = stackAreas(series, x, earnScale)
	hero.ZeroY, hero.ShowZero = earnScale.y(0), earnScale.lo < 0
	hero.YTicks = pfTicksFor(earnScale.lo, earnScale.hi, earnScale.y, pfHeroH, pfShortISK)
	if hasChange && !shared {
		hero.YTicksR = pfTicksFor(nwScale.lo, nwScale.hi, nwScale.y, pfHeroH, pfShortISK)
	}
	// The earned total's edge line over the stack.
	{
		xs, ys := make([]float64, n), make([]float64, n)
		for i, v := range edge {
			xs[i], ys[i] = x(i), earnScale.y(v)
		}
		hero.Lines = append(hero.Lines, pfLine{Points: pfPoints(xs, ys), Class: "pf-hero-edge"})
	}
	if hasChange {
		xs, ys := make([]float64, n), make([]float64, n)
		for i, v := range nwCum {
			xs[i], ys[i] = x(i), nwScale.y(v)
		}
		hero.Lines = append(hero.Lines, pfLine{Points: pfPoints(xs, ys), Class: "pf-hero-nw"})
		// The gap band: one quad per bucket interval where net
		// worth runs ahead of earned at both ends. On a shared
		// scale only; indexed scales have no shared gap.
		if shared && modes.Activity == "" {
			for i := 0; i+1 < n; i++ {
				if nwCum[i] > earnedCum[i] && nwCum[i+1] > earnedCum[i+1] {
					hero.GapQuads = append(hero.GapQuads, strings.Join([]string{
						strconv.FormatFloat(x(i), 'f', 1, 64) + "," + strconv.FormatFloat(earnScale.y(nwCum[i]), 'f', 1, 64),
						strconv.FormatFloat(x(i+1), 'f', 1, 64) + "," + strconv.FormatFloat(earnScale.y(nwCum[i+1]), 'f', 1, 64),
						strconv.FormatFloat(x(i+1), 'f', 1, 64) + "," + strconv.FormatFloat(earnScale.y(earnedCum[i+1]), 'f', 1, 64),
						strconv.FormatFloat(x(i), 'f', 1, 64) + "," + strconv.FormatFloat(earnScale.y(earnedCum[i]), 'f', 1, 64),
					}, " "))
				}
			}
		}
	}
	// Hits, with the gap at the cursor and, at the last bucket,
	// what the gap is made of.
	for i := range buckets {
		var rows []pfTipRow
		if modes.Activity != "" {
			rows = append(rows, pfTipRow{actLabel(modes.Activity) + " so far", signedISK(edge[i]) + " ISK", pfActivityClass[modes.Activity]})
		} else {
			rows = append(rows, pfTipRow{"Earned so far", signedISK(earnedCum[i]) + " ISK", "pf-hero-sw-e"})
		}
		if hasChange {
			rows = append(rows, pfTipRow{"Net worth change so far", signedISK(nwCum[i]) + " ISK", "pf-hero-sw-nw"})
			rows = append(rows, pfTipRow{"Gap", signedISK(nwCum[i]-earnedCum[i]) + " ISK", "sem-unverified"})
			if i == n-1 {
				if view.openValue > 0 {
					rows = append(rows, pfTipRow{"Open positions at appraisal", esi.FormatISK(view.openValue) + " ISK", "sem-kept"})
				}
				if view.hasAssets {
					other := math.Max(view.assetsNow-view.openValue, 0)
					rows = append(rows, pfTipRow{"Other assets and escrow", esi.FormatISK(other) + " ISK", "sem-recycled"})
				}
			}
		}
		hero.Hits = append(hero.Hits, pfHitFor(buckets[i], i, band, label, rows, uncovered[i]))
	}
	// End labels, nudged apart when the two series end close.
	posOf := func(sc heroScale, v float64) int {
		return int(math.Round(sc.y(v) / float64(pfHeroH) * 100))
	}
	earnedEnd := heroEnd{Text: "earned " + pfShortISK(edge[n-1]), Pos: strconv.Itoa(posOf(earnScale, edge[n-1])), Class: "pf-end-earned"}
	hero.Ends = append(hero.Ends, earnedEnd)
	if hasChange {
		nwEnd := heroEnd{Text: "worth " + pfShortISK(nwCum[n-1]), Pos: strconv.Itoa(posOf(nwScale, nwCum[n-1])), Class: "pf-end-nw"}
		ep, _ := strconv.Atoi(earnedEnd.Pos)
		np, _ := strconv.Atoi(nwEnd.Pos)
		if d := math.Abs(float64(np - ep)); d < 10 {
			shift := int(math.Ceil((10 - d) / 2))
			if np <= ep {
				np, ep = np-shift, ep+shift
			} else {
				np, ep = np+shift, ep-shift
			}
			nwEnd.Pos = strconv.Itoa(max(min(np, 100), 0))
			hero.Ends[0].Pos = strconv.Itoa(max(min(ep, 100), 0))
		}
		hero.Ends = append(hero.Ends, nwEnd)
		hero.Legend = append(hero.Legend, pfLegend{Class: "pf-hero-sw-nw", Label: "Net worth change", Value: signedISK(nwCum[n-1])})
	}
	// End labels stay inside the plot: hard against the top or
	// bottom edge a label sits half outside the panel and crowds
	// the mode pills above it.
	for i := range hero.Ends {
		p, _ := strconv.Atoi(hero.Ends[i].Pos)
		hero.Ends[i].Pos = strconv.Itoa(max(min(p, 96), 6))
	}
	for _, s := range series {
		hero.Legend = append(hero.Legend, pfLegend{Class: s.legendClass, Label: s.label, Value: signedISK(s.cum[n-1])})
	}
	if len(hero.GapQuads) > 0 {
		hero.Legend = append(hero.Legend, pfLegend{Class: "sem-unverified", Label: "Gap: ISK held as stock and assets"})
	}
	if hasChange && view.openValue > 0 {
		hero.GapChip = "Open positions " + pfShortISK(view.openValue) + " in the gap"
	}
	hero.Name = "Earned, running total"
	if hasChange {
		hero.Name = "Earned vs net worth, running total"
	}
	hero.Label = hero.Name
	switch {
	case modes.Activity != "":
		hero.Sub = "Only " + actLabel(modes.Activity) + ": its running total"
		if hasChange {
			hero.Sub += ", with net worth change over the top."
		} else {
			hero.Sub += " over the span."
		}
	case modes.Split == "character":
		hero.Sub = "The running total, split by character: each band is one character's part of it."
	case modes.Split == "activity":
		hero.Sub = "The running total, split by activity."
	default:
		hero.Sub = "The filled area is profit actually realized."
	}
	if hasChange {
		if shared {
			hero.Sub += " One shared scale: the bright line is what you are worth, and the gap between the curves is ISK sitting in stock and assets, not yet profit."
		} else {
			hero.Sub += " Each series rides its own scale: earned on the left, net worth on the right."
		}
	}
	return hero
}

// buildWaterfall shapes the span's ISK flow into floating
// steps. Its step definitions are pinned on pfWaterfall.
func buildWaterfall(view *profitView, gross, ind, earned, covCost, uncovRev float64) *pfWaterfall {
	if gross <= 0 {
		return nil
	}
	fees := gross - covCost - ind - earned
	const w, h = 600, 190
	levels := []float64{gross, gross - covCost, gross - covCost - ind, earned}
	hi, lo := gross, math.Min(0, earned)
	for _, l := range levels {
		hi, lo = math.Max(hi, l), math.Min(lo, l)
	}
	if hi == lo {
		hi = lo + 1
	}
	y := func(v float64) float64 { return 10 + (hi-v)/(hi-lo)*float64(h-20) }
	steps := []struct {
		name, class string
		short       string  // the axis label: the full name rides the legend and hover rows
		val         float64 // signed: what the step adds or takes
		top, bot    float64 // the levels the bar spans
	}{
		{"Gross in", "pf-wf-gross", "Gross in", gross, gross, 0},
		{"Cost of goods sold", "sem-recycled", "COGS", -covCost, levels[0], levels[1]},
		{"Industry costs", "pf-wf-industry", "Industry costs", -ind, levels[1], levels[2]},
		{"Fees, taxes and other outflows", "sem-lost", "Fees & other out", -fees, levels[2], levels[3]},
		{"Net earned", "sem-kept", "Net earned", earned, earned, 0},
	}
	if earned < 0 {
		steps[4].class = "sem-lost"
	}
	wf := &pfWaterfall{
		Name:  "Where the ISK went",
		Label: "ISK waterfall from gross in to net earned",
		W:     w, H: h,
		Title: fmt.Sprintf("Of %s ISK gross in, %s ISK was kept as profit", pfShortISK(gross), pfShortISK(earned)),
	}
	wf.Sub = wf.Title + "."
	wf.Info = "All the ISK that came in this span, taken apart: what went back out as the cost of what sold, what industry consumed, what fees and taxes took for good, and what stayed as profit. The steps are the By activity table's own figures, so they add up to Earned exactly."
	const barW = 64.0
	stepX := []float64{30, 144, 258, 372, 486}
	barFor := func(i int, top, bot float64, class string) pfWfBar {
		y1, y2 := y(top), y(bot)
		return pfWfBar{X: stepX[i], Y: math.Min(y1, y2), W: barW, H: math.Max(math.Abs(y2-y1), 1), Class: class}
	}
	for i, s := range steps {
		wf.Bars = append(wf.Bars, barFor(i, s.top, s.bot, s.class))
		// The step's value at its head: inside the bar when the
		// bar is tall enough to hold the line, a full line above
		// it when not (the label renders downward from its mark,
		// so a short bar needs the whole line of clearance).
		bar := wf.Bars[i]
		vy := bar.Y + 11
		if bar.H < 26 {
			vy = bar.Y - 13
		}
		val := pfShortISK(s.val)
		wf.Values = append(wf.Values, pfWfLabel{Text: val,
			XPos: strconv.Itoa(int(math.Round((stepX[i] + barW/2) / w * 100))),
			YPos: strconv.Itoa(int(math.Round(vy / h * 100)))})
		cls := "pf-x-mid"
		if i == 0 {
			cls = "pf-x-start"
		} else if i == len(steps)-1 {
			cls = "pf-x-end"
		}
		wf.Names = append(wf.Names, pfTick{Pos: strconv.Itoa(int(math.Round((stepX[i] + barW/2) / w * 100))), Label: s.short, Class: cls})
		rows := [][3]string{{s.name, signedISK(s.val) + " ISK", s.class}}
		if i == 4 && uncovRev > 0 {
			rows = append(rows, [3]string{"Of which no known cost", esi.FormatISK(uncovRev) + " ISK", "sem-unverified"})
		}
		js, _ := json.Marshal(rows)
		wf.Hits = append(wf.Hits, pfHit{X: stepX[i] - 25, W: 114, Date: s.name, Label: s.name, Rows: string(js)})
	}
	// Dashed connectors at each level between neighbouring bars.
	for i := 0; i < 4; i++ {
		lvl := levels[i]
		if i == 3 {
			lvl = earned
		}
		wf.Conns = append(wf.Conns, pfWfConn{X1: stepX[i] + barW, X2: stepX[i+1], Y: y(lvl)})
	}
	// The net bar's unverified cap: the top slice of the net
	// column, hatched, with a floor height so a small total
	// still reads.
	if earned > 0 && uncovRev > 0 {
		net := wf.Bars[4]
		capH := math.Min(math.Max(uncovRev/earned*net.H, 3), net.H)
		wf.Cap = &pfWfBar{X: net.X, Y: net.Y, W: net.W, H: capH, Class: "pf-wf-cap"}
		m := view.Modes
		m.Uncovered = "1"
		wf.CapHref = view.modeHref(m, "#items")
	}
	wf.Legend = []pfLegend{
		{Class: "sem-recycled", Label: "Capital recycled (COGS)", Value: esi.FormatISK(covCost)},
		{Class: "pf-wf-industry", Label: "Industry costs", Value: esi.FormatISK(ind)},
		{Class: "sem-lost", Label: "Fees, taxes and other outflows", Value: esi.FormatISK(fees)},
		{Class: "sem-kept", Label: "Net earned", Value: signedISK(earned)},
	}
	if uncovRev > 0 {
		wf.Legend = append(wf.Legend, pfLegend{Class: "pf-gap", Label: "Sales with no cost basis", Value: esi.FormatISK(uncovRev)})
	}
	return wf
}

// buildEfficiencyChart draws the trading panel in its two
// modes. Efficiency: each bucket's gross trading sales stacked
// into the cost of the goods (recycled), the fees and tax
// (lost) and what was kept (kept), uncovered sales hatched on
// top, a loss bucket's costs outlined above a marker at the
// sales height, and the 30-day rolling margin over it all on
// its own scale, dashed where the window's coverage is partial.
// Fees: the fee deep-dive, fees by kind with trading fees as a
// share of sales on the right scale.
func (app *Application) buildEfficiencyChart(ctx context.Context, view *profitView, buckets []string, band, bw float64, unit string, label func(string) string, first, last string,
	cov []db.ListTradeCoverageDailyRow, cogs []db.ListTradeCOGSBucketsRow, index map[string]int, bucketOf func(string) (int, bool),
	uncovered []bool, anyGap bool, gapBars []pfBar, strip []coverSeg, kind string, ids []int64, start time.Time, brokerInMargin bool) (*pfChart, error) {

	n := len(buckets)
	if view.Modes.Eff == "fees" {
		fees, err := app.queries.ListProfitFeeBuckets(ctx, db.ListProfitFeeBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromAt: start})
		if err != nil {
			return nil, err
		}
		type feeRow struct{ tax, broker, industry, lp, revenue float64 }
		fr := make([]feeRow, n)
		var feeTotal, tradeFees, revenue float64
		for _, f := range fees {
			if i, ok := index[f.Bucket]; ok {
				fr[i] = feeRow{f.SalesTax, f.Broker, f.Industry, f.LpStore, f.Revenue}
				feeTotal += f.SalesTax + f.Broker + f.Industry + f.LpStore
				tradeFees += f.SalesTax + f.Broker
				revenue += f.Revenue
			}
		}
		if feeTotal <= 0 {
			return nil, nil
		}
		var hs []float64
		for _, f := range fr {
			hs = append(hs, f.tax+f.broker+f.industry+f.lp)
		}
		sf := newPFScale(hs...)
		cf := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sf.y(0), First: first, Last: last,
			Label: "Fees per " + unit + ", and trading fees as a share of sales", HasGaps: anyGap, Gaps: gapBars, Low: "0", High: esi.FormatISK(sf.hi),
			Name: "Trading efficiency", AxisL: "ISK per " + unit, Strip: strip,
			Title: fmt.Sprintf("Paid %s ISK in fees", esi.FormatISK(feeTotal))}
		if revenue > 0 {
			cf.Title += fmt.Sprintf("; trading fees took %.1f%% of what sold", tradeFees/revenue*100)
			cf.Info = "Bars: fees by kind, on the left scale. Line: sales tax and broker fees as a share of market sales, on the right scale, with a gap where nothing sold."
		}
		cf.Sub = cf.Title + "."
		parts := []struct {
			class, name string
			v           func(feeRow) float64
		}{
			{"pf-f-tax", "Sales tax", func(f feeRow) float64 { return f.tax }},
			{"pf-f-broker", "Broker fees", func(f feeRow) float64 { return f.broker }},
			{"pf-f-industry", "Industry fees", func(f feeRow) float64 { return f.industry }},
			{"pf-f-lp", "LP store ISK", func(f feeRow) float64 { return f.lp }},
		}
		share := make([]float64, n)
		shareHi := 0.0
		for i, f := range fr {
			share[i] = math.NaN()
			if f.revenue > 0 {
				share[i] = (f.tax + f.broker) / f.revenue * 100
				shareHi = math.Max(shareHi, share[i])
			}
		}
		shown := map[string]float64{}
		feeRows := make([][]pfTipRow, n)
		for i, f := range fr {
			base := 0.0
			for _, p := range parts {
				if v := p.v(f); v > 0 {
					cf.Bars = append(cf.Bars, sf.bar(float64(i)*band+(band-bw)/2, bw, base, v, p.class, ""))
					base += v
					shown[p.class] += v
					feeRows[i] = append(feeRows[i], pfTipRow{p.name, esi.FormatISK(v) + " ISK", p.class})
				}
			}
			if base > 0 {
				feeRows[i] = append(feeRows[i], pfTipRow{"Total", esi.FormatISK(base) + " ISK", ""})
			}
			if !math.IsNaN(share[i]) {
				feeRows[i] = append(feeRows[i], pfTipRow{"Trading fees of sales", fmt.Sprintf("%.2f%% of %s ISK", share[i], esi.FormatISK(f.revenue)), "pf-share"})
			}
		}
		if shareHi > 0 {
			ss := pfScale{lo: 0, hi: shareHi * 1.15}
			cf.Lines = pfBrokenLines(share, band, ss.y, "pf-share")
			cf.AxisR = "Share of sales"
			cf.setYTicksRight(ss.lo, ss.hi, ss.y, pfChartH, func(v float64) string { return fmt.Sprintf("%.1f%%", v) })
		}
		cf.setHits(buckets, band, label, feeRows, uncovered)
		cf.setYTicks(0, sf.hi, sf.y, pfChartH, pfShortISK)
		cf.setXTicks(buckets, band)
		for _, p := range parts {
			if shown[p.class] > 0 {
				cf.Legend = append(cf.Legend, pfLegend{Class: p.class, Label: p.name, Value: esi.FormatISK(shown[p.class])})
			}
		}
		if shareHi > 0 {
			cf.Legend = append(cf.Legend, pfLegend{Class: "pf-share", Label: "Share of sales"})
		}
		return cf, nil
	}

	// Efficiency mode.
	type effRow struct{ sales, covRev, covCost, covTax, broker float64 }
	er := make([]effRow, n)
	for _, r := range cogs {
		if i, ok := index[r.Bucket]; ok {
			er[i].sales, er[i].covRev, er[i].covCost = r.Revenue, r.CovRevenue, r.CovCost
		}
	}
	for _, c := range cov {
		if i, ok := bucketOf(c.Day); ok {
			er[i].covTax += c.CovTax
			er[i].broker += c.Broker
		}
	}
	feesOf := func(r effRow) float64 {
		f := r.covTax
		if brokerInMargin {
			f += r.broker
		}
		return f
	}
	var totCovRev, totCost, totFees float64
	var heights []float64
	for _, r := range er {
		f := feesOf(r)
		totCovRev, totCost, totFees = totCovRev+r.covRev, totCost+r.covCost, totFees+f
		heights = append(heights, math.Max(r.sales, r.covCost+f))
	}
	if totCovRev <= 0 {
		return nil, nil
	}
	sc := newPFScale(heights...)
	ce := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sc.y(0), First: first, Last: last,
		Label:   "Trading sales split into cost of goods, fees and tax, and what was kept, per " + unit,
		HasGaps: anyGap, Gaps: gapBars, Strip: strip,
		Name: "Trading efficiency", AxisL: "ISK per " + unit, AxisR: "Margin"}
	effRows := make([][]pfTipRow, n)
	for i, r := range er {
		x := float64(i)*band + (band-bw)/2
		fees := feesOf(r)
		kept := math.Max(r.covRev-r.covCost-fees, 0)
		costs := r.covCost + fees
		if r.covCost > 0 {
			ce.Bars = append(ce.Bars, sc.bar(x, bw, 0, r.covCost, "sem-recycled", ""))
		}
		if fees > 0 {
			ce.Bars = append(ce.Bars, sc.bar(x, bw, r.covCost, fees, "sem-lost", ""))
		}
		if kept > 0 {
			ce.Bars = append(ce.Bars, sc.bar(x, bw, costs, kept, "sem-kept", ""))
		}
		if top := costs + kept; r.sales > top+0.5 {
			ce.Bars = append(ce.Bars, sc.bar(x, bw, top, r.sales-top, "pf-e-unc", ""))
		}
		if costs > r.sales+0.5 {
			// A loss bucket: the costs pass the sales height.
			// The part above the marker takes the lost outline,
			// and the marker itself is drawn across the bar.
			ce.Bars = append(ce.Bars, sc.bar(x, bw, r.sales, costs-r.sales, "pf-e-loss", ""))
			y := sc.y(r.sales)
			ce.Lines = append(ce.Lines, pfLine{Points: pfPoints([]float64{x, x + bw}, []float64{y, y}), Class: "pf-e-marker"})
		}
		if r.sales > 0 {
			effRows[i] = append(effRows[i], pfTipRow{"Gross sales", esi.FormatISK(r.sales) + " ISK", ""})
			if r.covCost > 0 {
				effRows[i] = append(effRows[i], pfTipRow{"Cost of goods", esi.FormatISK(r.covCost) + " ISK", "sem-recycled"})
			}
			if fees > 0 {
				effRows[i] = append(effRows[i], pfTipRow{"Fees and tax", esi.FormatISK(fees) + " ISK", "sem-lost"})
			}
			if kept > 0 {
				effRows[i] = append(effRows[i], pfTipRow{"Kept", esi.FormatISK(kept) + " ISK", "sem-kept"})
			}
			if r.sales > r.covRev+0.5 {
				effRows[i] = append(effRows[i], pfTipRow{"Sold with no known cost", esi.FormatISK(r.sales-r.covRev) + " ISK", "sem-unverified"})
			}
			if costs > r.sales+0.5 {
				effRows[i] = append(effRows[i], pfTipRow{"Lost on the day", esi.FormatISK(costs-r.sales) + " ISK", "sem-lost"})
			}
		}
	}

	// The 30-day rolling margin, one point per bucket at the
	// bucket's end, on its own right-hand scale: solid where
	// every unit in the window is covered, dashed where some
	// are not, absent where nothing covered sold.
	byDay := map[string]db.ListTradeCoverageDailyRow{}
	for _, r := range cov {
		byDay[r.Day] = r
	}
	full := make([]float64, n)
	part := make([]float64, n)
	lastMargin, lastShare := math.NaN(), 1.0
	for i, b := range buckets {
		full[i], part[i] = math.NaN(), math.NaN()
		end, err := time.Parse("2006-01-02", b)
		if err != nil {
			continue
		}
		if unit == "week" {
			end = end.AddDate(0, 0, 6)
		}
		var units, covUnits int64
		var rev, cost float64
		for k := 0; k < 30; k++ {
			r, ok := byDay[end.AddDate(0, 0, -k).Format("2006-01-02")]
			if !ok {
				continue
			}
			units, covUnits = units+r.Units, covUnits+r.CovUnits
			rev += r.CovRevenue
			cost += r.CovCost + r.CovTax
			if brokerInMargin {
				cost += r.Broker
			}
		}
		if units == 0 || rev <= 0 {
			effRows[i] = append(effRows[i], pfTipRow{"30-day margin", "no covered sales", ""})
			continue
		}
		m := (rev - cost) / rev * 100
		lastMargin, lastShare = m, float64(covUnits)/float64(units)
		effRows[i] = append(effRows[i], pfTipRow{"30-day margin", fmt.Sprintf("%.1f%%", m), "pf-roll"},
			pfTipRow{"Units covered", fmt.Sprintf("%.0f%%", math.Floor(lastShare*100)), ""})
		if covUnits >= units {
			full[i] = m
		} else {
			part[i] = m
		}
	}
	// Each run joins the next at the bucket the coverage
	// changed, so the line reads as one.
	for i := 1; i < n; i++ {
		switch {
		case !math.IsNaN(full[i]) && !math.IsNaN(part[i-1]):
			part[i] = full[i]
		case !math.IsNaN(part[i]) && !math.IsNaN(full[i-1]):
			full[i] = part[i]
		}
	}
	if !math.IsNaN(lastMargin) {
		lo, hi := math.Inf(1), math.Inf(-1)
		for i := range buckets {
			for _, v := range []float64{full[i], part[i]} {
				if !math.IsNaN(v) {
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
			}
		}
		if lo < 0 && hi < 0 {
			hi = 0
		}
		pad := math.Max((hi-lo)*0.15, 0.5)
		sm := pfScale{lo: lo - pad, hi: hi + pad}
		ce.Lines = append(ce.Lines, pfBrokenLines(full, band, sm.y, "pf-margin")...)
		ce.Lines = append(ce.Lines, pfBrokenLines(part, band, sm.y, "pf-margin pf-margin-part")...)
		ce.setYTicksRight(sm.lo, sm.hi, sm.y, pfChartH, func(v float64) string { return fmt.Sprintf("%.1f%%", v) })
		ce.Legend = append(ce.Legend, pfLegend{Class: "pf-roll", Label: "30-day margin"})
	}
	ce.setHits(buckets, band, label, effRows, uncovered)
	ce.setYTicks(0, sc.hi, sc.y, pfChartH, pfShortISK)
	ce.setXTicks(buckets, band)
	keptTotal := totCovRev - totCost - totFees
	ce.Title = fmt.Sprintf("Kept %s ISK of %s ISK covered sales", esi.FormatISK(keptTotal), esi.FormatISK(totCovRev))
	ce.Sub = ce.Title + "."
	if !math.IsNaN(lastMargin) {
		ce.Sub += fmt.Sprintf(" 30-day trading margin: %.1f%% on the %.0f%% of units covered.", lastMargin, math.Floor(lastShare*100))
	}
	ce.Info = "Each bar is a " + unit + "'s gross trading sales, split into what the goods cost, what fees and tax took, and what was kept. The hatched part sold with no known cost. Where goods and fees pass the top of the bar, the " + unit + " lost money."
	ce.Legend = append([]pfLegend{
		{Class: "sem-recycled", Label: "Cost of goods", Value: esi.FormatISK(totCost)},
		{Class: "sem-lost", Label: "Fees and tax", Value: esi.FormatISK(totFees)},
		{Class: "sem-kept", Label: "Kept", Value: esi.FormatISK(keptTotal)},
	}, ce.Legend...)
	return ce, nil
}
