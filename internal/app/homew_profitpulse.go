package app

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetProfitPulse, Title: "Profit pulse", Category: wcatMoney, Feature: "market",
		Href:        "/profit/",
		Description: "The Profit Analysis headline: what came in, what you kept each day, and the items that earned the most.",
		Default:     wSize{2, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "What you kept, its change on the window before, and a strip of the days."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "What you kept and the gross in beside the kept profit of each day."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "What you kept, the gross in and the top item over the kept profit of each day."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The figures, the kept profit of each day and the top three items."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds where the ISK went and the profit of each activity."},
		},
		Options: []widgetOption{
			optChoice("window", "Window", "30", "7:7 days", "30:30 days", "90:90 days"),
		},
		Build: buildProfitPulseWidget,
	})
}

// profitPulseView is what the Profit Pulse forms draw: the Profit
// Analysis figures over the window, from the stored ledger tables the
// worker folds (profit_daily for the figures and the days,
// trade_item_window for the items, trade_item_daily for the
// waterfall's cost of goods). Parts are built only for the forms this
// render draws.
type profitPulseView struct {
	HasData    bool
	Window     string   // "30 days"
	Lead       wLead    // kept as profit, and its change on the window before
	WideLead   wLead    // wide: kept as profit, out of the gross in
	Gross      string   // gross in, short
	GrossFull  string   // gross in, whole
	Tiles      []wTile  // medium, large
	Strip      *wBars   // small: kept profit per day
	Days       *wBars   // wide, tall, medium, large: kept profit per day
	TopItem    *wRanked // tall: the item that earned the most
	TopItems   *wRanked // medium: the three that earned the most
	Activities *wRanked // large: kept profit by activity
	Waterfall  *pfWaterfall
	WfTable    wTable // the waterfall's table twin
	Empty      wEmpty
}

// profitPulseTop is how many items the medium form ranks.
const profitPulseTop = 3

func buildProfitPulseWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &profitPulseView{Empty: wEmpty{Text: "No profit yet. Once your characters' sales, jobs and other income are in the ledger, the figures show here."}}
	if len(in.Chars) == 0 {
		return v
	}
	days, _ := strconv.Atoi(in.Opts.Get("window"))
	if days <= 0 {
		days = 30
	}
	v.Window = strconv.Itoa(days) + " days"
	ids := make([]int64, 0, len(in.Chars))
	for _, ch := range in.Chars {
		ids = append(ids, ch.CharacterID)
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	fromDay := now.AddDate(0, 0, -days+1).Format("2006-01-02")
	prevDay := now.AddDate(0, 0, -2*days+1).Format("2006-01-02")

	acts, err := app.queries.ListProfitByActivity(ctx, db.ListProfitByActivityParams{OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		logging.Errorf("profitpulse: profit by activity for user %d: %v", in.UserID, err)
		return v
	}
	// The page's own totals: the labeled activities only. The
	// unattributed remainder is not an activity and stays out of the
	// figures, exactly as on the Profit page.
	gross, kept, has := profitLabeledTotals(acts)
	v.HasData = has
	var industryCosts float64
	byAct := map[string]float64{}
	for _, a := range acts {
		if !profitActivityLabeled(a.Activity) {
			continue
		}
		byAct[a.Activity] += a.Income - a.Costs
		if a.Activity == actIndustry {
			industryCosts = a.Costs
		}
	}
	if !v.HasData {
		return v
	}
	v.Gross, v.GrossFull = iskCompact(gross)+" ISK", esi.FormatISK(gross)+" ISK"
	v.Lead = wLead{Label: "Kept as profit", Value: signedCompact(kept), Unit: "ISK", Full: signedISK(kept) + " ISK kept over the last " + v.Window, UpIsGood: true, Sub: "over " + v.Window}

	// The window before: the query reads from a day on, so the span
	// of both windows less this one is the one before.
	prevKept, prevAny := 0.0, false
	if both, err := app.queries.ListProfitByActivity(ctx, db.ListProfitByActivityParams{OwnerKind: ownerChar, OwnerIds: ids, FromDay: prevDay}); err != nil {
		logging.Errorf("profitpulse: previous window for user %d: %v", in.UserID, err)
	} else {
		bothIn, bothKept, _ := profitLabeledTotals(both)
		prevKept = bothKept - kept
		prevAny = bothIn != gross || bothKept != kept
	}
	if prevAny {
		d := kept - prevKept
		v.Lead.Change, v.Lead.Up = signedCompact(d), d >= 0
		v.Lead.ChangeText = signedISK(d) + " ISK on the " + v.Window + " before"
		v.Lead.Sub = "on the " + v.Window + " before"
	}

	v.WideLead = v.Lead
	v.WideLead.Change, v.WideLead.Sub = "", "of "+v.Gross+" gross in"

	if in.Draws("medium") || in.Draws("large") {
		v.Tiles = append(v.Tiles, wTile{Label: "Gross in", Value: iskCompact(gross), Unit: "ISK", Full: v.GrossFull})
		if gross > 0 {
			v.Tiles = append(v.Tiles, wTile{Label: "Kept of gross in", Value: strconv.FormatFloat(kept/gross*100, 'f', 0, 64) + "%", Full: "Kept " + signedISK(kept) + " of " + v.GrossFull})
		}
		if prevAny {
			v.Tiles = append(v.Tiles, wTile{Label: "Kept, the " + v.Window + " before", Value: signedCompact(prevKept), Unit: "ISK", Full: signedISK(prevKept) + " ISK"})
		}
	}

	// Kept profit per day, from the same profit_daily rows the
	// figures total (the labeled activities only, so the bars sum to
	// the kept figure).
	buckets, err := app.queries.ListProfitBuckets(ctx, db.ListProfitBucketsParams{Unit: "day", OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		logging.Errorf("profitpulse: daily buckets for user %d: %v", in.UserID, err)
	} else {
		span := pfBuckets(fromDay, "day", now)
		at := make(map[string]int, len(span))
		series := make([]wDay, len(span))
		for i, d := range span {
			at[d] = i
			t, _ := time.Parse("2006-01-02", d)
			series[i] = wDay{At: t, Values: []float64{0}}
		}
		for _, r := range buckets {
			if !profitActivityLabeled(r.Activity) {
				continue
			}
			if i, ok := at[r.Bucket]; ok {
				series[i].Values[0] += r.Income - r.Costs
			}
		}
		label := "Kept profit per day over the last " + v.Window
		if in.Draws("small") {
			v.Strip, _ = wBarChart(label, "Kept", series)
		}
		if in.Draws("wide") || in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
			v.Days, _ = wBarChart(label, "Kept", series)
		}
	}

	if in.Draws("tall") || in.Draws("medium") {
		prefs := app.profitPrefsFor(ctx, in.UserID)
		items, err := app.queries.ListTradeItems(ctx, db.ListTradeItemsParams{
			OwnerKind: ownerChar, OwnerIds: ids, Win: tradeWindowFor(days),
			BrokerInMargin: prefs.BrokerInMargin, RowCap: profitPulseTop,
		})
		if err != nil {
			logging.Errorf("profitpulse: top items for user %d: %v", in.UserID, err)
		}
		var ranked []wRankItem
		for _, r := range items {
			broker := 0.0
			if prefs.BrokerInMargin {
				broker = r.Broker
			}
			margin := r.CovRevenue - r.CovCost - r.CovTax - broker
			name := app.typeNameOrID(ctx, r.TypeID)
			ranked = append(ranked, wRankItem{Name: name, Link: itemLink(r.TypeID, name), Value: margin,
				Text: signedCompact(margin), Full: signedISK(margin) + " ISK margin"})
		}
		if in.Draws("tall") && len(ranked) > 0 {
			v.TopItem, _ = wRankedBars("The item with the best margin", ranked[:1], 1)
		}
		if in.Draws("medium") {
			v.TopItems, _ = wRankedBars("The items with the best margin", ranked, profitPulseTop)
		}
	}

	if in.Draws("large") {
		var parts []wRankItem
		for _, l := range profitActivityLabels {
			k, ok := byAct[l.key]
			if !ok || math.Abs(k) < 0.005 {
				continue
			}
			parts = append(parts, wRankItem{Name: l.label, Value: k, Text: signedCompact(k), Full: signedISK(k) + " ISK kept"})
		}
		sort.SliceStable(parts, func(i, j int) bool { return parts[i].Value > parts[j].Value })
		v.Activities, _ = wRankedBars("Kept profit by activity", parts, len(parts))

		cogs, err := app.queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{Unit: "day", OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay})
		if err != nil {
			logging.Errorf("profitpulse: cogs buckets for user %d: %v", in.UserID, err)
			return v
		}
		inSpan := make(map[string]bool, days)
		for _, b := range pfBuckets(fromDay, "day", now) {
			inSpan[b] = true
		}
		var covCost, uncovRev float64
		for _, r := range cogs {
			if !inSpan[r.Bucket] {
				continue
			}
			covCost += r.CovCost
			uncovRev += math.Max(r.Revenue-r.CovRevenue, 0)
		}
		v.Waterfall = buildWaterfall(&profitView{Days: days}, gross, industryCosts, kept, covCost, uncovRev)
		if wf := v.Waterfall; wf != nil {
			v.WfTable = wTable{Caption: wf.Title, Head: []string{"Step", "ISK"}, Rows: [][]string{{"Gross in", esi.FormatISK(gross)}}}
			for _, l := range wf.Legend {
				v.WfTable.Rows = append(v.WfTable.Rows, []string{l.Label, l.Value})
			}
		}
	}
	return v
}

// signedCompact writes a figure short with its sign: +1.2M, -350K, 0.
func signedCompact(v float64) string {
	if v > 0 {
		return "+" + iskCompact(v)
	}
	return iskCompact(v)
}

// profitLabeledTotals is the Profit page's headline from activity
// rows: gross in and kept profit over the labeled activities only
// (the unattributed remainder is not an activity). ok is false when
// no labeled activity has a row.
func profitLabeledTotals(acts []db.ListProfitByActivityRow) (gross, kept float64, ok bool) {
	for _, a := range acts {
		if !profitActivityLabeled(a.Activity) {
			continue
		}
		gross += a.Income
		kept += a.Income - a.Costs
		ok = true
	}
	return gross, kept, ok
}

// profitActivityLabeled reports whether an activity is one the Profit
// page names.
func profitActivityLabeled(activity string) bool {
	for _, l := range profitActivityLabels {
		if l.key == activity {
			return true
		}
	}
	return false
}
