package fit

import "testing"

// TestFitComputeHostileQuantities pins the fitting-DoS bounds
// (audit 1.4): a document claiming 10,000 instances of a rack
// type must not turn into 10,000 bound modifiers or tens of
// thousands of slots. Rack instances cap at 16 per type and 64
// across the fit in addItems, and slot counts clamp to the
// game's own maximum of 8 in slotsAndHardpoints.
func TestFitComputeHostileQuantities(t *testing.T) {
	const (
		shipID = int64(6001)
		subID  = int64(6101)
	)
	guns := []int64{6201, 6202, 6203, 6204, 6205}

	build := func() *Snapshot {
		f := newFitFixture()
		f.attrs(shipID, map[int64]float64{
			AttrHiSlots: 0, AttrMedSlots: 0, AttrLowSlots: 0, AttrRigSlots: 3,
		})
		// A subsystem granting 7 high slots per instance.
		f.attrs(subID, map[int64]float64{AttrHiSlotModifier: 7})
		f.effect(subID, EffectSubsystemSlot, 0)
		for _, g := range guns {
			f.effect(g, EffectHiPower, 0)
		}
		return f.snap
	}

	// A subsystem line at qty 10,000: 16 capped instances x 7
	// granted slots is 112 before the slot clamp, 70,000 with
	// no caps at all. The result must be the game's maximum.
	res := Compute(build(), shipID, []ItemInput{{TypeID: subID, Quantity: 10000}}, nil, nil, nil)
	if res.HighSlots != 8 {
		t.Fatalf("subsystem qty 10000: HighSlots = %d, want 8 (clamped)", res.HighSlots)
	}

	// One gun type at qty 10,000 binds as 16 instances, not 10,000.
	res = Compute(build(), shipID, []ItemInput{{TypeID: guns[0], Quantity: 10000}}, nil, nil, nil)
	if res.HighSlotsUsed != 16 {
		t.Fatalf("gun qty 10000: HighSlotsUsed = %d, want 16 (per-type instance cap)", res.HighSlotsUsed)
	}

	// Five gun types at qty 10,000 each: the fit-wide rack
	// total caps at 64, so the fifth type contributes nothing.
	items := make([]ItemInput, 0, len(guns))
	for _, g := range guns {
		items = append(items, ItemInput{TypeID: g, Quantity: 10000})
	}
	res = Compute(build(), shipID, items, nil, nil, nil)
	if res.HighSlotsUsed != 64 {
		t.Fatalf("five gun types at qty 10000: HighSlotsUsed = %d, want 64 (fit-wide rack cap)", res.HighSlotsUsed)
	}

	// A legitimate stack passes through untouched.
	res = Compute(build(), shipID, []ItemInput{{TypeID: guns[0], Quantity: 8}}, nil, nil, nil)
	if res.HighSlotsUsed != 8 {
		t.Fatalf("gun qty 8: HighSlotsUsed = %d, want 8", res.HighSlotsUsed)
	}
}
