package fitview

import (
	"testing"

	"evesynapse/internal/fit"
)

// capsSnapshot classifies the test types: 9001 a subsystem,
// 9100-9109 high-slot modules, 9003 a drone, 9004 cargo.
func capsSnapshot() *fit.Snapshot {
	snap := &fit.Snapshot{
		Attrs:       map[int64]map[int64]float64{},
		TypeEffects: map[int64][]int64{},
	}
	snap.TypeEffects[9001] = []int64{fit.EffectSubsystemSlot}
	for id := int64(9100); id <= 9109; id++ {
		snap.TypeEffects[id] = []int64{fit.EffectHiPower}
	}
	snap.Attrs[9003] = map[int64]float64{fit.AttrDroneBandwidthUsed: 25}
	return snap
}

func qtyOf(doc *FitDoc, typeID int64) (int, bool) {
	for _, it := range doc.Items {
		if it.TypeID == typeID {
			return it.Qty, true
		}
	}
	return 0, false
}

// TestSanitizeFitDocCapsHostileFit is the audit 1.4 document
// case: lines claiming 10,000 fitted instances are capped at
// what a hull can carry when the snapshot can classify them,
// while drone and cargo lines keep their bulk quantities.
func TestSanitizeFitDocCapsHostileFit(t *testing.T) {
	doc := &FitDoc{ShipTypeID: 587, Items: []FitDocItem{
		{TypeID: 9001, Qty: 10000, States: []string{fit.StateActive}},
		{TypeID: 9003, Qty: 10000},
		{TypeID: 9004, Qty: 10000},
	}}
	for id := int64(9100); id <= 9109; id++ {
		doc.Items = append(doc.Items, FitDocItem{TypeID: id, Qty: 10000})
	}
	SanitizeFitDoc(doc, capsSnapshot())

	if q, ok := qtyOf(doc, 9001); !ok || q != maxFittedPerLine {
		t.Fatalf("subsystem qty = %d (present %v), want %d", q, ok, maxFittedPerLine)
	}
	fittedTotal := 0
	for _, it := range doc.Items {
		switch it.TypeID {
		case 9003, 9004:
			if it.Qty != 10000 {
				t.Fatalf("bulk line %d qty = %d, want 10000 untouched", it.TypeID, it.Qty)
			}
		default:
			if it.Qty > maxFittedPerLine {
				t.Fatalf("fitted line %d qty = %d, want <= %d", it.TypeID, it.Qty, maxFittedPerLine)
			}
			fittedTotal += it.Qty
		}
	}
	if fittedTotal != maxFittedPerShip {
		t.Fatalf("fitted total = %d, want exactly the %d cap (hostile lines saturate it)", fittedTotal, maxFittedPerShip)
	}
	// The subsystem's states were normalized against its
	// capped quantity, its one real state first.
	for _, it := range doc.Items {
		if it.TypeID == 9001 {
			if len(it.States) != it.Qty || it.States[0] != fit.StateActive {
				t.Fatalf("subsystem states = %v, want %d entries starting with the active state", it.States, it.Qty)
			}
		}
	}
}

// TestSanitizeFitDocKeepsLegitFit: a real fit — a full rack of
// one gun, a bay of drones, a T3 subsystem set — passes through
// the caps unchanged.
func TestSanitizeFitDocKeepsLegitFit(t *testing.T) {
	doc := &FitDoc{ShipTypeID: 587, Items: []FitDocItem{
		{TypeID: 9100, Qty: 8},
		{TypeID: 9001, Qty: 5},
		{TypeID: 9003, Qty: 25},
		{TypeID: 9004, Qty: 5000},
	}}
	SanitizeFitDoc(doc, capsSnapshot())
	for _, want := range []struct {
		id  int64
		qty int
	}{{9100, 8}, {9001, 5}, {9003, 25}, {9004, 5000}} {
		if q, ok := qtyOf(doc, want.id); !ok || q != want.qty {
			t.Errorf("type %d qty = %d (present %v), want %d", want.id, q, ok, want.qty)
		}
	}
}

// TestSanitizeFitDocBoundsStateGrowth: without a snapshot the
// quantities keep the legacy bulk clamp, but the per-instance
// state array no longer grows to the claimed quantity — the
// sanitize step itself must not be the amplifier.
func TestSanitizeFitDocBoundsStateGrowth(t *testing.T) {
	doc := &FitDoc{ShipTypeID: 587, Items: []FitDocItem{
		{TypeID: 9100, Qty: 10000, States: []string{fit.StateActive}},
	}}
	SanitizeFitDoc(doc)
	it := doc.Items[0]
	if it.Qty != 10000 {
		t.Fatalf("unclassified qty = %d, want the legacy 10000 clamp", it.Qty)
	}
	if len(it.States) != maxFitStateEntries {
		t.Fatalf("states grew to %d entries, want the %d bound", len(it.States), maxFitStateEntries)
	}
	if it.States[0] != fit.StateActive {
		t.Fatalf("first state = %q, want the line's real state kept", it.States[0])
	}
}
