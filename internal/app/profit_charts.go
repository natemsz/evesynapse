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
	Gaps         []pfBar
	Legend       []pfLegend
	Low, High    string
	First, Last  string
	HasGaps      bool
	ShowZeroLine bool
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
	Net, Activities, Fees, FeeShare, Characters, Margin *pfChart
	Bucket                                              string // "day" or "week"
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

	// 1. Net per bucket, with a rolling average over 7 days or 4 weeks.
	net := make([]float64, len(buckets))
	for _, r := range rows {
		if i, ok := index[r.Bucket]; ok && r.Activity != actUnattributed {
			net[i] += r.Income - r.Costs
		}
	}
	window := 7
	if unit == "week" {
		window = 4
	}
	roll := make([]float64, len(net))
	var total, best float64
	bestAt := 0
	for i := range net {
		total += net[i]
		if net[i] > best {
			best, bestAt = net[i], i
		}
		lo, sum := max(0, i-window+1), 0.0
		for j := lo; j <= i; j++ {
			sum += net[j]
		}
		roll[i] = sum / float64(i-lo+1)
	}
	sc := newPFScale(append(append([]float64{}, net...), roll...)...)
	c := &pfChart{W: pfChartW, H: pfChartH, ZeroY: sc.y(0), First: first, Last: last, ShowZeroLine: true,
		Title: fmt.Sprintf("Earned %s ISK over %d days", signedISK(total), view.Days),
		Label: "Net earned per " + unit, HasGaps: anyGap, Gaps: gaps(),
		Low: signedISK(sc.lo), High: signedISK(sc.hi)}
	if best > 0 {
		c.Sub = fmt.Sprintf("Best %s: %s, %s ISK.", unit, label(buckets[bestAt]), esi.FormatISK(best))
	}
	xs, ys := make([]float64, len(roll)), make([]float64, len(roll))
	for i, v := range net {
		cls := "pf-gain"
		if v < 0 {
			cls = "pf-loss"
		}
		c.Bars = append(c.Bars, sc.bar(float64(i)*band+(band-bw)/2, bw, 0, v, cls, fmt.Sprintf("%s: %s ISK", label(buckets[i]), signedISK(v))))
		xs[i], ys[i] = float64(i)*band+band/2, sc.y(roll[i])
	}
	c.Lines = []pfLine{{Points: pfPoints(xs, ys), Class: "pf-roll"}}
	c.Legend = []pfLegend{{Class: "pf-gain", Label: "Gain"}, {Class: "pf-loss", Label: "Loss"},
		{Class: "pf-roll", Label: fmt.Sprintf("Average over %d %ss", window, unit)}}
	set.Net = c

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
	}
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

	// 3. Fees, and trading fees as a share of what sold.
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
			Label: "Fees per " + unit, HasGaps: anyGap, Gaps: gaps(), Low: "0", High: esi.FormatISK(sf.hi),
			Title: fmt.Sprintf("Paid %s ISK in fees", esi.FormatISK(feeTotal))}
		parts := []struct {
			class, name string
			v           func(feeRow) float64
		}{
			{"pf-f-tax", "Sales tax", func(f feeRow) float64 { return f.tax }},
			{"pf-f-broker", "Broker fees", func(f feeRow) float64 { return f.broker }},
			{"pf-f-industry", "Industry fees", func(f feeRow) float64 { return f.industry }},
			{"pf-f-lp", "LP store ISK", func(f feeRow) float64 { return f.lp }},
		}
		shown := map[string]float64{}
		for i, f := range fr {
			base := 0.0
			for _, p := range parts {
				if v := p.v(f); v > 0 {
					cf.Bars = append(cf.Bars, sf.bar(float64(i)*band+(band-bw)/2, bw, base, v, p.class, fmt.Sprintf("%s, %s: %s ISK", label(buckets[i]), p.name, esi.FormatISK(v))))
					base += v
					shown[p.class] += v
				}
			}
		}
		for _, p := range parts {
			if shown[p.class] > 0 {
				cf.Legend = append(cf.Legend, pfLegend{Class: p.class, Label: p.name, Value: esi.FormatISK(shown[p.class])})
			}
		}
		set.Fees = cf

		if revenue > 0 {
			share := make([]float64, len(fr))
			hi := 0.0
			for i, f := range fr {
				share[i] = math.NaN()
				if f.revenue > 0 {
					share[i] = (f.tax + f.broker) / f.revenue * 100
					hi = math.Max(hi, share[i])
				}
			}
			ss := newPFScale(0, hi)
			cs := &pfChart{W: pfChartW, H: pfChartH / 2, First: first, Last: last, Label: "Trading fees as a share of sales per " + unit,
				Low: "0%", High: fmt.Sprintf("%.1f%%", ss.hi),
				Title: fmt.Sprintf("Trading fees took %.1f%% of what sold", tradeFees/revenue*100),
				Sub:   "Sales tax and broker fees against market sales; a gap where nothing sold."}
			cs.Lines = pfBrokenLines(share, band, func(v float64) float64 { return ss.y(v) / 2 }, "pf-share")
			set.FeeShare = cs
		}
	}

	// 4. A running line per character.
	if kind == ownerChar && len(ids) > 1 {
		owners, err := app.queries.ListProfitOwnerBuckets(ctx, db.ListProfitOwnerBucketsParams{Unit: unit, OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
		if err != nil {
			return nil, err
		}
		per := map[int64][]float64{}
		for _, o := range owners {
			if per[o.OwnerID] == nil {
				per[o.OwnerID] = make([]float64, len(buckets))
			}
			if i, ok := index[o.Bucket]; ok {
				per[o.OwnerID][i] += o.Net
			}
		}
		if len(per) > 1 {
			order := make([]int64, 0, len(per))
			var all []float64
			for id, s := range per {
				order = append(order, id)
				run := 0.0
				for i := range s {
					run += s[i]
					s[i] = run
				}
				all = append(all, s...)
			}
			sort.Slice(order, func(i, j int) bool { return per[order[i]][len(buckets)-1] > per[order[j]][len(buckets)-1] })
			so := newPFScale(all...)
			co := &pfChart{W: pfChartW, H: pfChartH, ZeroY: so.y(0), First: first, Last: last, ShowZeroLine: true,
				Label: "Running earned per character", HasGaps: anyGap, Gaps: gaps(), Low: signedISK(so.lo), High: signedISK(so.hi),
				Title: fmt.Sprintf("%s earned the most: %s ISK", names[order[0]], signedISK(per[order[0]][len(buckets)-1])),
				Sub:   "Each sale counts for the character that sold, whoever bought the stock."}
			for n, id := range order {
				cls := fmt.Sprintf("pf-ch pf-ch-%d", n%6+1)
				xs, ys := make([]float64, len(buckets)), make([]float64, len(buckets))
				for i, v := range per[id] {
					xs[i], ys[i] = float64(i)*band+band/2, so.y(v)
				}
				co.Lines = append(co.Lines, pfLine{Points: pfPoints(xs, ys), Class: cls})
				co.Legend = append(co.Legend, pfLegend{Class: cls, Label: names[id], Value: signedISK(per[id][len(buckets)-1])})
			}
			set.Characters = co
		}
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
		for i, d := range days {
			full[i], part[i] = math.NaN(), math.NaN()
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
