package app

// Beaten orders (schema 105): the order-health pass's station-level
// verdict for sell and buy orders alike, kept on order_lifecycle. An
// order is beaten when another player's order at its own station
// asks less (a sell) or bids more (a buy). The owner's other
// characters never count: a player's own alts are not competition.
// For a beaten order the pass stores the beating price, the next
// valid price past it (ESI has no order-modify endpoint, so the page
// offers that price to copy into the client), and the margin at it.

import (
	"context"
	"database/sql"
	"math"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// priceTick is the smallest step a price may move by: EVE keeps four
// significant digits, never finer than 0.01 ISK.
func priceTick(p float64) float64 {
	if p <= 0 {
		return 0.01
	}
	return math.Max(math.Pow(10, math.Floor(math.Log10(p))-3), 0.01)
}

func roundCents(p float64) float64 { return math.Round(p*100) / 100 }

// nextPricePast is the first valid price strictly better than the
// beating one: just under it for a sell, just over it for a buy.
func nextPricePast(beating float64, buy bool) float64 {
	if beating <= 0 {
		return 0
	}
	t := priceTick(beating)
	if buy {
		return roundCents((math.Floor(beating/t+1e-9) + 1) * t)
	}
	c := (math.Ceil(beating/t-1e-9) - 1) * t
	if decade := math.Pow(10, math.Floor(math.Log10(beating))); c < decade-1e-9 && t/10 >= 0.01 {
		// Stepping down crossed into the decade below, where the
		// tick is ten times finer: the best price is just under.
		c = (math.Ceil(beating/(t/10)-1e-9) - 1) * (t / 10)
	}
	return math.Max(roundCents(c), 0.01)
}

// stationCompetition finds the best competing order at an order's own
// station on its side of the book, leaving out every order the owner
// holds. beaten is true when that order beats ours by at least a cent.
func stationCompetition(o esi.CharOrder, book []esi.MarketOrder, own map[int64]bool) (best float64, volume int64, beaten bool) {
	for _, b := range book {
		if b.IsBuyOrder != o.IsBuyOrder || b.LocationID != o.LocationID || own[b.OrderID] || b.OrderID == o.OrderID {
			continue
		}
		switch {
		case best == 0, o.IsBuyOrder && b.Price > best, !o.IsBuyOrder && b.Price < best:
			best, volume = b.Price, b.VolumeRemain
		case b.Price == best:
			volume += b.VolumeRemain
		}
	}
	if best == 0 {
		return 0, 0, false
	}
	if o.IsBuyOrder {
		return best, volume, best > o.Price+0.005
	}
	return best, volume, best < o.Price-0.005
}

// beatenMargin is the margin at the suggested price after the sales
// tax rate: a sell against what the stock cost, a buy against the
// typical sell it could be resold at. ok is false with no cost.
func beatenMargin(suggested, unitCost, taxRate float64, buy bool) (float64, bool) {
	if suggested <= 0 || unitCost <= 0 {
		return 0, false
	}
	if buy {
		return (unitCost*(1-taxRate) - suggested) / suggested * 100, true
	}
	return (suggested*(1-taxRate) - unitCost) / suggested * 100, true
}

// beatenContext caches, for one pass, what the margins need per
// character: its open position costs and its observed sales tax rate.
type beatenContext struct {
	app   *Application
	costs map[int64]map[int64]float64 // character -> type -> cost per unit
	pools map[int64]ledgerPool        // character -> the pool its positions sit in
	rates map[int64]float64
	stats map[[2]int64]float64 // (region, type) -> typical sell for a beaten buy's resale basis
}

// regionTypicalSell is the stat pack's typical sell for a type in a
// region, read at most once per pass however many beaten buys of
// that type the pass prices.
func (b *beatenContext) regionTypicalSell(ctx context.Context, regionID, typeID int64) float64 {
	if b.stats == nil {
		b.stats = map[[2]int64]float64{}
	}
	key := [2]int64{regionID, typeID}
	if v, ok := b.stats[key]; ok {
		return v
	}
	v := 0.0
	if s, err := b.app.queries.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{RegionID: regionID, TypeID: typeID}); err == nil {
		v = s.TypicalSell
	}
	b.stats[key] = v
	return v
}

func (b *beatenContext) unitCost(ctx context.Context, characterID, typeID int64) float64 {
	if b.costs == nil {
		b.costs = map[int64]map[int64]float64{}
	}
	m, ok := b.costs[characterID]
	if !ok {
		m = map[int64]float64{}
		b.costs[characterID] = m
	}
	if c, ok := m[typeID]; ok {
		return c
	}
	if b.pools == nil {
		b.pools = map[int64]ledgerPool{}
	}
	pool, ok := b.pools[characterID]
	if !ok {
		pool = b.app.profitPoolForCharacter(ctx, characterID)
		b.pools[characterID] = pool
	}
	rows, err := b.app.queries.ListPositionCostsForTypes(ctx, db.ListPositionCostsForTypesParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeIds: []int64{typeID}})
	c := 0.0
	if err == nil && len(rows) > 0 && rows[0].Quantity > 0 {
		c = rows[0].Cost / float64(rows[0].Quantity)
	}
	m[typeID] = c
	return c
}

// taxRate is the character's effective sales tax over the last 90
// days of its ledger, or 0 when it sold nothing in that time.
func (b *beatenContext) taxRate(ctx context.Context, characterID int64) float64 {
	if b.rates == nil {
		b.rates = map[int64]float64{}
	}
	if r, ok := b.rates[characterID]; ok {
		return r
	}
	r := 0.0
	if row, err := b.app.queries.GetObservedSalesTaxRate(ctx, db.GetObservedSalesTaxRateParams{
		CharacterID: characterID, Since: time.Now().UTC().AddDate(0, 0, -90),
	}); err == nil && row.Revenue > 0 && row.Tax > 0 {
		r = math.Min(row.Tax/row.Revenue, 0.5)
	}
	b.rates[characterID] = r
	return r
}

// verdict prices one open order against its station's book.
func (b *beatenContext) verdict(ctx context.Context, characterID int64, o esi.CharOrder, book []esi.MarketOrder, own map[int64]bool) (p db.UpdateOrderLifecycleBookParams, beaten bool) {
	best, volume, beaten := stationCompetition(o, book, own)
	p = db.UpdateOrderLifecycleBookParams{CharacterID: characterID, OrderID: o.OrderID, BeatingPrice: best, BeatingVolume: volume}
	if !beaten {
		return p, false
	}
	p.SuggestedPrice = nextPricePast(best, o.IsBuyOrder)
	p.TaxRate = b.taxRate(ctx, characterID)
	if o.IsBuyOrder {
		p.UnitCost = b.regionTypicalSell(ctx, o.RegionID, o.TypeID)
	} else {
		p.UnitCost = b.unitCost(ctx, characterID, o.TypeID)
	}
	if m, ok := beatenMargin(p.SuggestedPrice, p.UnitCost, p.TaxRate, o.IsBuyOrder); ok {
		p.MarginPct = sql.NullFloat64{Float64: m, Valid: true}
	}
	return p, true
}
