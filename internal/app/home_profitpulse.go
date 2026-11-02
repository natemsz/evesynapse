package app

import (
	"context"
	"math"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Profit pulse: the Profit Analysis headline as a home widget. The
// figures are the Profit page's own, over the page's default span
// (the last 30 days, all your characters), read from the stored
// ledger tables the worker folds: profit_daily for the headline,
// trade_item_window for the top item, trade_item_daily for the
// waterfall's cost of goods. Nothing is recomputed at render.
//
// How much shows follows the size the user picked for the widget,
// the same stored size every widget reads: the smallest sizes show
// the two headline figures; 2x2, 2x3 and 3x2 add the top item and
// a mini chart of kept profit per day, placed beside the figures
// in shapes at least as wide as tall and below them in taller
// ones; and the remaining larger sizes add the waterfall instead,
// drawn by the Profit page's own chart builder (buildWaterfall)
// from the same span totals the page's waterfall walks.
// ---------------------------------------------------------------------------

type profitPulseWidget struct {
	HasData      bool
	GrossIn      string // gross in over the span, house ISK format
	Kept         string // kept as profit (the page's Earned), signed
	KeptDir      string // dirOf(kept): "up" | "down" | ""
	TopItem      string // the period's top item name; "" when none, or the size does not show it
	TopMargin    string // the top item's margin, signed
	TopMarginDir string
	Mini         *profitPulseMini // 2x2, 2x3 and 3x2 only: kept profit per day
	MiniSide     bool             // the mini chart sits right of the figures (w >= h), not below them
	Waterfall    *pfWaterfall     // the remaining larger sizes only
}

// profitPulseMini is the mini chart of the 2x2, 2x3 and 3x2
// sizes: one bar per day of the span, kept profit above the zero
// line, losses below. Beside the figures it is drawn tall and
// narrow; below them, the same height and the card's full width.
type profitPulseMini struct {
	W, H  int
	ZeroY float64
	Bars  []profitPulseMiniBar
}

type profitPulseMiniBar struct {
	X, Y, W, H float64
	Class      string // sem-kept | sem-lost, the semantic fill tokens
}

// buildProfitPulseMini shapes per-day kept profit into mini bars
// on a scale shared across the span, so the days compare. Days
// with no kept profit draw no bar. The chart stands 72 units
// tall either way; beside the figures it is half as wide.
func buildProfitPulseMini(nets []float64, side bool) *profitPulseMini {
	maxAbs := 0.0
	for _, v := range nets {
		maxAbs = math.Max(maxAbs, math.Abs(v))
	}
	if maxAbs <= 0 {
		return nil
	}
	w, h := 288, 72
	if side {
		w = 144
	}
	m := &profitPulseMini{W: w, H: h, ZeroY: float64(h) / 2}
	band := float64(w) / float64(len(nets))
	bw := band * 0.6
	for i, v := range nets {
		if v == 0 {
			continue
		}
		bh := math.Max(math.Abs(v)/maxAbs*(float64(h)/2-2), 1)
		bar := profitPulseMiniBar{X: float64(i)*band + (band-bw)/2, W: bw, H: bh, Class: "sem-kept"}
		if v > 0 {
			bar.Y = m.ZeroY - bh
		} else {
			bar.Y, bar.Class = m.ZeroY, "sem-lost"
		}
		m.Bars = append(m.Bars, bar)
	}
	if len(m.Bars) == 0 {
		return nil
	}
	return m
}

// profitPulseDays is the widget's span: the Profit page's default.
const profitPulseDays = 30

func (app *Application) buildProfitPulse(ctx context.Context, userID int64, gw, gh int) *profitPulseWidget {
	w := &profitPulseWidget{}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("profitpulse: list characters for user %d: %v", userID, err)
		return w
	}
	if len(chars) == 0 {
		return w
	}
	ids := make([]int64, 0, len(chars))
	for _, ch := range chars {
		ids = append(ids, ch.CharacterID)
	}
	now := time.Now().UTC()
	fromDay := now.AddDate(0, 0, -profitPulseDays+1).Format("2006-01-02")

	acts, err := app.queries.ListProfitByActivity(ctx, db.ListProfitByActivityParams{
		OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay,
	})
	if err != nil {
		logging.Errorf("profitpulse: profit by activity for user %d: %v", userID, err)
		return w
	}
	// The page's own totals: the labeled activities only. The
	// unattributed remainder is not an activity and stays out of
	// both figures, exactly as on the Profit page.
	labeled := make(map[string]bool, len(profitActivityLabels))
	for _, l := range profitActivityLabels {
		labeled[l.key] = true
	}
	var gross, kept, industryCosts float64
	for _, a := range acts {
		if !labeled[a.Activity] {
			continue
		}
		gross += a.Income
		kept += a.Income - a.Costs
		if a.Activity == actIndustry {
			industryCosts = a.Costs
		}
		w.HasData = true
	}
	if !w.HasData {
		return w
	}
	w.GrossIn = esi.FormatISK(gross)
	w.Kept = signedISK(kept)
	w.KeptDir = dirOf(kept)

	// The detail scales with the widget's size: below the default
	// size the figures stand alone.
	area := gw * gh
	if area < 4 {
		return w
	}
	prefs := app.profitPrefsFor(ctx, userID)
	items, err := app.queries.ListTradeItems(ctx, db.ListTradeItemsParams{
		OwnerKind: ownerChar, OwnerIds: ids, Win: tradeWindowFor(profitPulseDays),
		BrokerInMargin: prefs.BrokerInMargin, RowCap: 1,
	})
	if err != nil {
		logging.Errorf("profitpulse: top item for user %d: %v", userID, err)
	} else if len(items) > 0 {
		r := items[0]
		broker := 0.0
		if prefs.BrokerInMargin {
			broker = r.Broker
		}
		margin := r.CovRevenue - r.CovCost - r.CovTax - broker
		w.TopItem = app.typeNameOrID(ctx, r.TypeID)
		w.TopMargin = signedISK(margin)
		w.TopMarginDir = dirOf(margin)
	}
	if gw >= 2 && gh >= 2 && area <= 6 {
		// The mini chart sizes (2x2, 2x3, 3x2): kept profit per
		// day over the same span, from the same profit_daily
		// rows the headline totals (the charts' own bucket
		// query, the labeled activities only, so the bars sum
		// to Kept). From 3x3 up the waterfall below takes the
		// chart's place.
		buckets, err := app.queries.ListProfitBuckets(ctx, db.ListProfitBucketsParams{
			Unit: "day", OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay,
		})
		if err != nil {
			logging.Errorf("profitpulse: daily buckets for user %d: %v", userID, err)
			return w
		}
		days := pfBuckets(fromDay, "day", now)
		dayIndex := make(map[string]int, len(days))
		for i, d := range days {
			dayIndex[d] = i
		}
		nets := make([]float64, len(days))
		for _, r := range buckets {
			if !labeled[r.Activity] {
				continue
			}
			if i, ok := dayIndex[r.Bucket]; ok {
				nets[i] += r.Income - r.Costs
			}
		}
		w.MiniSide = gw >= gh
		w.Mini = buildProfitPulseMini(nets, w.MiniSide)
		return w
	}
	if area < 5 {
		return w
	}
	cogs, err := app.queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{
		Unit: "day", OwnerKind: ownerChar, OwnerIds: ids, FromDay: fromDay,
	})
	if err != nil {
		logging.Errorf("profitpulse: cogs buckets for user %d: %v", userID, err)
		return w
	}
	inSpan := make(map[string]bool, profitPulseDays)
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
	w.Waterfall = buildWaterfall(&profitView{Days: profitPulseDays}, gross, industryCosts, kept, covCost, uncovRev)
	return w
}
