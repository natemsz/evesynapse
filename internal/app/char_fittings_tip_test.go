package app

import (
	"strings"
	"testing"

	"evesynapse/internal/fit"
)

// TestFitTipLinesByModuleKind: each kind of module shows what it
// changes — a hardener its four resists in their colors (heated
// figures when overheated), a subsystem the slots it adds, a passive
// module its bonus, an MWD its boost and signature bloom.
func TestFitTipLinesByModuleKind(t *testing.T) {
	const invuln, subsystem, mwd, extender = 1, 2, 3, 4
	res := &fit.Result{
		ItemAttrs: map[int64]map[int64]float64{
			invuln:    {984: -30, 985: -30, 986: -30, 987: -30, fit.AttrCapacitorNeed: 40, fit.AttrCPU: 29},
			subsystem: {fit.AttrHiSlotModifier: 2, fit.AttrMedSlotModifier: 1, fit.AttrTurretHardPointModifier: 2},
			mwd:       {fit.AttrSpeedFactor: 505, fitAttrSignatureRadiusBonus: 500, fit.AttrCPU: 50},
			extender:  {fitAttrCapacityBonus: 2600, fit.AttrPower: 165},
		},
		StateAttrs: map[string]map[int64]float64{
			fit.StateAttrKey(invuln, fit.StateOverheated): {984: -36, 985: -36, 986: -36, 987: -36},
		},
	}
	doc := &fitDoc{Charges: map[int64]int64{}}
	lines := func(typeID int64, state string) string {
		var b strings.Builder
		for _, l := range fitTipLines(res, nil, doc, typeID, state, fitEffAttrs(res, nil, typeID, state)) {
			b.WriteString(l.Label + "|" + l.Value + "|" + l.Class + ";")
		}
		return b.String()
	}

	got := lines(invuln, "")
	for _, want := range []string{"EM|+30%|res-em;", "Thermal|+30%|res-th;", "Kinetic|+30%|res-kin;", "Explosive|+30%|res-exp;", "Cap|40 GJ|", "CPU|29 tf|"} {
		if !strings.Contains(got, want) {
			t.Errorf("invuln: missing %q in %s", want, got)
		}
	}
	if heated := lines(invuln, fit.StateOverheated); !strings.Contains(heated, "EM|+36%|res-em") {
		t.Errorf("overheated invuln should show its heated resists: %s", heated)
	}
	got = lines(subsystem, "")
	for _, want := range []string{"High slots|+2|", "Mid slots|+1|", "Turrets|+2|"} {
		if !strings.Contains(got, want) {
			t.Errorf("subsystem: missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "Low slots") {
		t.Errorf("subsystem: a zero modifier should not show: %s", got)
	}
	got = lines(mwd, "")
	for _, want := range []string{"Boost|505%|", "Signature|+500%|"} {
		if !strings.Contains(got, want) {
			t.Errorf("mwd: missing %q in %s", want, got)
		}
	}
	if got = lines(extender, ""); !strings.Contains(got, "Shield HP|+2,600|") && !strings.Contains(got, "Shield HP|+2600|") {
		t.Errorf("extender: missing its shield HP in %s", got)
	}
}
