package fit

import "testing"

// TestFitEngineBurstBuffs pins command burst buffs: a running burst's
// buff acts on its own ship, and when the fleet carries the same buff
// the strongest value holds (Armor Resistance Bonus aggregates by
// minimum: the most negative).
func TestFitEngineBurstBuffs(t *testing.T) {
	const (
		ship   = 1004
		burst  = 2020
		charge = 2021
	)
	fx := newFitFixture()
	fx.attrs(ship, map[int64]float64{267: 0.5})
	fx.meta(267, false, 1)
	fx.group(burst, groupCommandBurst)
	fx.attrs(burst, map[int64]float64{2468: 13, 2469: -10})
	fx.attrs(charge, map[int64]float64{})
	fx.snap.Bursts = &BurstData{Buffs: map[int64]*Buff{
		13: {ID: 13, Name: "Armor Resistance Bonus", Minimum: true, Operation: opPostPercent,
			Mods: []BuffMod{{Kind: "item", Attr: 267}}},
	}}
	items := []ItemInput{{TypeID: burst, Quantity: 1}}
	loaded := map[int64]int64{burst: charge}

	own := ComputeFleet(fx.snap, ship, items, nil, loaded, nil, nil)
	near(t, "own burst", own.ShipAttrs[267], 0.45, 1e-9)

	fleet := ComputeFleet(fx.snap, ship, items, nil, loaded, nil, []BuffValue{{ID: 13, Value: -20}})
	near(t, "stronger fleet burst holds", fleet.ShipAttrs[267], 0.4, 1e-9)
	if len(fleet.Buffs) != 1 || !fleet.Buffs[0].Fleet || fleet.Buffs[0].Value != -20 {
		t.Fatalf("buffs acting = %+v, want the fleet's -20", fleet.Buffs)
	}
}
