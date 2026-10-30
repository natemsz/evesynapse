package app

// The Profit page's charts (profit v2, part 4): daily net with its
// rolling average, the split by activity, fees beside fees as a share
// of sales, a running line per character, and the rolling trading
// margin. Each reads one grouped query over the span (day buckets up
// to 90 days, week buckets beyond) and is drawn here as SVG geometry
// the template only places: colours come from classes on theme
// tokens, words from HTML around the marks. A bucket in which some
// sales have no known cost or tax line is hatched on every chart, and
// the margin line breaks there rather than drawing a partial figure.

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
	Title, Sub   string // the finding, with its number; what it covers
	Label        string // aria-label
	W, H         int
	ZeroY        float64
	Bars         []pfBar
	Lines        []pfLine
	Areas        []pfLine // stacked bands, drawn under the lines
	Gaps         []pfBar
	Legend       []pfLegend
	Low, High    string
	First, Last  string
	HasGaps      bool
	ShowZeroLine bool
	Hits         []pfHit  // one hover column per bucket, for the site's chart tip
	YTicks       []pfTick // value gridlines and their labels
	YTicksR      []pfTick // a second value scale, labelled on the right
	XTicks       []pfTick // date labels under the plot
}

// pfHit is one bucket's hover column: the tip's heading and its rows
// (JSON: [label, value, swatch class]) for app.js's chart tip.
type pfHit struct {
	X, W        float64
	Date, Label string
	Rows        string
}

// pfTick is one axis label: Y (plot units) for its gridline, Pos (a
// whole percent of the plot, a pf-y-N or pf-x-N class: the CSP allows no
// inline style) for the HTML label laid over it, so the words stay
// crisp however the plot stretches.
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
	Net, Activities, Fees, COGS, Margin *pfChart // Net: the running total earned, by character
	Bucket                              string   // "day" or "week"
}

// pfActivityClass names each activity's colour class; the same
// activity takes the same colour on every chart.
var pfActivityClass = map[string]string{
	actTrading: "pf-a-trading", actIndustry: "pf-a-industry", actBounties: "pf-a-bounties",
	actPI: "pf-a-pi", actContracts: "pf-a-contracts", actSales: "pf-a-sales", actLP: "pf-a-lp",
	actOther: "pf-a-other", actUpkeep: "pf-a-upkeep", actTransfers: "pf-a-transfers",
	actUnattributed: "pf-a-unattributed",
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

func (app *Application) buildProfitCharts(ctx context.Context, view *profitView, kind string, ids []int64, names map[int64]string, fromDay string, brokerInMargin bool) (*pfCharts, error) {
	now := time.Now().UTC()
	unit := "day"
	if view.Days > 90 {
		unit = "week"
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

	// Coverage: buckets where some sales have no known cost or tax.
	start, _ := time.Parse("2006-01-02", fromDay)
	cov, err := app.queries.ListTradeCoverageDaily(ctx, db.ListTradeCoverageDailyParams{OwnerKind: kind, OwnerIds: ids, FromDay: start.AddDate(0, 0, -29).Format("2006-01-02")})
	if err != nil {
		return nil, err
	}
	uncovered := make([]bool, len(buckets))
	for _, c := range cov {
		if c.Units > c.CovUnits {
			if i, ok := bucketOf(c.Day); ok {
				uncovered[i] = true
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

	rows, err := app.queries.ListProfitBuckets(ctx, db.ListProfitBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}

	// 1. Earned over time: the running total of what every owner
	// earned, stacked by character so each band is that character's
	// part of it (one band for one owner). Bands sit above zero while a
	// character is ahead and below while behind; the total runs on top.
	owners, err := app.queries.ListProfitOwnerBuckets(ctx, db.ListProfitOwnerBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}
	per := map[int64][]float64{}
	stepNet := make([]float64, len(buckets))
	for _, o := range owners {
		i, ok := index[o.Bucket]
		if !ok {
			continue
		}
		if per[o.OwnerID] == nil {
			per[o.OwnerID] = make([]float64, len(buckets))
		}
		per[o.OwnerID][i] += o.Net
		stepNet[i] += o.Net
	}
	if len(per) > 0 {
		order := make([]int64, 0, len(per))
		total := make([]float64, len(buckets))
		for id, s := range per {
			order = append(order, id)
			run := 0.0
			for i := range s {
				run += s[i]
				s[i] = run
				total[i] += run
			}
		}
		lastAt := len(buckets) - 1
		sort.Slice(order, func(i, j int) bool {
			if per[order[i]][lastAt] != per[order[j]][lastAt] {
				return per[order[i]][lastAt] > per[order[j]][lastAt]
			}
			return order[i] < order[j]
		})
		pos, neg := make([]float64, len(buckets)), make([]float64, len(buckets))
		lows, highs := map[int64][]float64{}, map[int64][]float64{}
		for _, id := range order {
			lo, hi := make([]float64, len(buckets)), make([]float64, len(buckets))
			for i, v := range per[id] {
				if v >= 0 {
					lo[i], hi[i] = pos[i], pos[i]+v
					pos[i] = hi[i]
				} else {
					lo[i], hi[i] = neg[i]+v, neg[i]
					neg[i] = lo[i]
				}
			}
			lows[id], highs[id] = lo, hi
		}
		se := newPFScale(append(append(append([]float64{}, pos...), neg...), total...)...)
		name := func(id int64) string {
			if n, ok := names[id]; ok && n != "" {
				return n
			}
			return view.OwnerLabel
		}
		ce := &pfChart{W: pfChartW, H: pfChartH, ZeroY: se.y(0), First: first, Last: last, ShowZeroLine: true,
			Label: "Running total earned, by character", HasGaps: anyGap, Gaps: gaps(),
			Title: fmt.Sprintf("Earned %s ISK over %d days", signedISK(total[lastAt]), view.Days)}
		if len(order) > 1 {
			ce.Sub = "The running total, split by character: each band is one character's part of it."
		} else {
			ce.Sub = "The running total over the span."
		}
		x := func(i int) float64 { return float64(i)*band + band/2 }
		for n, id := range order {
			cls := fmt.Sprintf("pf-area pf-ch-%d", n%6+1)
			var pts []string
			for i := range buckets {
				pts = append(pts, strconv.FormatFloat(x(i), 'f', 1, 64)+","+strconv.FormatFloat(se.y(highs[id][i]), 'f', 1, 64))
			}
			for i := len(buckets) - 1; i >= 0; i-- {
				pts = append(pts, strconv.FormatFloat(x(i), 'f', 1, 64)+","+strconv.FormatFloat(se.y(lows[id][i]), 'f', 1, 64))
			}
			ce.Areas = append(ce.Areas, pfLine{Points: strings.Join(pts, " "), Class: cls})
			share := ""
			if total[lastAt] > 0 && per[id][lastAt] > 0 {
				share = fmt.Sprintf(", %.0f%%", per[id][lastAt]/total[lastAt]*100)
			}
			ce.Legend = append(ce.Legend, pfLegend{Class: fmt.Sprintf("pf-ch-%d", n%6+1), Label: name(id), Value: signedISK(per[id][lastAt]) + share})
		}
		xs, ys := make([]float64, len(buckets)), make([]float64, len(buckets))
		for i, v := range total {
			xs[i], ys[i] = x(i), se.y(v)
		}
		if len(order) > 1 {
			ce.Lines = []pfLine{{Points: pfPoints(xs, ys), Class: "pf-total"}}
			ce.Legend = append(ce.Legend, pfLegend{Class: "pf-total", Label: "Total"})
		}
		earnedRows := make([][]pfTipRow, len(buckets))
		for i := range buckets {
			if len(order) > 1 {
				earnedRows[i] = append(earnedRows[i], pfTipRow{"Total so far", signedISK(total[i]) + " ISK", "pf-total"})
			}
			for n, id := range order {
				v := per[id][i]
				value := signedISK(v) + " ISK"
				if len(order) > 1 && total[i] > 0 && v > 0 {
					value += fmt.Sprintf(" (%.0f%%)", v/total[i]*100)
				}
				label := name(id)
				if len(order) == 1 {
					label = "So far"
				}
				earnedRows[i] = append(earnedRows[i], pfTipRow{label, value, fmt.Sprintf("pf-ch-%d", n%6+1)})
			}
			earnedRows[i] = append(earnedRows[i], pfTipRow{"Earned that " + unit, signedISK(stepNet[i]) + " ISK", ""})
		}
		ce.setHits(buckets, band, label, earnedRows, uncovered)
		ce.setYTicks(se.lo, se.hi, se.y, pfChartH, pfShortISK)
		ce.setXTicks(buckets, band)
		set.Net = ce
	}

	// 2. By activity: income stacked above zero, costs below.
	type stack struct{ up, down float64 }
	stacks := make([]stack, len(buckets))
	netBy := map[string]float64{}
	present := map[string]bool{}
	type seg struct {
		i         int
		act       string
		inc, cost float64
	}
	var segs []seg
	for _, r := range rows {
		i, ok := index[r.Bucket]
		if !ok {
			continue
		}
		if r.Activity == actUnattributed {
			// The remainder is a net figure, not income and costs.
			if v := r.Income - r.Costs; v >= 0 {
				segs = append(segs, seg{i, r.Activity, v, 0})
			} else {
				segs = append(segs, seg{i, r.Activity, 0, -v})
			}
		} else {
			segs = append(segs, seg{i, r.Activity, r.Income, r.Costs})
			netBy[r.Activity] += r.Income - r.Costs
		}
		present[r.Activity] = true
	}
	for _, s := range segs {
		stacks[s.i].up += s.inc
		stacks[s.i].down -= s.cost
	}
	var extent []float64
	for _, s := range stacks {
		extent = append(extent, s.up, s.down)
	}
	sa := newPFScale(extent...)
	ca := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sa.y(0), First: first, Last: last, ShowZeroLine: true,
		Label: "Income and costs per " + unit + " by activity", HasGaps: anyGap, Gaps: gaps(),
		Low: signedISK(sa.lo), High: signedISK(sa.hi), Sub: "Income above the line, costs below."}
	ups, downs := make([]float64, len(buckets)), make([]float64, len(buckets))
	actRows := make([][]pfTipRow, len(buckets))
	for _, s := range segs {
		cls := pfActivityClass[s.act]
		x := float64(s.i)*band + (band-bw)/2
		name := s.act
		for _, l := range profitActivityLabels {
			if l.key == s.act {
				name = l.label
			}
		}
		if s.act == actUnattributed {
			name = "Not explained"
		}
		if s.inc > 0 {
			ca.Bars = append(ca.Bars, sa.bar(x, bw, ups[s.i], s.inc, cls, fmt.Sprintf("%s, %s: %s ISK in", label(buckets[s.i]), name, esi.FormatISK(s.inc))))
			ups[s.i] += s.inc
		}
		if s.cost > 0 {
			ca.Bars = append(ca.Bars, sa.bar(x, bw, downs[s.i], -s.cost, cls+" pf-cost", fmt.Sprintf("%s, %s: %s ISK out", label(buckets[s.i]), name, esi.FormatISK(s.cost))))
			downs[s.i] -= s.cost
		}
		if v := s.inc - s.cost; v != 0 {
			actRows[s.i] = append(actRows[s.i], pfTipRow{name, signedISK(v) + " ISK", cls})
		}
	}
	for i := range actRows {
		if len(actRows[i]) > 0 {
			actRows[i] = append(actRows[i], pfTipRow{"Net", signedISK(ups[i]+downs[i]) + " ISK", ""})
		}
	}
	ca.setHits(buckets, band, label, actRows, uncovered)
	ca.setYTicks(sa.lo, sa.hi, sa.y, pfChartH, pfShortISK)
	ca.setXTicks(buckets, band)
	top, topNet := "", math.Inf(-1)
	for _, l := range profitActivityLabels {
		if present[l.key] {
			ca.Legend = append(ca.Legend, pfLegend{Class: pfActivityClass[l.key], Label: l.label, Value: signedISK(netBy[l.key])})
			if netBy[l.key] > topNet {
				top, topNet = l.label, netBy[l.key]
			}
		}
	}
	if present[actUnattributed] {
		ca.Legend = append(ca.Legend, pfLegend{Class: pfActivityClass[actUnattributed], Label: "Not explained"})
	}
	if top != "" {
		ca.Title = fmt.Sprintf("%s brought the most: %s ISK net", top, signedISK(topNet))
		set.Activities = ca
	}

	// 3. Fees by kind, with trading fees as a share of what sold drawn
	// over them on their own (right-hand) percent scale.
	fees, err := app.queries.ListProfitFeeBuckets(ctx, db.ListProfitFeeBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromAt: start})
	if err != nil {
		return nil, err
	}
	type feeRow struct{ tax, broker, industry, lp, revenue float64 }
	fr := make([]feeRow, len(buckets))
	var feeTotal, tradeFees, revenue float64
	for _, f := range fees {
		if i, ok := index[f.Bucket]; ok {
			fr[i] = feeRow{f.SalesTax, f.Broker, f.Industry, f.LpStore, f.Revenue}
			feeTotal += f.SalesTax + f.Broker + f.Industry + f.LpStore
			tradeFees += f.SalesTax + f.Broker
			revenue += f.Revenue
		}
	}
	if feeTotal > 0 {
		var hs []float64
		for _, f := range fr {
			hs = append(hs, f.tax+f.broker+f.industry+f.lp)
		}
		sf := newPFScale(hs...)
		cf := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sf.y(0), First: first, Last: last,
			Label: "Fees per " + unit + ", and trading fees as a share of sales", HasGaps: anyGap, Gaps: gaps(), Low: "0", High: esi.FormatISK(sf.hi),
			Title: fmt.Sprintf("Paid %s ISK in fees", esi.FormatISK(feeTotal))}
		if revenue > 0 {
			cf.Title += fmt.Sprintf("; trading fees took %.1f%% of what sold", tradeFees/revenue*100)
			cf.Sub = "Bars: fees by kind (left scale). Line: sales tax and broker fees as a share of market sales (right scale), with a gap where nothing sold."
		}
		parts := []struct {
			class, name string
			v           func(feeRow) float64
		}{
			{"pf-f-tax", "Sales tax", func(f feeRow) float64 { return f.tax }},
			{"pf-f-broker", "Broker fees", func(f feeRow) float64 { return f.broker }},
			{"pf-f-industry", "Industry fees", func(f feeRow) float64 { return f.industry }},
			{"pf-f-lp", "LP store ISK", func(f feeRow) float64 { return f.lp }},
		}
		share := make([]float64, len(fr))
		shareHi := 0.0
		for i, f := range fr {
			share[i] = math.NaN()
			if f.revenue > 0 {
				share[i] = (f.tax + f.broker) / f.revenue * 100
				shareHi = math.Max(shareHi, share[i])
			}
		}
		shown := map[string]float64{}
		feeRows := make([][]pfTipRow, len(buckets))
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
		set.Fees = cf
	}

	// 4. Cost of goods sold: per bucket, what the covered sales brought
	// and, inside it, what those goods had cost; the rest is gross
	// margin before tax and fees.
	cogs, err := app.queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return nil, err
	}
	type cogsRow struct{ sold, covRev, cost float64 }
	cr := make([]cogsRow, len(buckets))
	var covRev, cost, sold float64
	for _, r := range cogs {
		if i, ok := index[r.Bucket]; ok {
			cr[i] = cogsRow{r.Revenue, r.CovRevenue, r.CovCost}
			covRev, cost, sold = covRev+r.CovRevenue, cost+r.CovCost, sold+r.Revenue
		}
	}
	if covRev > 0 {
		var hs []float64
		for _, r := range cr {
			hs = append(hs, r.covRev)
		}
		sg := newPFScale(hs...)
		cg := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sg.y(0), First: first, Last: last,
			Label: "Trading sales and the cost of the goods sold per " + unit, HasGaps: anyGap, Gaps: gaps(),
			Title: fmt.Sprintf("Goods sold cost %s ISK: %.0f%% of covered sales", esi.FormatISK(cost), cost/covRev*100),
			Sub:   "Trading sales with a known cost. The darker part of each bar is what those goods cost; the rest is gross margin, before tax and fees.",
			Legend: []pfLegend{{Class: "pf-sold", Label: "Covered sales", Value: esi.FormatISK(covRev)},
				{Class: "pf-cogs", Label: "Cost of goods", Value: esi.FormatISK(cost)}}}
		cogsRows := make([][]pfTipRow, len(buckets))
		for i, r := range cr {
			x := float64(i)*band + (band-bw)/2
			if r.covRev > 0 {
				cg.Bars = append(cg.Bars, sg.bar(x, bw, 0, r.covRev, "pf-sold", ""))
				cg.Bars = append(cg.Bars, sg.bar(x, bw, 0, math.Min(r.cost, r.covRev), "pf-cogs", ""))
				gross := r.covRev - r.cost
				cogsRows[i] = []pfTipRow{
					{"Covered sales", esi.FormatISK(r.covRev) + " ISK", "pf-sold"},
					{"Cost of goods", esi.FormatISK(r.cost) + " ISK", "pf-cogs"},
					{"Gross margin", fmt.Sprintf("%s ISK, %.1f%%", signedISK(gross), gross/r.covRev*100), ""},
				}
			}
			if r.sold > r.covRev+0.5 {
				cogsRows[i] = append(cogsRows[i], pfTipRow{"Sold with no known cost", esi.FormatISK(r.sold-r.covRev) + " ISK", ""})
			}
		}
		cg.setHits(buckets, band, label, cogsRows, uncovered)
		cg.setYTicks(0, sg.hi, sg.y, pfChartH, pfShortISK)
		cg.setXTicks(buckets, band)
		set.COGS = cg
	}

	// 5. The trading margin over the last 30 days, day by day, on
	// covered sales only: solid where every unit sold in those 30 days
	// is covered, dashed where some are not (the margin then speaks
	// for the covered part alone), absent where nothing covered sold.
	if len(cov) > 0 {
		days := pfBuckets(fromDay, "day", now)
		byDay := map[string]db.ListTradeCoverageDailyRow{}
		for _, r := range cov {
			byDay[r.Day] = r
		}
		full := make([]float64, len(days))
		part := make([]float64, len(days))
		hi, lo := 0.0, 0.0
		lastMargin, lastShare := math.NaN(), 1.0
		marginRows := make([][]pfTipRow, len(days))
		for i, d := range days {
			full[i], part[i] = math.NaN(), math.NaN()
			marginRows[i] = []pfTipRow{{"30-day margin", "no covered sales", ""}}
			day, _ := time.Parse("2006-01-02", d)
			var units, covUnits int64
			var rev, cost float64
			for k := 0; k < 30; k++ {
				r, ok := byDay[day.AddDate(0, 0, -k).Format("2006-01-02")]
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
				continue
			}
			m := (rev - cost) / rev * 100
			hi, lo = math.Max(hi, m), math.Min(lo, m)
			lastMargin, lastShare = m, float64(covUnits)/float64(units)
			marginRows[i] = []pfTipRow{{"30-day margin", fmt.Sprintf("%.1f%%", m), "pf-roll"},
				{"Covered sales", esi.FormatISK(rev) + " ISK", ""},
				{"Units covered", fmt.Sprintf("%.0f%%", math.Floor(lastShare*100)), ""}}
			if covUnits >= units {
				full[i] = m
			} else {
				part[i] = m
			}
		}
		// Each run joins the next at the day the coverage changed, so
		// the line reads as one.
		for i := 1; i < len(days); i++ {
			switch {
			case !math.IsNaN(full[i]) && !math.IsNaN(part[i-1]):
				part[i] = full[i]
			case !math.IsNaN(part[i]) && !math.IsNaN(full[i-1]):
				full[i] = part[i]
			}
		}
		if math.IsNaN(lastMargin) && hi == 0 && lo == 0 {
			return set, nil
		}
		// A margin line fits its own range; zero stays in view only when
		// a margin went below it.
		lo, hi = math.Inf(1), math.Inf(-1)
		for i := range days {
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
		dband, _ := pfBand(len(days))
		cm := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sm.y(0), First: days[0], Last: days[len(days)-1], ShowZeroLine: lo < 0,
			Label: "Trading margin over the last 30 days", Low: fmt.Sprintf("%.1f%%", sm.lo), High: fmt.Sprintf("%.1f%%", sm.hi),
			Sub:    "On covered sales only. Dashed where some units sold in those 30 days had no known cost or tax line, so the margin covers the rest.",
			Legend: []pfLegend{{Class: "pf-roll", Label: "Every sale covered"}, {Class: "pf-dash", Label: "Some sales not covered"}}}
		cm.Lines = append(pfBrokenLines(full, dband, sm.y, "pf-margin"), pfBrokenLines(part, dband, sm.y, "pf-margin pf-margin-part")...)
		cm.setHits(days, dband, func(s string) string { return s }, marginRows, nil)
		cm.setYTicks(sm.lo, sm.hi, sm.y, pfChartH, func(v float64) string { return fmt.Sprintf("%.1f%%", v) })
		cm.setXTicks(days, dband)
		cm.Title = fmt.Sprintf("30-day trading margin: %.1f%%", lastMargin)
		if lastShare < 1 {
			cm.Title += fmt.Sprintf(" on the %.0f%% of units covered", math.Floor(lastShare*100))
		}
		set.Margin = cm
	}
	return set, nil
}

// pfBrokenLines draws a series as line segments, breaking at missing
// values (NaN) rather than drawing through them.
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

// pfShortISK writes an axis amount short: "60M", "-130.5M", "2.5B".
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

// pfNiceStep rounds a raw step up to 1, 2 or 5 times a power of ten.
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

// setYTicks labels four or five round values across a scale. y maps
// a value to plot units; h is the plot's height in those units.
func (c *pfChart) setYTicks(lo, hi float64, y func(float64) float64, h int, format func(float64) string) {
	step := pfNiceStep((hi - lo) / 4)
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		yy := y(v)
		c.YTicks = append(c.YTicks, pfTick{Y: yy, Pos: strconv.Itoa(int(math.Round(yy / float64(h) * 100))), Label: format(v)})
	}
}

// setXTicks labels the first, middle and last bucket by date.
func (c *pfChart) setXTicks(buckets []string, band float64) {
	if len(buckets) == 0 {
		return
	}
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
		c.XTicks = append(c.XTicks, pfTick{Pos: strconv.Itoa(int(math.Round(x / float64(c.W) * 100))), Label: label, Class: classes[k]})
	}
}

// setHits gives every bucket its hover column, the tip heading and
// rows, and a row saying when the bucket's trading is partly covered.
func (c *pfChart) setHits(buckets []string, band float64, label func(string) string, rows [][]pfTipRow, uncovered []bool) {
	for i, b := range buckets {
		r := rows[i]
		if i < len(uncovered) && uncovered[i] {
			r = append(r, pfTipRow{Label: "Partly covered", Value: "some sales lack a cost or tax line", Class: "pf-gap"})
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
		c.Hits = append(c.Hits, pfHit{X: float64(i) * band, W: band, Date: heading, Label: heading, Rows: string(js)})
	}
}

// pfDonut is the income split by activity: one ring segment per
// activity that brought ISK in, largest first, drawn as dashes on one
// circle (no path arithmetic), with the site's chart tip on each.
type pfDonut struct {
	Total  string
	Slices []pfSlice
}

type pfSlice struct {
	Class, Dash, Offset string
	Label, Value, Share string
	Rows                string
}

// pfDonutCirc is the ring's circumference at radius 60.
const pfDonutCirc = 2 * math.Pi * 60

// buildIncomeDonut splits income by activity. Income only: costs and
// the unexplained remainder are not parts of a whole.
func buildIncomeDonut(acts []db.ListProfitByActivityRow) *pfDonut {
	type part struct {
		key, label  string
		income, net float64
	}
	var parts []part
	total := 0.0
	for _, l := range profitActivityLabels {
		for _, a := range acts {
			if a.Activity == l.key && a.Income > 0 {
				parts = append(parts, part{l.key, l.label, a.Income, a.Income - a.Costs})
				total += a.Income
			}
		}
	}
	if total <= 0 || len(parts) < 2 {
		return nil
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].income > parts[j].income })
	d := &pfDonut{Total: pfShortISK(total)}
	offset := 0.0
	for _, p := range parts {
		share := p.income / total
		length := share * pfDonutCirc
		rows, _ := json.Marshal([][3]string{
			{"Income", esi.FormatISK(p.income) + " ISK", pfActivityClass[p.key]},
			{"Share of income", fmt.Sprintf("%.1f%%", share*100), ""},
			{"Net after costs", signedISK(p.net) + " ISK", ""},
		})
		d.Slices = append(d.Slices, pfSlice{
			Class: pfActivityClass[p.key], Label: p.label, Value: esi.FormatISK(p.income), Share: fmt.Sprintf("%.0f%%", share*100),
			Dash:   strconv.FormatFloat(math.Max(length-1, 0.5), 'f', 2, 64) + " " + strconv.FormatFloat(pfDonutCirc, 'f', 2, 64),
			Offset: strconv.FormatFloat(-offset, 'f', 2, 64),
			Rows:   string(rows),
		})
		offset += length
	}
	return d
}

// setYTicksRight labels a second value scale on the plot's right edge
// (no gridlines: the left scale owns those).
func (c *pfChart) setYTicksRight(lo, hi float64, y func(float64) float64, h int, format func(float64) string) {
	step := pfNiceStep((hi - lo) / 4)
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		yy := y(v)
		c.YTicksR = append(c.YTicksR, pfTick{Y: yy, Pos: strconv.Itoa(int(math.Round(yy / float64(h) * 100))), Label: format(v)})
	}
}
