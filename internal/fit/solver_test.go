package fit

import "testing"

// Solver fixtures: a hull with 575 tf CPU and 1000 MW powergrid,
// one module that eats 586.5 tf (the Pilgrim shape: over CPU by
// exactly 11.5), one that eats 1040 MW, and two implant ladders.
// CPU hardwirings (slot 6) post-percent the ship's cpuOutput by
// their bonus attribute 9601; powergrid hardwirings (slot 7) do
// the same to powerOutput via 9602 — the shape the real 'Gnome'
// and 'Squire' effects have in the SDE (ItemModifier on the ship,
// operation 6).
const (
	solveShip    = int64(1001)
	solveCPUHog  = int64(3001)
	solvePGHog   = int64(3002)
	solveCPU1    = int64(7001) // +1% CPU, slot 6
	solveCPU2    = int64(7002) // +2% CPU, slot 6
	solveCPU2Alt = int64(7007) // +2% CPU, slot 6, cheaper grade
	solveCPU5    = int64(7003) // +5% CPU, slot 6
	solvePG3     = int64(7004) // +3% PG, slot 7
	solvePG4     = int64(7006) // +4% PG, slot 7
	solvePG5     = int64(7005) // +5% PG, slot 7
)

func solverFixture() *fitFixture {
	fx := newFitFixture()
	fx.attrs(solveShip, map[int64]float64{
		AttrCPUOutput: 575, AttrPowerOutput: 1000,
		AttrHiSlots: 8, AttrMedSlots: 8, AttrLowSlots: 8, AttrRigSlots: 3,
	})
	fx.attrs(solveCPUHog, map[int64]float64{AttrCPU: 586.5, AttrPower: 10})
	fx.flag(solveCPUHog, EffectHiPower)
	fx.attrs(solvePGHog, map[int64]float64{AttrCPU: 0, AttrPower: 1040})
	fx.flag(solvePGHog, EffectHiPower)
	for _, attr := range []int64{AttrCPUOutput, AttrPowerOutput, 9601, 9602} {
		fx.meta(attr, false, 0)
	}
	cpu := []struct {
		id    int64
		bonus float64
	}{{solveCPU1, 1}, {solveCPU2, 2}, {solveCPU2Alt, 2}, {solveCPU5, 5}}
	for _, c := range cpu {
		fx.attrs(c.id, map[int64]float64{331: 6, 9601: c.bonus})
		fx.effect(c.id, 9701, 0, fitMod("shipID", "ItemModifier", AttrCPUOutput, 9601, 6))
	}
	pg := []struct {
		id    int64
		bonus float64
	}{{solvePG3, 3}, {solvePG4, 4}, {solvePG5, 5}}
	for _, p := range pg {
		fx.attrs(p.id, map[int64]float64{331: 7, 9602: p.bonus})
		fx.effect(p.id, 9702, 0, fitMod("shipID", "ItemModifier", AttrPowerOutput, 9602, 6))
	}
	return fx
}

func solverCandidates() []ResourceImplant {
	return []ResourceImplant{
		{TypeID: solveCPU1, Slot: 6, Resource: ResourceCPU, Bonus: 1, Price: 400000},
		{TypeID: solveCPU2, Slot: 6, Resource: ResourceCPU, Bonus: 2, Price: 5000000},
		{TypeID: solveCPU2Alt, Slot: 6, Resource: ResourceCPU, Bonus: 2, Price: 900000},
		{TypeID: solveCPU5, Slot: 6, Resource: ResourceCPU, Bonus: 5, Price: 90000000},
		{TypeID: solvePG3, Slot: 7, Resource: ResourcePG, Bonus: 3, Price: 800000},
		{TypeID: solvePG4, Slot: 7, Resource: ResourcePG, Bonus: 4, Price: 6000000},
		{TypeID: solvePG5, Slot: 7, Resource: ResourcePG, Bonus: 5, Price: 80000000},
	}
}

func solveWith(fx *fitFixture, items []ItemInput, base []int64, baseSlots map[int]int64, candidates []ResourceImplant) ([]SolvePick, []int64) {
	compute := func(implants []int64) *Result {
		return Compute(fx.snap, solveShip, items, nil, nil, implants)
	}
	return SolveImplants(compute, base, baseSlots, candidates)
}

func pickIDs(picks []SolvePick) []int64 {
	out := make([]int64, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.TypeID)
	}
	return out
}

// The Pilgrim case: over CPU by exactly 11.5 tf on a 575 tf
// output. +1% gives 580.75 (not enough), +2% gives exactly
// 586.5 (legal), so the smallest sufficient implant is the 2% —
// and between the two 2% grades, the cheaper one wins.
func TestSolveImplantsPilgrimCPU(t *testing.T) {
	fx := solverFixture()
	items := []ItemInput{{TypeID: solveCPUHog, Quantity: 1}}
	picks, unsolved := solveWith(fx, items, nil, nil, solverCandidates())
	if len(unsolved) != 0 {
		t.Fatalf("unsolved = %v, want none", unsolved)
	}
	if len(picks) != 1 || picks[0].TypeID != solveCPU2Alt {
		t.Fatalf("picks = %v, want the cheaper +2%% CPU implant %d", pickIDs(picks), solveCPU2Alt)
	}
	if picks[0].Slot != 6 || picks[0].Resource != ResourceCPU {
		t.Errorf("pick = %+v, want slot 6 CPU", picks[0])
	}
}

// Powergrid is symmetric: 1040 MW against 1000 is over by 4%,
// so the 3% ladder rung fails and the 4% is the smallest
// sufficient.
func TestSolveImplantsPowergrid(t *testing.T) {
	fx := solverFixture()
	items := []ItemInput{{TypeID: solvePGHog, Quantity: 1}}
	picks, unsolved := solveWith(fx, items, nil, nil, solverCandidates())
	if len(unsolved) != 0 {
		t.Fatalf("unsolved = %v, want none", unsolved)
	}
	if len(picks) != 1 || picks[0].TypeID != solvePG4 {
		t.Fatalf("picks = %v, want the +4%% PG implant %d", pickIDs(picks), solvePG4)
	}
}

// Both resources over, different slots: both get solved, CPU's
// pick listed first. The powergrid total is 1050 MW here (the
// CPU hog draws 10 of its own), so the PG fix is the 5% rung.
func TestSolveImplantsBothOver(t *testing.T) {
	fx := solverFixture()
	items := []ItemInput{{TypeID: solveCPUHog, Quantity: 1}, {TypeID: solvePGHog, Quantity: 1}}
	picks, unsolved := solveWith(fx, items, nil, nil, solverCandidates())
	if len(unsolved) != 0 {
		t.Fatalf("unsolved = %v, want none", unsolved)
	}
	if len(picks) != 2 || picks[0].TypeID != solveCPU2Alt || picks[1].TypeID != solvePG5 {
		t.Fatalf("picks = %v, want [%d %d]", pickIDs(picks), solveCPU2Alt, solvePG5)
	}
}

// Over by more than the strongest rung adds (a 700 tf module on
// a 575 tf output needs +21.7%; the ladder tops out at +5%): no
// solution, and the resource is reported unsolved.
func TestSolveImplantsNoneSuffices(t *testing.T) {
	fx := solverFixture()
	fx.attrs(int64(3009), map[int64]float64{AttrCPU: 700, AttrPower: 10})
	fx.flag(int64(3009), EffectHiPower)
	items := []ItemInput{{TypeID: 3009, Quantity: 1}}
	picks, unsolved := solveWith(fx, items, nil, nil, solverCandidates())
	if len(picks) != 0 {
		t.Fatalf("picks = %v, want none", pickIDs(picks))
	}
	if len(unsolved) != 1 || unsolved[0] != ResourceCPU {
		t.Fatalf("unsolved = %v, want [CPU]", unsolved)
	}
}

// Both ladders living in one slot (the real 'Gnome'/'Squire'
// situation is two families, one slot each in these fixtures —
// here they collide): with both resources over, one slot cannot
// hold both fixes, so the honest answer is no solution.
func TestSolveImplantsSlotConflict(t *testing.T) {
	fx := solverFixture()
	conflict := solverCandidates()
	for i := range conflict {
		if conflict[i].Resource == ResourcePG {
			conflict[i].Slot = 6
		}
	}
	items := []ItemInput{{TypeID: solveCPUHog, Quantity: 1}, {TypeID: solvePGHog, Quantity: 1}}
	picks, unsolved := solveWith(fx, items, nil, nil, conflict)
	if len(picks) != 0 {
		t.Fatalf("picks = %v, want none (slot conflict)", pickIDs(picks))
	}
	if len(unsolved) != 2 {
		t.Fatalf("unsolved = %v, want both resources", unsolved)
	}
}

// A clone already wearing the weak rung: the solver evicts it
// and steps up the ladder instead of stacking or giving up.
func TestSolveImplantsReplacesWeakCloneImplant(t *testing.T) {
	fx := solverFixture()
	items := []ItemInput{{TypeID: solveCPUHog, Quantity: 1}}
	base := []int64{solveCPU1}
	baseSlots := map[int]int64{6: solveCPU1}
	picks, unsolved := solveWith(fx, items, base, baseSlots, solverCandidates())
	if len(unsolved) != 0 {
		t.Fatalf("unsolved = %v, want none", unsolved)
	}
	if len(picks) != 1 || picks[0].TypeID != solveCPU2Alt {
		t.Fatalf("picks = %v, want [%d] replacing the +1%%", pickIDs(picks), solveCPU2Alt)
	}
}

// A legal fit needs nothing: no picks, nothing unsolved.
func TestSolveImplantsAlreadyLegal(t *testing.T) {
	fx := solverFixture()
	picks, unsolved := solveWith(fx, nil, nil, nil, solverCandidates())
	if len(picks) != 0 || len(unsolved) != 0 {
		t.Fatalf("picks = %v unsolved = %v, want both empty", pickIDs(picks), unsolved)
	}
}
