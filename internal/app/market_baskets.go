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
)

// refreshBasketValues writes today's value for every basket priced in
// regionID, first rebuilding the history of any basket that has none.
// q is the sweep transaction's queries.
func refreshBasketValues(ctx context.Context, q *db.Queries, regionID int64, day string) error {
	fresh, err := q.ListMarketBasketsWithoutHistory(ctx, regionID)
	if err != nil {
		return err
	}
	for _, id := range fresh {
		if _, err := q.RebuildMarketBasketValues(ctx, id); err != nil {
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
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteMarketBasketItems(ctx, basketID); err != nil {
		return nil, err
	}
	for _, l := range lines {
		id, ok := ids[strings.ToLower(l.Name)]
		if !ok {
			unknown = append(unknown, l.Name)
			continue
		}
		if err := qtx.InsertMarketBasketItem(ctx, db.InsertMarketBasketItemParams{BasketID: basketID, TypeID: id, Quantity: l.Quantity}); err != nil {
			return nil, err
		}
	}
	if err := qtx.DeleteMarketBasketValues(ctx, basketID); err != nil {
		return nil, err
	}
	if _, err := qtx.RebuildMarketBasketValues(ctx, basketID); err != nil {
		return nil, err
	}
	return unknown, tx.Commit()
}

// basketDay is the stored day key for t.
func basketDay(t time.Time) string { return t.UTC().Format("2006-01-02") }
