package app

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Stored basket figures (schema 123). Wherever a basket's daily values
// are written (the region's sweep, an edit of its items), the figures
// its pages show are derived in the same transaction and stored: the
// summary (worth, changes, the 30-day line) and each item's unit,
// worth, share, change and impact. The pages only read them.

// basketFigureChunk bounds how many baskets one read of their values
// covers, under ListMarketBasketValuesForBaskets's row cap.
const basketFigureChunk = 50

// storeBasketFigures derives and stores the figures of the given
// baskets from their stored values, their items and the stat pack.
func storeBasketFigures(ctx context.Context, tx db.DBTX, basketIDs []int64, now time.Time) error {
	for start := 0; start < len(basketIDs); start += basketFigureChunk {
		end := min(start+basketFigureChunk, len(basketIDs))
		if err := storeBasketFigureChunk(ctx, tx, basketIDs[start:end], now); err != nil {
			return err
		}
	}
	return nil
}

func storeBasketFigureChunk(ctx context.Context, tx db.DBTX, ids []int64, now time.Time) error {
	q := db.New(tx)
	series := make(map[int64][]db.ListMarketBasketValuesRow, len(ids))
	rows, err := q.ListMarketBasketValuesForBaskets(ctx, db.ListMarketBasketValuesForBasketsParams{
		BasketIds: ids, Since: basketDay(now.AddDate(0, 0, -34)),
	})
	if err != nil {
		return fmt.Errorf("basket values: %w", err)
	}
	for _, v := range rows {
		series[v.BasketID] = append(series[v.BasketID], db.ListMarketBasketValuesRow{
			Day: v.Day, ValueSell: v.ValueSell, ValueBuy: v.ValueBuy, PricedItems: v.PricedItems, ItemCount: v.ItemCount,
		})
	}
	items, err := q.ListMarketBasketItemsForBaskets(ctx, ids)
	if err != nil {
		return fmt.Errorf("basket items: %w", err)
	}
	regionRows, err := q.ListMarketBasketRegions(ctx, ids)
	if err != nil {
		return fmt.Errorf("basket regions: %w", err)
	}
	regionOf := make(map[int64]int64, len(regionRows))
	typesIn := make(map[int64]map[int64]bool)
	for _, r := range regionRows {
		regionOf[r.ID] = r.RegionID
	}
	for _, it := range items {
		reg := regionOf[it.BasketID]
		if typesIn[reg] == nil {
			typesIn[reg] = map[int64]bool{}
		}
		typesIn[reg][it.TypeID] = true
	}
	// Each item's stat pack row: its basket's region, the global book
	// for a type that region does not carry (as the values are).
	stats := make(map[[2]int64]*db.MarketRegionStat)
	allTypes := map[int64]bool{}
	for reg, set := range typesIn {
		typeIDs := make([]int64, 0, len(set))
		for id := range set {
			typeIDs = append(typeIDs, id)
			allTypes[id] = true
		}
		got, err := q.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{RegionID: reg, Column2: typeIDs})
		if err != nil {
			return fmt.Errorf("basket stat packs: %w", err)
		}
		for i := range got {
			stats[[2]int64{reg, got[i].TypeID}] = &got[i]
		}
	}
	global := make(map[int64]*db.MarketRegionStat)
	if len(allTypes) > 0 {
		typeIDs := make([]int64, 0, len(allTypes))
		for id := range allTypes {
			typeIDs = append(typeIDs, id)
		}
		got, err := q.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{RegionID: globalMarketRegion, Column2: typeIDs})
		if err != nil {
			return fmt.Errorf("basket global stat packs: %w", err)
		}
		for i := range got {
			global[got[i].TypeID] = &got[i]
		}
	}

	// The summaries.
	nan := math.NaN()
	var (
		sIDs, sPriced, sCounts                      []int64
		sSell, sBuy, sC1, sC7, sC30, sD1, sD7, sD30 []float64
		sLines, sDirs, sSparks                      []string
		sLows, sHighs, sMeans                       []float64
	)
	for _, id := range ids {
		vals := series[id]
		sIDs = append(sIDs, id)
		var last db.ListMarketBasketValuesRow
		if n := len(vals); n > 0 {
			last = vals[n-1]
		}
		sSell, sBuy = append(sSell, last.ValueSell), append(sBuy, last.ValueBuy)
		sPriced, sCounts = append(sPriced, last.PricedItems), append(sCounts, last.ItemCount)
		for _, days := range []int{1, 7, 30} {
			chg, delta := nan, nan
			if now, then, ok := basketChangeBase(vals, days); ok {
				chg, delta = (now-then)/then*100, now-then
			}
			switch days {
			case 1:
				sC1, sD1 = append(sC1, chg), append(sD1, delta)
			case 7:
				sC7, sD7 = append(sC7, chg), append(sD7, delta)
			default:
				sC30, sD30 = append(sC30, chg), append(sD30, delta)
			}
		}
		line, dir, spark, lo, hi, mean := basketLineFigures(vals, now)
		sLines, sDirs, sSparks = append(sLines, line), append(sDirs, dir), append(sSparks, spark)
		sLows, sHighs, sMeans = append(sLows, lo), append(sHighs, hi), append(sMeans, mean)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO market_basket_summary (basket_id, value_sell, value_buy, priced_items, item_count,
			chg_1d, chg_7d, chg_30d, delta_1d, delta_7d, delta_30d,
			line_30d, line_dir, spark_30d, low_30d, high_30d, mean_30d, updated_at)
		SELECT u.b, u.vs, u.vb, u.p, u.c,
			NULLIF(u.c1, 'NaN'), NULLIF(u.c7, 'NaN'), NULLIF(u.c30, 'NaN'),
			NULLIF(u.d1, 'NaN'), NULLIF(u.d7, 'NaN'), NULLIF(u.d30, 'NaN'),
			u.l, u.dir, u.s, u.lo, u.hi, u.mean, $18
		FROM unnest($1::bigint[], $2::float8[], $3::float8[], $4::bigint[], $5::bigint[],
			$6::float8[], $7::float8[], $8::float8[], $9::float8[], $10::float8[], $11::float8[],
			$12::text[], $13::text[], $14::text[], $15::float8[], $16::float8[], $17::float8[])
			AS u(b, vs, vb, p, c, c1, c7, c30, d1, d7, d30, l, dir, s, lo, hi, mean)
		ON CONFLICT (basket_id) DO UPDATE SET
			value_sell = excluded.value_sell, value_buy = excluded.value_buy,
			priced_items = excluded.priced_items, item_count = excluded.item_count,
			chg_1d = excluded.chg_1d, chg_7d = excluded.chg_7d, chg_30d = excluded.chg_30d,
			delta_1d = excluded.delta_1d, delta_7d = excluded.delta_7d, delta_30d = excluded.delta_30d,
			line_30d = excluded.line_30d, line_dir = excluded.line_dir, spark_30d = excluded.spark_30d,
			low_30d = excluded.low_30d, high_30d = excluded.high_30d, mean_30d = excluded.mean_30d,
			updated_at = excluded.updated_at`,
		sIDs, sSell, sBuy, sPriced, sCounts,
		sC1, sC7, sC30, sD1, sD7, sD30,
		sLines, sDirs, sSparks, sLows, sHighs, sMeans,
		now); err != nil {
		return fmt.Errorf("basket summaries: %w", err)
	}

	// The items: unit and worth at the stat pack's typical sell, share
	// of the basket's priced worth, and per period the price change
	// and the ISK it moved the basket (worth now less worth then).
	total := make(map[int64]float64)
	type itemFig struct {
		basket, typeID int64
		priced         bool
		unit, value    float64
		chg, impact    [3]float64
	}
	figs := make([]itemFig, 0, len(items))
	for _, it := range items {
		reg := regionOf[it.BasketID]
		s := stats[[2]int64{reg, it.TypeID}]
		if s == nil {
			s = global[it.TypeID]
		}
		f := itemFig{basket: it.BasketID, typeID: it.TypeID, chg: [3]float64{nan, nan, nan}, impact: [3]float64{nan, nan, nan}}
		if s != nil && s.TypicalSell > 0 {
			f.priced, f.unit = true, s.TypicalSell
			f.value = s.TypicalSell * float64(it.Quantity)
			total[it.BasketID] += f.value
			for i, days := range []int{1, 7, 30} {
				if c := periodChange(s, days); c.Valid && c.Float64 > -100 {
					f.chg[i] = c.Float64
					f.impact[i] = f.value - f.value/(1+c.Float64/100)
				}
			}
		}
		figs = append(figs, f)
	}
	var (
		iBaskets, iTypes                                         []int64
		iPriced                                                  []bool
		iUnits, iValues, iShares, iC1, iC7, iC30, iI1, iI7, iI30 []float64
	)
	for _, f := range figs {
		share := 0.0
		if f.priced && total[f.basket] > 0 {
			share = f.value / total[f.basket] * 100
		}
		iBaskets, iTypes, iPriced = append(iBaskets, f.basket), append(iTypes, f.typeID), append(iPriced, f.priced)
		iUnits, iValues, iShares = append(iUnits, f.unit), append(iValues, f.value), append(iShares, share)
		iC1, iC7, iC30 = append(iC1, f.chg[0]), append(iC7, f.chg[1]), append(iC30, f.chg[2])
		iI1, iI7, iI30 = append(iI1, f.impact[0]), append(iI7, f.impact[1]), append(iI30, f.impact[2])
	}
	if err := q.DeleteMarketBasketItemStats(ctx, ids); err != nil {
		return fmt.Errorf("basket item stats clear: %w", err)
	}
	if len(figs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO market_basket_item_stats (basket_id, type_id, priced, unit, value, share_pct,
			chg_1d, chg_7d, chg_30d, impact_1d, impact_7d, impact_30d)
		SELECT u.b, u.t, u.p, u.unit, u.v, u.sh,
			NULLIF(u.c1, 'NaN'), NULLIF(u.c7, 'NaN'), NULLIF(u.c30, 'NaN'),
			NULLIF(u.i1, 'NaN'), NULLIF(u.i7, 'NaN'), NULLIF(u.i30, 'NaN')
		FROM unnest($1::bigint[], $2::bigint[], $3::bool[], $4::float8[], $5::float8[], $6::float8[],
			$7::float8[], $8::float8[], $9::float8[], $10::float8[], $11::float8[], $12::float8[])
			AS u(b, t, p, unit, v, sh, c1, c7, c30, i1, i7, i30)`,
		iBaskets, iTypes, iPriced, iUnits, iValues, iShares,
		iC1, iC7, iC30, iI1, iI7, iI30); err != nil {
		return fmt.Errorf("basket item stats: %w", err)
	}
	return nil
}

// basketLineFigures is a basket's 30-day worth as stored lines: the
// list's trend column points (60 by 16), the line's direction, the
// hover card's points (the site's 100 by 28 sparkline box) and the
// window's low, high and mean. Empty under two priced days.
func basketLineFigures(series []db.ListMarketBasketValuesRow, now time.Time) (line, dir, spark string, lo, hi, mean float64) {
	since := basketDay(now.AddDate(0, 0, -30))
	values := make([]float64, 0, len(series))
	for _, v := range series {
		if v.ValueSell > 0 && v.Day >= since {
			values = append(values, v.ValueSell)
		}
	}
	if len(values) < 2 {
		return "", "", "", 0, 0, 0
	}
	lo, hi = values[0], values[0]
	sum := 0.0
	for _, v := range values {
		lo, hi = min(lo, v), max(hi, v)
		sum += v
	}
	mean = sum / float64(len(values))
	pts := make([]string, len(values))
	for i, v := range values {
		x := float64(i) * 60 / float64(len(values)-1)
		y := 8.0
		if hi > lo {
			y = 14 - (v-lo)/(hi-lo)*12
		}
		pts[i] = fmt.Sprintf("%.1f,%.1f", x, y)
	}
	dir = "flat"
	if last, first := values[len(values)-1], values[0]; last > first {
		dir = "up"
	} else if last < first {
		dir = "down"
	}
	spark, _, _ = sparklineSVG(values)
	return strings.Join(pts, " "), dir, spark, lo, hi, mean
}
