package fit

import (
	"maps"
	"sort"
)

// Implant solving: when a fit runs over its CPU or powergrid
// budget, the cheapest fix in the game is usually an implant that
// raises the ship's output by a few percent: a 'Gypsy' CPU or
// 'Squire' Power Grid hardwiring, or the Genolution Core
// Augmentation set. SolveImplants searches the combinations for
// the smallest sufficient set and reports the picks; the caller applies them as a
// simulation overlay (the fit document's implant overrides), so
// the pilot's real clone is never touched.

// The two resources an output implant can raise, named by the
// ship attribute they modify (dgmAttributeTypes).
const (
	ResourceCPU = AttrCPUOutput   // 48
	ResourcePG  = AttrPowerOutput // 11
)

// ResourceImplant is one ladder candidate: an implant type whose
// effects raise one fitting output, or (Resource 0) a set piece
// that raises other candidates' bonuses. Bonus is the ladder percent
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

// solveMaxTrials bounds the engine runs one solve may spend. The
// real ladders are small (the hardwiring slot's dozen rungs and
// the Genolution set's four slots), so a full search fits well
// under it; the bound only guards against a future SDE growing
// the candidate list.
const solveMaxTrials = 2000

// SolveImplants finds the implants that make an over-budget fit
// legal. compute runs the engine for a complete implant set;
// base is the fit's current effective set (clone plus overrides)
// and baseSlots maps each base implant's slot to its type. A pick
// replaces whatever occupies its slot, exactly like a document
// override.
//
// The search tries combinations across slots, fewest implants
// first: one implant rarely fixes both resources (the CPU and
// powergrid hardwirings share slot 6), while pieces in other
// slots can (Genolution's CA-1 and CA-2, whose set bonus also
// raises each other's). Among the legal sets of the smallest
// size the cheapest wins (an unknown price sorts last), then the
// smallest advertised bonus, then the lowest type IDs. Candidates
// with Resource 0 carry no output bonus of their own but raise
// other implants' bonuses (a set piece); they join a set only
// when it needs them. Success returns the picks (CPU's first,
// then powergrid's, then set pieces) and no unsolved resources;
// failure returns no picks and the resources over budget.
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
	// Each slot's options: every candidate that is not already
	// what the slot holds, in ladder order.
	bySlot := make(map[int][]ResourceImplant)
	for _, c := range sortedCandidates(candidates) {
		if baseSlots[c.Slot] == c.TypeID {
			continue
		}
		bySlot[c.Slot] = append(bySlot[c.Slot], c)
	}
	slots := make([]int, 0, len(bySlot))
	for s := range bySlot {
		slots = append(slots, s)
	}
	sort.Ints(slots)

	trials := 0
	var best []ResourceImplant
	var bestPrice, bestBonus float64
	var walk func(from, need int, set []ResourceImplant)
	walk = func(from, need int, set []ResourceImplant) {
		if trials >= solveMaxTrials {
			return
		}
		if need == 0 {
			current := append([]int64(nil), base...)
			slotMap := make(map[int]int64, len(baseSlots)+len(set))
			maps.Copy(slotMap, baseSlots)
			for _, c := range set {
				current, slotMap = withImplant(current, slotMap, c)
			}
			trials++
			res := compute(current)
			if resourceOver(res, ResourceCPU) || resourceOver(res, ResourcePG) {
				return
			}
			price, bonus := setCost(set)
			if best == nil || price < bestPrice || (price == bestPrice && bonus < bestBonus) {
				best = append([]ResourceImplant(nil), set...)
				bestPrice, bestBonus = price, bonus
			}
			return
		}
		for i := from; i <= len(slots)-need; i++ {
			for _, c := range bySlot[slots[i]] {
				walk(i+1, need-1, append(set, c))
			}
		}
	}
	for size := 1; size <= len(slots) && best == nil && trials < solveMaxTrials; size++ {
		walk(0, size, nil)
	}
	if best == nil {
		return nil, over
	}
	out := make([]SolvePick, 0, len(best))
	for _, resource := range []int64{ResourceCPU, ResourcePG, 0} {
		for _, c := range best {
			if c.Resource == resource {
				out = append(out, SolvePick{TypeID: c.TypeID, Slot: c.Slot, Resource: c.Resource, Bonus: c.Bonus})
			}
		}
	}
	return out, nil
}

// setCost is a candidate set's total price (an unknown price
// counts as dearer than any known one) and advertised bonus.
func setCost(set []ResourceImplant) (price, bonus float64) {
	for _, c := range set {
		p := c.Price
		if p <= 0 {
			p = 1e15
		}
		price += p
		bonus += c.Bonus
	}
	return price, bonus
}

// sortedCandidates is the candidates in ladder order: the
// smallest bonus first (the least implant that can do the job),
// the cheapest first among equals, an unknown price after every
// known one, then by type ID.
func sortedCandidates(candidates []ResourceImplant) []ResourceImplant {
	out := append([]ResourceImplant(nil), candidates...)
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
	maps.Copy(nextSlots, slots)
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
