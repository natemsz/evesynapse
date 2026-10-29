package app

// The Profit page's Trading tab (schema 109): per item over the
// span, the covered margin (matched units of sales whose tax paired,
// less their cost, their tax share and, by default, the broker fees
// tied to the item), how much of what sold is covered, and every
// uncovered unit by its reason. Read from trade_item_window only.
// Also the player's profit preferences and the cost basis form.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const tradeItemRowCap = 100

// tradeWindowFor maps the page's span to a stored window.
func tradeWindowFor(days int) int32 {
	switch days {
	case 7, 30, 90, 365:
		return int32(days)
	}
	return 30
}

func (app *Application) attachTradeItems(ctx context.Context, view *profitView, kind string, ids []int64, brokerFees float64, brokerInMargin bool) error {
	win := tradeWindowFor(view.Days)
	rows, err := app.queries.ListTradeItems(ctx, db.ListTradeItemsParams{OwnerKind: kind, OwnerIds: ids, Win: win, BrokerInMargin: brokerInMargin, RowCap: tradeItemRowCap})
	if err != nil {
		return err
	}
	broker := func(b float64) float64 {
		if brokerInMargin {
			return b
		}
		return 0
	}
	for _, r := range rows {
		net := r.CovRevenue - r.CovCost - r.CovTax - broker(r.Broker)
		row := profitItemRow{
			TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID),
			Sold: esi.FormatInt(r.Units), Revenue: esi.FormatISK(r.Revenue),
			Margin: signedISK(net), MarginDir: dirOf(net),
			Covered:       r.Units > 0 && r.CovUnits >= r.Units,
			CanEnterBasis: view.BasisPool != "" && r.GapNoBuy > 0,
		}
		if r.CovRevenue > 0 {
			row.MarginPct = fmt.Sprintf("%.1f%%", net/r.CovRevenue*100)
		}
		if !row.Covered && r.Units > 0 {
			row.CoveredOf = fmt.Sprintf("on %s of %s units", esi.FormatInt(r.CovUnits), esi.FormatInt(r.Units))
		}
		if r.PrevCovUnits > 0 {
			d := net - (r.PrevCovNet - broker(r.PrevBroker))
			row.Delta, row.DeltaDir = signedISK(d), dirOf(d)
		}
		for _, g := range []struct {
			key   string
			units int64
		}{{"before_history", r.GapBeforeHistory}, {"ledger_hole", r.GapLedgerHole}, {"no_buy", r.GapNoBuy}, {"no_tax", r.GapNoTax}} {
			if g.units > 0 {
				row.Gaps = append(row.Gaps, profitGap{Units: esi.FormatInt(g.units), Label: tradeGapLabels[g.key]})
			}
		}
		view.Items = append(view.Items, row)
	}

	sum, err := app.queries.SumTradeItems(ctx, db.SumTradeItemsParams{OwnerKind: kind, OwnerIds: ids, Win: win})
	if err != nil {
		return err
	}
	if sum.Units == 0 && sum.Broker == 0 {
		return nil
	}
	net := sum.CovNet - broker(sum.Broker)
	view.ItemsNet, view.ItemsNetDir = signedISK(net), dirOf(net)
	if sum.PrevCovUnits > 0 {
		d := net - (sum.PrevCovNet - broker(sum.PrevBroker))
		view.ItemsDelta, view.ItemsDeltaDir = signedISK(d), dirOf(d)
	}
	if sum.Units > 0 {
		view.ItemsCoverage = fmt.Sprintf("%.0f%%", math.Floor(float64(sum.CovUnits)/float64(sum.Units)*100))
	}
	if loose := brokerFees - sum.Broker; loose > 0.5 {
		view.BrokerLoose = esi.FormatISK(loose)
	}
	view.ItemsAsOf = sum.AsOfDay
	return nil
}

// handleProfitPrefs saves the player's profit preferences. A change of
// pooling refolds their books on the next sync.
func (app *Application) handleProfitPrefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/profit/")
	if userID == 0 || r.ParseForm() != nil {
		back("That form didn't come through. Try again.")
		return
	}
	pooled := r.Form.Get("pooled") == "1"
	broker := r.Form.Get("broker_in_margin") == "1"
	was := app.profitPrefsFor(ctx, userID)
	if err := app.queries.UpsertUserProfitPrefs(ctx, db.UpsertUserProfitPrefsParams{UserID: userID, Pooled: pooled, BrokerInMargin: broker}); err != nil {
		logging.Errorf("profit: save prefs for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	if was.Pooled != pooled {
		back("Saved. Your trading figures are recomputed on the next sync.")
		return
	}
	back("Saved.")
}

// handleProfitBasis records a cost basis the player entered for units
// of an item no buy covered, dated just before the pool first sold
// them uncovered, and has the pool refold on its next pass.
func (app *Application) handleProfitBasis(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/profit/")
	if userID == 0 || r.ParseForm() != nil {
		back("That form didn't come through. Try again.")
		return
	}
	kind, idRaw, _ := strings.Cut(r.Form.Get("pool"), "-")
	poolID, _ := strconv.ParseInt(idRaw, 10, 64)
	typeID, _ := strconv.ParseInt(r.Form.Get("type_id"), 10, 64)
	qty, _ := strconv.ParseInt(strings.ReplaceAll(r.Form.Get("quantity"), ",", ""), 10, 64)
	unit, err := strconv.ParseFloat(strings.ReplaceAll(r.Form.Get("unit_cost"), ",", ""), 64)
	if err != nil || typeID <= 0 || qty <= 0 || qty > 1e12 || unit < 0 || unit > 1e14 || math.IsNaN(unit) {
		back("Give a quantity and a cost per unit.")
		return
	}
	if !app.ownsProfitPool(ctx, userID, kind, poolID) {
		back("That isn't one of your pools.")
		return
	}
	at, err := app.queries.GetFirstUncoveredSale(ctx, db.GetFirstUncoveredSaleParams{PoolKind: kind, PoolID: poolID, TypeID: typeID})
	if err != nil {
		at = time.Now().UTC()
	}
	if err := app.queries.InsertManualCostBasis(ctx, db.InsertManualCostBasisParams{
		PoolKind: kind, PoolID: poolID, TypeID: typeID, At: at.Add(-time.Second), Quantity: qty, UnitCost: unit,
		Note: clip(strings.TrimSpace(r.Form.Get("note")), 200),
	}); err != nil {
		logging.Errorf("profit: manual basis for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	if err := app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: kind, PoolID: poolID}); err != nil {
		logging.Errorf("profit: mark pool %s %d for refold: %v", kind, poolID, err)
	}
	back("Saved. That item's margin is recomputed on the next sync.")
}

// ownsProfitPool reports whether a pool is the player's: their
// account, one of their characters, or a corporation whose wallet one
// of their characters reads.
func (app *Application) ownsProfitPool(ctx context.Context, userID int64, kind string, id int64) bool {
	switch kind {
	case poolAcct:
		return id == userID
	case ownerChar, ownerCorp:
		chars, err := app.queries.ListCharactersByUser(ctx, userID)
		if err != nil {
			return false
		}
		if kind == ownerCorp {
			return app.profitCorpsFor(ctx, chars)[id]
		}
		for _, ch := range chars {
			if ch.CharacterID == id {
				return true
			}
		}
	}
	return false
}
