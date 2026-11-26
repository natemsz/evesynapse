package app

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetWalletFlow, Title: "Wallet Flow", Category: wcatMoney, Feature: "wallet",
		Requires:    "wallet",
		Href:        "/wallet/",
		Description: "Income against spend over 7 and 30 days, where it came from and went, and a per-character breakdown.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The net over 30 days, with what came in and went out."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The 7- and 30-day net beside the running net."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "The net, in against out for 7 and 30 days, and the running net."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The 7- and 30-day table, how income and spend split, and the running net."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds the top sources and sinks and each character's net."},
		},
		Needs:   []string{esi.SnapWalletJournal},
		Options: []widgetOption{optScope()},
		Build:   buildWalletFlowWidget,
	})
}

// walletFlowView is what the Wallet Flow forms draw: the journal
// window's figures (the Wallet page's own builder) and the parts each
// form shows, built only for the forms this render draws.
type walletFlowView struct {
	*walletFlowWidget
	Lead     wLead
	InOut    string      // wide: in and out over 30 days on one line
	Meters   []flowMeter // small, tall: in and out over 30 days
	Meters7  []flowMeter // tall: in and out over 7 days, drawn when there is room
	Figs     []flowFig   // medium, large: in, out and net over 7 and 30 days
	Line     *wChart     // the running net over 30 days
	InSplit  *wSegbar    // medium: income by kind
	OutSplit *wSegbar    // medium: spend by kind
	Sources  *wRanked    // large
	Sinks    *wRanked    // large
	ByChar   *wRanked    // large: each character's net over 30 days
	Empty    wEmpty
}

// flowFig is one row of the figures table: in, out or net over 7
// and 30 days, short, with the whole figures as titles.
type flowFig struct {
	Name                string
	Week, Month         string
	WeekFull, MonthFull string
}

// flowMeter is one income or spend row: its bar against the largest
// figure beside it, in the income or the spend colour (Tone).
type flowMeter struct {
	Name string
	Pct  float64
	Text string
	Tone string
}

// flowSplitParts is how many kinds a split names before the rest
// fold into Other: five, as live listed them, each with its share.
const flowSplitParts = 5

func buildWalletFlowWidget(ctx context.Context, app *Application, in *widgetInput) any {
	f := app.buildWalletFlow(in.Bundles)
	v := &walletFlowView{walletFlowWidget: f, Empty: wEmpty{Text: "Journal data appears here once your characters have synced."}}
	if !f.Any {
		return v
	}
	net30, net7 := f.in30+f.out30, f.in7+f.out7
	v.Lead = wLead{Label: "Net, 30 days", Value: iskCompactSigned(net30), Unit: "ISK", Full: f.Net30 + " ISK in less out over 30 days"}
	v.Lead.Sub = iskCompactSigned(net7) + " ISK over 7 days"
	v.InOut = "In " + iskCompactSigned(f.in30) + ", out " + iskCompactSigned(f.out30)

	meter := func(name string, val, top float64) flowMeter {
		m := flowMeter{Name: name, Text: iskCompactSigned(val), Tone: "w-s1"}
		if val < 0 {
			m.Tone = "w-s2"
		}
		if top > 0 {
			m.Pct = math.Abs(val) / top * 100
		}
		return m
	}
	if in.Draws("small") || in.Draws("tall") {
		top := math.Max(f.in30, -f.out30)
		if in.Draws("tall") {
			top = math.Max(top, math.Max(f.in7, -f.out7))
		}
		v.Meters = []flowMeter{meter("In, 30 days", f.in30, top), meter("Out, 30 days", f.out30, top)}
		if in.Draws("tall") {
			v.Meters7 = []flowMeter{meter("In, 7 days", f.in7, top), meter("Out, 7 days", f.out7, top)}
		}
	}
	if in.Draws("medium") || in.Draws("large") {
		fig := func(name string, week, month float64) flowFig {
			return flowFig{Name: name, Week: iskCompactSigned(week), Month: iskCompactSigned(month),
				WeekFull: signedISK(week) + " ISK", MonthFull: signedISK(month) + " ISK"}
		}
		v.Figs = []flowFig{fig("In", f.in7, f.in30), fig("Out", f.out7, f.out30), fig("Net", net7, net30)}
	}
	if in.Draws("wide") || in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
		v.Line, _ = wLineChart("Running net, income less spend, over 30 days", "Running net", 1, flowRunningDays(f, in.Now))
	}
	if in.Draws("medium") {
		v.InSplit, _ = wSegments("Income, 30 days", flowSplit(f.refIn))
		v.OutSplit, _ = wSegments("Spend, 30 days", flowSplit(f.refOut))
	}
	if in.Draws("large") {
		v.Sources, _ = wRankedBars("Top sources, 30 days", flowSorted(f.refIn), walletFlowTop)
		v.Sinks, _ = wRankedBars("Top sinks, 30 days", flowSorted(f.refOut), walletFlowTop)
		mine := make(map[int64]bool, len(in.Chars))
		for _, ch := range in.Chars {
			mine[ch.CharacterID] = true
		}
		var chars []wRankItem
		for _, c := range f.Chars {
			net := c.w30.In + c.w30.Out
			chars = append(chars, wRankItem{Name: c.Name, Link: charLink(mine, c.ID, c.Name), Value: math.Abs(net),
				Text: iskCompactSigned(net), Full: signedISK(net) + " ISK net over 30 days"})
		}
		sort.SliceStable(chars, func(i, j int) bool { return chars[i].Value > chars[j].Value })
		v.ByChar, _ = wRankedBars("Net over 30 days by character", chars, len(chars))
	}
	return v
}

// flowRunningDays is the running net on every day of the 30-day
// window, carried over days with no entries.
func flowRunningDays(f *walletFlowWidget, now time.Time) []wDay {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	at := make(map[string]float64, len(f.netDays))
	for _, p := range f.netDays {
		at[p.At.Format("2006-01-02")] = p.Balance
	}
	// The window is the last 30 days counted back from this hour, so
	// its first calendar day is the one 30 days ago.
	edge := now.Add(-walletFlowDays * 24 * time.Hour)
	start := time.Date(edge.Year(), edge.Month(), edge.Day(), 0, 0, 0, 0, time.UTC)
	days := make([]wDay, 0, walletFlowDays+1)
	running := 0.0
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		if b, ok := at[d.Format("2006-01-02")]; ok {
			running = b
		}
		days = append(days, wDay{At: d, Values: []float64{running}})
	}
	return days
}

// flowRefName writes a journal ref type as words: "Bounty prizes".
func flowRefName(ref string) string {
	s := strings.ReplaceAll(ref, "_", " ")
	if s == "" {
		return "Other"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// flowSorted is one side's ref types, largest first (spend by size),
// ties by name.
func flowSorted(totals map[string]float64) []wRankItem {
	var items []wRankItem
	for ref, total := range totals {
		a := math.Abs(total)
		if a == 0 {
			continue
		}
		items = append(items, wRankItem{Name: flowRefName(ref), Value: a, Text: iskCompactSigned(total), Full: signedISK(total) + " ISK"})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Value != items[j].Value {
			return items[i].Value > items[j].Value
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// flowSplit is one side's ref types as parts of its whole: the
// largest flowSplitParts in the categorical colours, the rest as
// Other in the neutral one.
func flowSplit(totals map[string]float64) []wPart {
	items := flowSorted(totals)
	var parts []wPart
	other := 0.0
	for i, it := range items {
		if i >= flowSplitParts {
			other += it.Value
			continue
		}
		parts = append(parts, wPart{Name: it.Name, Slot: i + 1, Value: it.Value, Text: iskCompact(it.Value), Full: esi.FormatISK(it.Value) + " ISK"})
	}
	if other > 0 {
		parts = append(parts, wPart{Name: "Other", Slot: 0, Value: other, Text: iskCompact(other), Full: esi.FormatISK(other) + " ISK"})
	}
	return parts
}
