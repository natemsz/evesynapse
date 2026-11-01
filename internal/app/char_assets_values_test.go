package app

// The per-location value fold (schema 118): every stack files
// under the place its outermost container sits in; value counts
// everything a price can value, fitted modules and cargo
// included; ships and volume count the top level only; a stack
// with no price counts as unpriced, never as value.

import (
	"testing"

	"evesynapse/internal/esi"
)

func TestFoldAssetLocationValues(t *testing.T) {
	station := int64(60003760)
	other := int64(60008494)
	items := []esi.Asset{
		// An assembled ship with a fitted module and cargo.
		{ItemID: 1, TypeID: 100, Quantity: 1, IsSingleton: true, LocationID: station, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 2, TypeID: 200, Quantity: 1, IsSingleton: true, LocationID: 1, LocationType: "item", LocationFlag: "HiSlot0"},
		{ItemID: 3, TypeID: 300, Quantity: 50, LocationID: 1, LocationType: "item", LocationFlag: "Cargo"},
		// Loose stacks at the station: one priced, one not.
		{ItemID: 4, TypeID: 400, Quantity: 1000, LocationID: station, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 5, TypeID: 500, Quantity: 7, LocationID: station, LocationType: "station", LocationFlag: "Hangar"},
		// A container holding a priced stack.
		{ItemID: 6, TypeID: 600, Quantity: 1, IsSingleton: true, LocationID: station, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 7, TypeID: 400, Quantity: 5, LocationID: 6, LocationType: "item", LocationFlag: "Unlocked"},
		// And one loose stack somewhere else entirely.
		{ItemID: 8, TypeID: 400, Quantity: 3, LocationID: other, LocationType: "station", LocationFlag: "Hangar"},
	}
	prices := map[int64]float64{100: 10, 200: 3, 300: 2, 400: 1, 600: 4} // type 500 has no price
	volumes := map[int64]float64{100: 1000, 200: 5, 300: 1, 400: 0.5, 500: 2, 600: 100}

	rows := foldAssetLocationValues(items,
		func(typeID int64) bool { return typeID == 100 },
		func(typeID int64) (float64, bool) { p, ok := prices[typeID]; return p, ok },
		func(typeID int64) float64 { return volumes[typeID] })
	if len(rows) != 2 {
		t.Fatalf("folded %d places, want 2: %+v", len(rows), rows)
	}
	byLoc := map[int64]assetLocValue{}
	for _, r := range rows {
		byLoc[r.LocationID] = r
	}

	got := byLoc[station]
	// Every stack at the station counts, the fitted module and
	// the container's contents included.
	if got.TotalStacks != 7 {
		t.Errorf("stacks = %d, want 7", got.TotalStacks)
	}
	if got.TotalItems != 1065 { // 1+1+50+1000+7+1+5
		t.Errorf("items = %d, want 1065", got.TotalItems)
	}
	// Value counts everything priced, at any depth: the hull 10,
	// the module 3, the cargo 2x50, the loose 1000, the container
	// itself 4, and its contents 5. The unpriced stack adds none.
	if got.EstValue != 1122 {
		t.Errorf("value = %v, want 1122", got.EstValue)
	}
	if got.PricedStacks != 6 {
		t.Errorf("priced stacks = %d, want 6 (one stack has no price)", got.PricedStacks)
	}
	// One ship at the top level; the fitted module inside it is
	// not a second one.
	if got.Ships != 1 {
		t.Errorf("ships = %d, want 1", got.Ships)
	}
	// Volume counts the top level only: the hull 1000, the loose
	// 1000x0.5, the unpriced 7x2, the container 100. What is
	// inside the ship or the container adds nothing here.
	if got.Volume != 1614 {
		t.Errorf("volume = %v, want 1614", got.Volume)
	}

	got = byLoc[other]
	if got.TotalStacks != 1 || got.TotalItems != 3 || got.EstValue != 3 || got.Ships != 0 || got.Volume != 1.5 {
		t.Errorf("other place = %+v, want one priced stack of 3 worth 3 at volume 1.5", got)
	}
}
