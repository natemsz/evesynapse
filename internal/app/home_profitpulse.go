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
// the two headline figures, the default size adds the top item,
// and a larger size adds the waterfall, drawn by the Profit page's
// own chart builder (buildWaterfall) from the same span totals the
// page's waterfall walks.
// ---------------------------------------------------------------------------

type profitPulseWidget struct {
	HasData      bool
	GrossIn      string // gross in over the span, house ISK format
	Kept         string // kept as profit (the page's Earned), signed
	KeptDir      string // dirOf(kept): "up" | "down" | ""
	TopItem      string // the period's top item name; "" when none, or the size does not show it
	TopMargin    string // the top item's margin, signed
	TopMarginDir string
	Waterfall    *pfWaterfall // largest sizes only
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
