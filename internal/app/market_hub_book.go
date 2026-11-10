package app

// The hub book: each swept region's whole order book held in memory
// between sweeps, in a compact form (about 70 bytes an order, some
// 60 MB for every hub together). It does two jobs.
//
// A sweep compares the book it just read with the one it read last
// time, item by item, and writes only the items whose orders changed
// (storeRegionBook). Only about half a percent of a hub's orders
// change between two of ESI's five-minute windows, so most of a
// sweep's database work goes away.
//
// It is also the order book every market surface reads for a hub:
// the item page, the order-health pass and the warming that reads
// books ahead of a view take it from memory instead of asking ESI
// for the item's book again, since the sweep read the same ESI
// answer moments ago.
//
// Low-RAM mode (LOW_RAM_MODE=1) keeps no book between sweeps: each
// sweep then writes every item, as before, and the surfaces ask ESI.

import (
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// bookOrder is one order as the hub book holds it.
type bookOrder struct {
	OrderID      int64
	LocationID   int64
	SystemID     int64
	Price        float64
	VolumeRemain int64
	VolumeTotal  int64
	Issued       int64 // unix seconds
	Duration     int32
	Range        uint8 // index into orderRanges
	IsBuy        bool
}

// orderRanges is ESI's order range enum, which bookOrder stores as
// an index. A value outside it reads back as "".
var orderRanges = []string{"", "station", "solarsystem", "region", "1", "2", "3", "4", "5", "10", "20", "30", "40"}

func orderRangeIndex(r string) uint8 {
	for i, v := range orderRanges {
		if v == r {
			return uint8(i)
		}
	}
	return 0
}

func compactOrder(o esi.MarketOrder) bookOrder {
	var issued int64
	if t, err := time.Parse(time.RFC3339, o.Issued); err == nil {
		issued = t.Unix()
	}
	return bookOrder{
		OrderID: o.OrderID, LocationID: o.LocationID, SystemID: o.SystemID,
		Price: o.Price, VolumeRemain: o.VolumeRemain, VolumeTotal: o.VolumeTotal,
		Issued: issued, Duration: int32(o.Duration), Range: orderRangeIndex(o.Range),
		IsBuy: o.IsBuyOrder,
	}
}

func (o bookOrder) marketOrder(typeID int64) esi.MarketOrder {
	issued := ""
	if o.Issued != 0 {
		issued = time.Unix(o.Issued, 0).UTC().Format(time.RFC3339)
	}
	return esi.MarketOrder{
		OrderID: o.OrderID, TypeID: typeID, LocationID: o.LocationID, SystemID: o.SystemID,
		IsBuyOrder: o.IsBuy, Price: o.Price, VolumeTotal: o.VolumeTotal, VolumeRemain: o.VolumeRemain,
		Range: orderRanges[o.Range], Issued: issued, Duration: int(o.Duration),
	}
}

// typeBook is one item's orders in one region, each side sorted by
// price ascending (then order ID), so two reads of the same book
// compare equal element by element.
type typeBook struct {
	sells, buys []bookOrder
}

func sortBookSide(side []bookOrder) {
	sort.Slice(side, func(i, j int) bool {
		if side[i].Price != side[j].Price {
			return side[i].Price < side[j].Price
		}
		return side[i].OrderID < side[j].OrderID
	})
}

func sameBookSide(a, b []bookOrder) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameTypeBook reports whether two reads of an item's book hold the
// same orders with the same figures.
func sameTypeBook(a, b *typeBook) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameBookSide(a.sells, b.sells) && sameBookSide(a.buys, b.buys)
}

// hubBook is one region's whole book from one sweep.
type hubBook struct {
	types        map[int64]*typeBook
	orders       int
	sweptAt      time.Time         // when the sweep stored it
	lastModified time.Time         // ESI's Last-Modified on every page read
	expires      time.Time         // ESI's Expires on page 1
	day          string            // the UTC day the sweep wrote, "2006-01-02"
	avgVolume    map[int64]float64 // the 7-day sold-per-day figure stored beside each item
}

// newHubBook groups a read book's orders by item and sorts each side.
func newHubBook(pages [][]esi.MarketOrder) *hubBook {
	b := &hubBook{types: make(map[int64]*typeBook)}
	for _, page := range pages {
		for _, o := range page {
			tb := b.types[o.TypeID]
			if tb == nil {
				tb = &typeBook{}
				b.types[o.TypeID] = tb
			}
			if o.IsBuyOrder {
				tb.buys = append(tb.buys, compactOrder(o))
			} else {
				tb.sells = append(tb.sells, compactOrder(o))
			}
			b.orders++
		}
	}
	for _, tb := range b.types {
		sortBookSide(tb.sells)
		sortBookSide(tb.buys)
	}
	return b
}

// hubBookServeMaxAge is how old a held hub book may be and still
// serve: two sweep gates and a margin. Past it (the sweeps stopped)
// the surfaces go back to asking ESI.
var hubBookServeMaxAge = 2*regionSweepGate + 5*time.Minute

// heldHubBook is the region's held book, or nil.
func (app *Application) heldHubBook(regionID int64) *hubBook {
	app.hubBooksMu.RLock()
	defer app.hubBooksMu.RUnlock()
	return app.hubBooks[regionID]
}

func (app *Application) holdHubBook(regionID int64, b *hubBook) {
	if app.cfg.lowRAMMode {
		return
	}
	app.hubBooksMu.Lock()
	defer app.hubBooksMu.Unlock()
	if app.hubBooks == nil {
		app.hubBooks = make(map[int64]*hubBook)
	}
	app.hubBooks[regionID] = b
}

// hubOrderBook serves one item's book in a swept region from the
// held hub book, split into sides: sells price-ascending, buys
// price-descending. ok is false when no current book is held for
// the region; an item absent from a held book has no orders there.
func (app *Application) hubOrderBook(regionID, typeID int64) (sells, buys []esi.MarketOrder, sweptAt time.Time, ok bool) {
	b := app.heldHubBook(regionID)
	if b == nil || time.Since(b.sweptAt) > hubBookServeMaxAge {
		return nil, nil, time.Time{}, false
	}
	tb := b.types[typeID]
	if tb == nil {
		return []esi.MarketOrder{}, []esi.MarketOrder{}, b.sweptAt, true
	}
	sells = make([]esi.MarketOrder, len(tb.sells))
	for i, o := range tb.sells {
		sells[i] = o.marketOrder(typeID)
	}
	buys = make([]esi.MarketOrder, len(tb.buys))
	for i := range tb.buys {
		buys[i] = tb.buys[len(tb.buys)-1-i].marketOrder(typeID)
	}
	return sells, buys, b.sweptAt, true
}
