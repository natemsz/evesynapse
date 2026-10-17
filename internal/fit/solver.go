package fit

import "sort"

// Implant solving: when a fit runs over its CPU or powergrid
// budget, the cheapest fix in the game is usually a hardwiring —
// a 'Gnome' CPU Tuning or 'Squire' Power Grid implant that raises
// the ship's output by a few percent. SolveImplants searches the
// ladder for the smallest sufficient implant per over-budget
// resource and reports the picks; the caller applies them as a
// simulation overlay (the fit document's implant overrides), so
// the pilot's real clone is never touched.

// The two resources an output implant can raise, named by the
// ship attribute they modify (dgmAttributeTypes).
const (
	ResourceCPU = AttrCPUOutput   // 48
	ResourcePG  = AttrPowerOutput // 11
)

// ResourceImplant is one ladder candidate: an implant type whose
// effects raise one fitting output. Bonus is the ladder percent
// the implant advertises (its modifying attribute's value); Price
// is the guide price, 0 when unknown.
type ResourceImplant struct {
	TypeID   int64
	Slot     int
	Resource int64 // ResourceCPU or ResourcePG
	Bonus    float64
	Price    float64
}

// SolvePick is one chosen implant.
type SolvePick struct {
	TypeID   int64
	Slot     int
	Resource int64
	Bonus    float64
}

// solveEpsilon is the fitting tolerance: percent folds leave
// residue around 1e-12 on values in the hundreds, so "over" is
// judged against a hair above the budget, never exactly at it.
const solveEpsilon = 1e-6

func resourceOver(res *Result, resource int64) bool {
	if res == nil {
		return false
	}
	switch resource {
	case ResourceCPU:
		return res.CPUUsed > res.CPUMax+solveEpsilon
	case ResourcePG:
		return res.PowergridUsed > res.PowergridMax+solveEpsilon
	}
	return false
}

// SolveImplants finds the implants that make an over-budget fit
// legal. compute runs the engine for a complete implant set;
// base is the fit's current effective set (clone plus overrides)
// and baseSlots maps each base implant's slot to its type. A pick
// replaces whatever occupies its slot, exactly like a document
// override, and a trial counts only when the resource it targets
// fits and the other resource is no worse than before the trial.
//
// The search solves CPU first, then powergrid (the owner's
// order), and retries in the opposite order when the first pass
// fails — the two ladders can share a slot, and one order can
// succeed where the other cannot. Success returns the picks
// (CPU's first) and no unsolved resources; failure returns the
// picks that were not enough — none, the search is
// all-or-nothing — plus the resources still over budget.
func SolveImplants(compute func(implants []int64) *Result, base []int64, baseSlots map[int]int64, candidates []ResourceImplant) (picks []SolvePick, unsolved []int64) {
	baseRes := compute(base)
	var over []int64
	for _, r := range []int64{ResourceCPU, ResourcePG} {
		if resourceOver(baseRes, r) {
			over = append(over, r)
		}
	}
	if len(over) == 0 {
		return nil, nil
	}
	orders := [][2]int64{
		{ResourceCPU, ResourcePG},
		{ResourcePG, ResourceCPU},
	}
	for _, order := range orders {
		if got, ok := solveInOrder(compute, base, baseSlots, candidates, order); ok {
			return got, nil
		}
	}
	return nil, over
}

// solveInOrder tries to clear the two resources in the given
// order, one smallest-sufficient implant each.
func solveInOrder(compute func(implants []int64) *Result, base []int64, baseSlots map[int]int64, candidates []ResourceImplant, order [2]int64) ([]SolvePick, bool) {
	current := append([]int64(nil), base...)
	slots := make(map[int]int64, len(baseSlots))
	for s, id := range baseSlots {
		slots[s] = id
	}
	chosen := make(map[int]ResourceImplant)
	for _, resource := range order {
		res := compute(current)
		if !resourceOver(res, resource) {
			continue
		}
		other := order[0]
		if other == resource {
			other = order[1]
		}
		otherWasOver := resourceOver(res, other)
		found := false
		for _, cand := range ladderFor(candidates, resource) {
			trial, trialSlots := withImplant(current, slots, cand)
			trialRes := compute(trial)
			if resourceOver(trialRes, resource) {
				continue
			}
			// Replacing a same-slot implant can withdraw the
			// other resource's bonus: never let a pick push a
			// resource that currently fits over budget.
			if !otherWasOver && resourceOver(trialRes, other) {
				continue
			}
			current, slots = trial, trialSlots
			chosen[cand.Slot] = cand
			found = true
			break
		}
		if !found {
			return nil, false
		}
	}
	final := compute(current)
	if resourceOver(final, ResourceCPU) || resourceOver(final, ResourcePG) {
		return nil, false
	}
	out := make([]SolvePick, 0, len(chosen))
	for _, resource := range []int64{ResourceCPU, ResourcePG} {
		for _, cand := range chosen {
			if cand.Resource == resource {
				out = append(out, SolvePick{TypeID: cand.TypeID, Slot: cand.Slot, Resource: cand.Resource, Bonus: cand.Bonus})
			}
		}
	}
	return out, true
}

// ladderFor is one resource's candidates in pick order: the
// smallest bonus first (the least implant that can do the job),
// the cheapest first among equals — the ladder grades share
// percents, and the low-grade is the cheap one. An unknown price
// sorts after every known one at the same bonus.
func ladderFor(candidates []ResourceImplant, resource int64) []ResourceImplant {
	var out []ResourceImplant
	for _, c := range candidates {
		if c.Resource == resource && c.Bonus > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Bonus != b.Bonus {
			return a.Bonus < b.Bonus
		}
		pa, pb := a.Price, b.Price
		if pa <= 0 {
			pa = 1 << 62
		}
		if pb <= 0 {
			pb = 1 << 62
		}
		if pa != pb {
			return pa < pb
		}
		return a.TypeID < b.TypeID
	})
	return out
}

// withImplant returns the implant set with cand plugged into its
// slot, evicting whatever the slot held (a clone implant or an
// earlier pick).
func withImplant(current []int64, slots map[int]int64, cand ResourceImplant) ([]int64, map[int]int64) {
	nextSlots := make(map[int]int64, len(slots)+1)
	for s, id := range slots {
		nextSlots[s] = id
	}
	evicted := nextSlots[cand.Slot] // 0 when the slot is empty
	nextSlots[cand.Slot] = cand.TypeID
	next := make([]int64, 0, len(current)+1)
	for _, id := range current {
		if id == evicted || id == cand.TypeID {
			continue
		}
		next = append(next, id)
	}
	next = append(next, cand.TypeID)
	return next, nextSlots
}
