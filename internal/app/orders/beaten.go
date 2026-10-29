package orders

// Beaten orders (schema 105): every open order across the account
// that another player has beaten at its own station, read from the
// verdicts the order-health pass stores on order_lifecycle. Each row
// carries the beating price, the gap, what is left to sell or buy,
// when it was beaten, the next valid price past the competition and
// the margin at that price. ESI cannot change an order's price, so
// the suggested price is offered to copy into the game client.

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// beatenRow is one beaten order, display-ready.
type beatenRow struct {
	Character    string
	Item         string
	TypeID       int64
	Side         string
	Loc          module.PlaceRef
	MyPrice      string
	Beating      string
	BeatingQty   string
	Gap          string // ISK
	GapPct       string
	Remaining    string
	BeatenAgo    string
	Times        int64
	Suggested    string // display
	SuggestedRaw string // plain number for the clipboard
	Margin       string
	MarginDir    string
	MarginBasis  string
	Checked      string
}

// buildBeatenOrders reads the account's beaten orders.
func (app *mod) buildBeatenOrders(ctx context.Context, userID int64) []beatenRow {
	if userID == 0 {
		return nil
	}
	rows, err := app.queries.ListBeatenOrders(ctx, db.ListBeatenOrdersParams{UserID: userID})
	if err != nil {
		logging.Errorf("orders: beaten orders for user %d: %v", userID, err)
		return nil
	}
	out := make([]beatenRow, 0, len(rows))
	for _, r := range rows {
		buy := r.IsBuyOrder == 1
		row := beatenRow{
			Character:  r.CharacterName,
			Item:       app.deps.TypeNameOrID(ctx, r.TypeID),
			TypeID:     r.TypeID,
			Side:       "Sell",
			Loc:        app.deps.LinkPlace(ctx, r.LocationID, app.deps.EconLocationTitle(ctx, r.LocationID)),
			MyPrice:    esi.FormatISK(r.ListedPrice),
			Beating:    esi.FormatISK(r.BeatingPrice),
			BeatingQty: esi.FormatInt(r.BeatingVolume),
			Remaining:  esi.FormatInt(r.VolumeRemainLast),
			Times:      r.OutbidEvents,
		}
		if buy {
			row.Side = "Buy"
		}
		gap := math.Abs(r.ListedPrice - r.BeatingPrice)
		row.Gap = esi.FormatISK(gap)
		if r.ListedPrice > 0 {
			row.GapPct = fmt.Sprintf("%.2f%%", gap/r.ListedPrice*100)
		}
		if r.BeatenAt.Valid {
			row.BeatenAgo = app.deps.AgeText(r.BeatenAt.Time)
		}
		if r.BeatenChecked.Valid {
			row.Checked = app.deps.AgeText(r.BeatenChecked.Time)
		}
		if r.SuggestedPrice > 0 {
			row.Suggested = esi.FormatISK(r.SuggestedPrice)
			row.SuggestedRaw = strconv.FormatFloat(r.SuggestedPrice, 'f', 2, 64)
		}
		if r.MarginPct.Valid {
			row.Margin = fmt.Sprintf("%+.1f%%", r.MarginPct.Float64)
			switch {
			case r.MarginPct.Float64 > 0.05:
				row.MarginDir = "up"
			case r.MarginPct.Float64 < -0.05:
				row.MarginDir = "down"
			}
			if buy {
				row.MarginBasis = "if resold at " + esi.FormatISK(r.UnitCost) + " ISK"
			} else {
				row.MarginBasis = "on a cost of " + esi.FormatISK(r.UnitCost) + " ISK"
			}
			if r.TaxRate > 0 {
				row.MarginBasis += fmt.Sprintf(", after %.2f%% sales tax", r.TaxRate*100)
			} else {
				row.MarginBasis += ", before sales tax"
			}
		} else if !buy {
			row.MarginBasis = "No cost on record for this stock"
		} else {
			row.MarginBasis = "No resale price in this region"
		}
		out = append(out, row)
	}
	return out
}
