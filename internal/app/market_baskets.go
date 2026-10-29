package app

// Market baskets (schema 103): named lists of types priced in one hub
// region, the presets and each player's own. A basket's value per day
// is stored (market_basket_values_daily) from the stat pack: today's
// value inside the transaction that completes the region's sweep, and
// its whole history from the stored daily rows when its contents
// change. Pages read those rows, never re-pricing a basket themselves.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	db "evesynapse/internal/db/sqlc"
)

const (
	// basketMaxItems bounds one basket's contents.
	basketMaxItems = 200
	// basketMaxPerUser bounds how many baskets one player keeps.
	basketMaxPerUser = 25
	basketNameMax    = 60
	// basketMaxQuantity bounds one type's quantity in a basket: far
	// past any real stack, and low enough that summing a basket's
	// lines can never overflow the quantity column.
	basketMaxQuantity = 1_000_000_000_000_000
)

// refreshBasketValues writes today's value for every basket priced in
// regionID, first rebuilding the history of any basket that has none.
// q is the sweep transaction's queries.
func refreshBasketValues(ctx context.Context, q *db.Queries, regionID int64, day string) error {
	fresh, err := q.ListMarketBasketsWithoutHistory(ctx, regionID)
	if err != nil {
		return err
	}
	since := day
	if t, err := time.Parse("2006-01-02", day); err == nil {
		since = basketDay(t.AddDate(0, 0, -bookTrendDays))
	}
	for _, id := range fresh {
		if _, err := q.RebuildMarketBasketValues(ctx, db.RebuildMarketBasketValuesParams{BasketID: id, Since: since}); err != nil {
			return err
		}
	}
	_, err = q.RefreshMarketBasketValuesForRegion(ctx, db.RefreshMarketBasketValuesForRegionParams{RegionID: regionID, Day: day})
	return err
}

// basketLine is one pasted line resolved or not.
type basketLine struct {
	Name     string
	Quantity int64
}

// parseBasketLines reads one item per line: a name, optionally
// followed by a quantity ("Tritanium 1000", "Tritanium x1000",
// "Tritanium\t1,000"). A line with no quantity counts one.
func parseBasketLines(text string) []basketLine {
	var out []basketLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		name, qty := line, int64(1)
		if i := strings.LastIndexFunc(line, unicode.IsSpace); i > 0 {
			tok := strings.TrimLeft(strings.TrimSpace(line[i:]), "xX×")
			tok = strings.ReplaceAll(tok, ",", "")
			if n, err := strconv.ParseInt(tok, 10, 64); err == nil && n > 0 {
				name, qty = strings.TrimSpace(line[:i]), n
			}
		}
		if qty > basketMaxQuantity {
			qty = basketMaxQuantity
		}
		if name != "" {
			out = append(out, basketLine{Name: name, Quantity: qty})
		}
		if len(out) > basketMaxItems {
			break
		}
	}
	return out
}

// saveBasketItems resolves pasted lines and replaces a basket's items
// and value history in one transaction. It returns the names it could
// not resolve.
func (app *Application) saveBasketItems(ctx context.Context, basketID int64, lines []basketLine) (unknown []string, err error) {
	if len(lines) > basketMaxItems {
		return nil, fmt.Errorf("a basket holds at most %d items", basketMaxItems)
	}
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		names = append(names, strings.ToLower(l.Name))
	}
	resolved, err := app.queries.ResolveTypeNamesExact(ctx, names)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(resolved))
	for _, r := range resolved {
		ids[strings.ToLower(r.Name)] = r.TypeID
	}
	merged := map[int64]int64{}
	var order []int64
	for _, l := range lines {
		id, ok := ids[strings.ToLower(l.Name)]
		if !ok {
			unknown = append(unknown, l.Name)
			continue
		}
		if _, seen := merged[id]; !seen {
			order = append(order, id)
		}
		merged[id] += l.Quantity
		if merged[id] > basketMaxQuantity {
			merged[id] = basketMaxQuantity
		}
	}
	if err := app.replaceBasketItems(ctx, basketID, order, merged); err != nil {
		return nil, err
	}
	return unknown, nil
}

// basketItemQty is one type and quantity the basket builder picked.
type basketItemQty struct {
	TypeID   int64
	Quantity int64
}

// saveBasketItemsByID replaces a basket's items from picked types,
// merging repeats with the same saturation the pasted-line path
// applies.
func (app *Application) saveBasketItemsByID(ctx context.Context, basketID int64, items []basketItemQty) error {
	if len(items) > basketMaxItems {
		return fmt.Errorf("a basket holds at most %d items", basketMaxItems)
	}
	merged := map[int64]int64{}
	var order []int64
	for _, it := range items {
		if it.TypeID <= 0 {
			continue
		}
		qty := it.Quantity
		if qty < 1 {
			qty = 1
		}
		if _, seen := merged[it.TypeID]; !seen {
			order = append(order, it.TypeID)
		}
		merged[it.TypeID] += qty
		if merged[it.TypeID] > basketMaxQuantity {
			merged[it.TypeID] = basketMaxQuantity
		}
	}
	return app.replaceBasketItems(ctx, basketID, order, merged)
}

// replaceBasketItems swaps a basket's contents and rebuilds its
// value history in one transaction.
func (app *Application) replaceBasketItems(ctx context.Context, basketID int64, order []int64, merged map[int64]int64) error {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteMarketBasketItems(ctx, basketID); err != nil {
		return err
	}
	for _, id := range order {
		if err := qtx.InsertMarketBasketItem(ctx, db.InsertMarketBasketItemParams{BasketID: basketID, TypeID: id, Quantity: merged[id]}); err != nil {
			return err
		}
	}
	if err := qtx.DeleteMarketBasketValues(ctx, basketID); err != nil {
		return err
	}
	if _, err := qtx.RebuildMarketBasketValues(ctx, db.RebuildMarketBasketValuesParams{BasketID: basketID, Since: basketDay(time.Now().AddDate(0, 0, -bookTrendDays))}); err != nil {
		return err
	}
	return tx.Commit()
}

// basketDay is the stored day key for t.
func basketDay(t time.Time) string { return t.UTC().Format("2006-01-02") }
