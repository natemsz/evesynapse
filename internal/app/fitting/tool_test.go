package fitting

import (
	"fmt"
	"testing"

	"evesynapse/internal/fitview"
)

// TestFitESIFittingBody: the editor document maps to the ESI
// fitting body — one flag per module instance (HiSlot0-7 etc.),
// charges riding their weapon's slot, drones to DroneBay and cargo
// to Cargo. Over-slot documents are an error, never silently
// truncated.
const (
	fitShipID    = int64(1001)
	fitBlasterID = int64(3001)
	fitPlateID   = int64(3002)
)

func TestFitESIFittingBody(t *testing.T) {
	const (
		droneID  = int64(9001)
		cargoID  = int64(9002)
		chargeID = int64(9003)
	)
	doc := &fitview.FitDoc{
		Name:       "Test fit",
		ShipTypeID: fitShipID,
		Items: []fitview.FitDocItem{
			{TypeID: fitBlasterID, Qty: 2},
			{TypeID: fitPlateID, Qty: 1},
			{TypeID: droneID, Qty: 3},
			{TypeID: cargoID, Qty: 10},
		},
		Charges: map[int64]int64{fitBlasterID: chargeID},
	}
	familyOf := map[int64]string{
		fitBlasterID: fitview.FitFamilyHigh,
		fitPlateID:   fitview.FitFamilyLow,
		droneID:      fitview.FitFamilyDrone,
		cargoID:      fitview.FitFamilyCargo,
	}
	body, err := fitESIFittingBody("Test fit", doc, familyOf)
	if err != nil {
		t.Fatalf("fitESIFittingBody: %v", err)
	}
	if body.Name != "Test fit" || body.ShipTypeID != fitShipID {
		t.Errorf("body name/ship = %q/%d", body.Name, body.ShipTypeID)
	}
	if body.Description == "" {
		t.Error("body description should mark EveSynapse as the creator")
	}
	got := map[string][]int64{}
	qty := map[string]int64{}
	for _, it := range body.Items {
		got[it.Flag] = append(got[it.Flag], it.TypeID)
		qty[it.Flag] = it.Quantity
	}
	// One flag per module instance; the charge rides the first
	// blaster's slot (HiSlot0 carries both the module and its ammo).
	want := map[string][]int64{
		"HiSlot0":  {fitBlasterID, chargeID},
		"HiSlot1":  {fitBlasterID},
		"LoSlot0":  {fitPlateID},
		"DroneBay": {droneID},
		"Cargo":    {cargoID},
	}
	for flag, ids := range want {
		if fmt.Sprint(got[flag]) != fmt.Sprint(ids) {
			t.Errorf("flag %s = %v, want %v", flag, got[flag], ids)
		}
	}
	if qty["DroneBay"] != 3 || qty["Cargo"] != 10 {
		t.Errorf("drone/cargo quantities = %d/%d, want 3/10", qty["DroneBay"], qty["Cargo"])
	}
	if len(body.Items) != 6 { // 2 blasters + plate + drone + cargo + charge
		t.Errorf("items = %d, want 6", len(body.Items))
	}

	// Nine high-slot modules: no HiSlot8 exists, so this is an
	// error rather than a silent drop.
	doc.Items = []fitview.FitDocItem{{TypeID: fitBlasterID, Qty: 9}}
	if _, err := fitESIFittingBody("Test fit", doc, familyOf); err == nil {
		t.Error("9 high-slot modules should be an error, got nil")
	}
}
