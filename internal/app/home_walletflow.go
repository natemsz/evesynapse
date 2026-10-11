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
	"sort"
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
	Chart    balanceChart
	HasChart bool
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
	sort.SliceStable(w.Chars, func(i, j int) bool { return w.Chars[i].Name < w.Chars[j].Name })
	var days []time.Time
	for d := range dayNet {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	var points []balancePoint
	var running float64
	for _, d := range days {
		running += dayNet[d]
		points = append(points, balancePoint{At: d, Balance: running})
	}
	if chart, ok := buildBalanceChart(points); ok {
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
