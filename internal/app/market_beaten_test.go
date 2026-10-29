package app

import (
	"math"
	"testing"

	"evesynapse/internal/esi"
)

// The suggested reprice is the next price the game allows past the
// competition (four significant digits, a cent at the finest), the
// owner's own orders never count as competition, and the margin is
// taken after sales tax.
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

	// Sell at 97.9 on a 90 ISK cost after 4% tax: (97.9*0.96-90)/97.9.
	if m, ok := beatenMargin(97.9, 90, 0.04, false); !ok || math.Abs(m-(97.9*0.96-90)/97.9*100) > 1e-9 {
		t.Errorf("sell margin = %v %v", m, ok)
	}
	if _, ok := beatenMargin(97.9, 0, 0.04, false); ok {
		t.Error("margin without a cost on record must be absent")
	}
}
