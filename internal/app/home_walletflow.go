package app

// ---------------------------------------------------------------------------
// Wallet flow: income vs spend across all characters from the
// stored wallet journals. Balance tells what there is; flow
// tells whether it is growing. A cumulative net-flow line (the
// theme's balance chart), 7/30-day totals, top sources and
// sinks by journal ref type, and a per-character breakdown
// with the standard client-side filter. Rendered reads only:
// journals are newest-first, so each character stops at the
// window's edge.
// ---------------------------------------------------------------------------

import (
	"context"
	"evesynapse/internal/balancechart"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// walletFlowDays is the chart and totals window; walletFlowWeek
// is the short totals window.
const (
	walletFlowDays = 30
	walletFlowWeek = 7
)

// walletFlowTop caps each sources/sinks list.
const walletFlowTop = 5

// flowTotal is one window's income, spend, and net.
type flowTotal struct {
	In  float64
	Out float64 // negative-or-zero sum, kept signed
}

// flowChar is one character's 7/30-day flow, preformatted:
// templates print strings, never raw floats.
type flowChar struct {
	Name  string
	ID    int64
	In7   string
	Out7  string
	Net7  string
	In30  string
	Out30 string
	Net30 string
	// Search is the client-side filter text.
	Search string
	// w7/w30 carry the window math for tests; the template
	// reads the formatted strings above.
	w7  flowTotal
	w30 flowTotal
}

// flowRef is one ref type's 30-day total (income or spend),
// preformatted for the template.
type flowRef struct {
	Ref   string
	Total string
}

// walletFlowWidget is the flow module.
type walletFlowWidget struct {
	Any      bool
	In7      string
	Out7     string
	Net7     string
	In30     string
	Out30    string
	Net30    string
	Sources  []flowRef
	Sinks    []flowRef
	Chars    []flowChar
	Chart    balancechart.BalanceChart
	HasChart bool
	// InPie and OutPie split the 30-day income and spend by ref
	// type, as share of each side's total; nil when a side is empty.
	InPie  *flowPie
	OutPie *flowPie
}

// flowPie is one donut: the largest ref types and the rest as
// Other, each a ring slice drawn server-side (viewBox 0 0 100 100).
type flowPie struct {
	Slices []flowSlice
}

type flowSlice struct {
	Index int    // palette class, pie-s0..pie-s5 (5 is Other)
	Label string // the ref type, readable
	Pct   string // "42.1%"
	D     string // SVG path of the ring slice
}

// buildWalletFlow assembles the widget from every character's
// stored journal window.
func (app *Application) buildWalletFlow(ctx context.Context, bundles []*charSnaps) *walletFlowWidget {
	w := &walletFlowWidget{}
	now := time.Now().UTC()
	dayStart := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	edge30 := now.Add(-walletFlowDays * 24 * time.Hour)
	edge7 := now.Add(-walletFlowWeek * 24 * time.Hour)
	dayNet := make(map[time.Time]float64)
	refIn := make(map[string]float64)
	refOut := make(map[string]float64)
	for _, b := range bundles {
		if !b.journalKnown {
			continue
		}
		w.Any = true
		fc := flowChar{Name: b.ch.Name, ID: b.ch.CharacterID, Search: b.ch.Name}
		for _, e := range b.journal {
			at, err := time.Parse(time.RFC3339, e.Date)
			if err != nil || at.After(now) {
				continue
			}
			if at.Before(edge30) {
				break // newest-first: older rows only follow
			}
			dayNet[dayStart(at)] += e.Amount
			fc.w30.add(e.Amount)
			if !at.Before(edge7) {
				fc.w7.add(e.Amount)
			}
			if e.Amount > 0 {
				refIn[e.RefType] += e.Amount
			} else if e.Amount < 0 {
				refOut[e.RefType] += e.Amount
			}
		}
		w.Chars = append(w.Chars, fc)
	}
	return app.finishWalletFlow(w, dayNet, refIn, refOut)
}

// add folds one journal amount into the window total.
func (t *flowTotal) add(amount float64) {
	if amount > 0 {
		t.In += amount
	} else {
		t.Out += amount
	}
}

// finishWalletFlow totals the widget, ranks sources/sinks, and
// builds the cumulative net-flow chart.
func (app *Application) finishWalletFlow(w *walletFlowWidget, dayNet map[time.Time]float64, refIn, refOut map[string]float64) *walletFlowWidget {
	var in7, out7, in30, out30 float64
	for i := range w.Chars {
		fc := &w.Chars[i]
		fc.In7 = esi.FormatISK(fc.w7.In)
		fc.Out7 = esi.FormatISK(fc.w7.Out)
		fc.Net7 = esi.FormatISK(fc.w7.In + fc.w7.Out)
		fc.In30 = esi.FormatISK(fc.w30.In)
		fc.Out30 = esi.FormatISK(fc.w30.Out)
		fc.Net30 = esi.FormatISK(fc.w30.In + fc.w30.Out)
		in7 += fc.w7.In
		out7 += fc.w7.Out
		in30 += fc.w30.In
		out30 += fc.w30.Out
	}
	w.In7 = esi.FormatISK(in7)
	w.Out7 = esi.FormatISK(out7)
	w.Net7 = esi.FormatISK(in7 + out7)
	w.In30 = esi.FormatISK(in30)
	w.Out30 = esi.FormatISK(out30)
	w.Net30 = esi.FormatISK(in30 + out30)
	w.Sources = topFlowRefs(refIn, false)
	w.Sinks = topFlowRefs(refOut, true)
	w.InPie = buildFlowPie(refIn)
	w.OutPie = buildFlowPie(refOut)
	sort.SliceStable(w.Chars, func(i, j int) bool { return w.Chars[i].Name < w.Chars[j].Name })
	var days []time.Time
	for d := range dayNet {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	var points []balancechart.BalancePoint
	var running float64
	for _, d := range days {
		running += dayNet[d]
		points = append(points, balancechart.BalancePoint{At: d, Balance: running})
	}
	if chart, ok := balancechart.BuildBalanceChart(points); ok {
		chart.Label = "Cumulative net flow, ISK per day"
		chart.LegendNote = "Net income minus spend, per day"
		w.Chart = chart
		w.HasChart = true
	}
	return w
}

// topFlowRefs ranks ref-type totals: largest income first, or
// largest spend (most negative) first when spend. Totals are
// formatted on the way out; the template never sees floats.
func topFlowRefs(totals map[string]float64, spend bool) []flowRef {
	type ranked struct {
		ref   string
		total float64
	}
	var all []ranked
	for ref, total := range totals {
		if total == 0 {
			continue
		}
		all = append(all, ranked{ref: ref, total: total})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if spend {
			return all[i].total < all[j].total
		}
		return all[i].total > all[j].total
	})
	var out []flowRef
	for i, r := range all {
		if i >= walletFlowTop {
			break
		}
		out = append(out, flowRef{Ref: r.ref, Total: esi.FormatISK(r.total)})
	}
	return out
}

// flowPieSlices is how many ref types a donut names before the
// rest fold into Other.
const flowPieSlices = 5

// buildFlowPie splits one side's ref-type totals into donut slices
// by share of the side's total (spend totals are negative; their
// size is what counts). nil when the side moved nothing.
func buildFlowPie(totals map[string]float64) *flowPie {
	type part struct {
		ref string
		amt float64
	}
	var parts []part
	var sum float64
	for ref, v := range totals {
		if v < 0 {
			v = -v
		}
		if v == 0 {
			continue
		}
		parts = append(parts, part{ref, v})
		sum += v
	}
	if sum == 0 {
		return nil
	}
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].amt != parts[j].amt {
			return parts[i].amt > parts[j].amt
		}
		return parts[i].ref < parts[j].ref
	})
	if len(parts) > flowPieSlices {
		var other float64
		for _, p := range parts[flowPieSlices:] {
			other += p.amt
		}
		parts = append(parts[:flowPieSlices], part{"Other", other})
	}
	pie := &flowPie{}
	at := 0.0 // fraction of the ring already drawn
	for i, p := range parts {
		frac := p.amt / sum
		idx := i
		if p.ref == "Other" {
			idx = flowPieSlices
		}
		pie.Slices = append(pie.Slices, flowSlice{
			Index: idx,
			Label: strings.ReplaceAll(p.ref, "_", " "),
			Pct:   fmt.Sprintf("%.1f%%", 100*frac),
			D:     ringSlicePath(at, at+frac),
		})
		at += frac
	}
	return pie
}

// ringSlicePath draws the ring between fractions a and b of a full
// turn, clockwise from twelve o'clock: outer radius 46, inner 28,
// centred on (50,50). A whole ring is drawn as two halves, since a
// single arc cannot close on itself.
func ringSlicePath(a, b float64) string {
	if b-a >= 0.9999 {
		return ringSlicePath(a, a+0.5) + " " + ringSlicePath(a+0.5, a+1)
	}
	const cx, cy, ro, ri = 50.0, 50.0, 46.0, 28.0
	pt := func(f, r float64) (float64, float64) {
		ang := 2*math.Pi*f - math.Pi/2
		return cx + r*math.Cos(ang), cy + r*math.Sin(ang)
	}
	large := 0
	if b-a > 0.5 {
		large = 1
	}
	x0, y0 := pt(a, ro)
	x1, y1 := pt(b, ro)
	x2, y2 := pt(b, ri)
	x3, y3 := pt(a, ri)
	return fmt.Sprintf("M%.2f %.2fA%.0f %.0f 0 %d 1 %.2f %.2fL%.2f %.2fA%.0f %.0f 0 %d 0 %.2f %.2fZ",
		x0, y0, ro, ro, large, x1, y1, x2, y2, ri, ri, large, x3, y3)
}
