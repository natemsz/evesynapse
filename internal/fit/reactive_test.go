package fit

import "testing"

// TestReactiveHardenerAdaptsToEvenDamage pins the hardener's split
// on an Omen's armor (resonances 0.50/0.65/0.75/0.80, EM, thermal,
// kinetic, explosive) under even damage: the cycles settle into a
// three-step loop whose average holds no EM and 10.5/21/28.5%
// thermal/kinetic/explosive, the full 60% of the module.
func TestReactiveHardenerAdaptsToEvenDamage(t *testing.T) {
	got := adaptReactiveSplit([4]float64{0.5, 0.65, 0.75, 0.8}, [4]float64{0.85, 0.85, 0.85, 0.85}, 0.06)
	want := [4]float64{1, 0.895, 0.79, 0.715}
	for i := range want {
		near(t, "hardener resonance", got[i], want[i], 1e-9)
	}
}
