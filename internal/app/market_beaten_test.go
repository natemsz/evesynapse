package app

import (
	"math"
	"testing"

	"evesynapse/internal/esi"
	"evesynapse/internal/tradefees"
)

// The suggested reprice is the next price the game allows past the
// competition (four significant digits, a cent at the finest), the
// owner's own orders never count as competition, and the margin is
// taken after broker fee and sales tax via tradefees.
func TestBeatenOrderPricing(t *testing.T) {
	for _, c := range []struct {
		beating float64
		buy     bool
		want    float64
	}{
		{1000, false, 999.9},  // crossing down a decade: the finer tick
		{1234, false, 1233},   // tick 1 at four digits
		{12.34, false, 12.33}, // tick 0.01
		{5_000_000, false, 4_999_000},
		{999.9, true, 1000}, // crossing up a decade
		{1234, true, 1235},
		{0.01, false, 0.01}, // never under a cent
	} {
		if got := nextPricePast(c.beating, c.buy); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("nextPricePast(%v, buy=%v) = %v, want %v", c.beating, c.buy, got, c.want)
		}
	}

	mine := esi.CharOrder{OrderID: 1, LocationID: 60003760, Price: 100}
	book := []esi.MarketOrder{
		{OrderID: 1, LocationID: 60003760, Price: 100, VolumeRemain: 5},
		{OrderID: 2, LocationID: 60003760, Price: 95, VolumeRemain: 7}, // the owner's alt
		{OrderID: 3, LocationID: 60003760, Price: 98, VolumeRemain: 3},
		{OrderID: 4, LocationID: 60003760, Price: 98, VolumeRemain: 2},
		{OrderID: 5, LocationID: 60008494, Price: 50, VolumeRemain: 9}, // another station
	}
	best, vol, beaten := stationCompetition(mine, book, map[int64]bool{1: true, 2: true})
	if !beaten || best != 98 || vol != 5 {
		t.Errorf("competition = %v x%d beaten=%v, want 98 x5 beaten (alt and other station ignored)", best, vol, beaten)
	}

	broker, tax := tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax()
	// Sell at 97.9 on a 90 ISK cost after broker fee and sales tax:
	wantSell := tradefees.NetSellMarginPct(90, 97.9, broker, tax)
	if m, ok := beatenMargin(97.9, 90, broker, tax, false); !ok || math.Abs(m-wantSell) > 1e-9 {
		t.Errorf("sell margin = %v %v, want %v", m, ok, wantSell)
	}
	// Buy at 87 resold at 100 after broker fees and sales tax:
	wantBuy := tradefees.NetMarginPct(87, 100, broker, tax)
	if m, ok := beatenMargin(87, 100, broker, tax, true); !ok || math.Abs(m-wantBuy) > 1e-9 {
		t.Errorf("buy margin = %v %v, want %v", m, ok, wantBuy)
	}
	if _, ok := beatenMargin(97.9, 0, broker, tax, false); ok {
		t.Error("margin without a cost on record must be absent")
	}
}
