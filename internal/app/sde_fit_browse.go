package app

import (
	"sort"

	"evesynapse/internal/fit"
	"evesynapse/internal/fitview"
)

// fitBrowseGroup is one market group of a family's Browse tree
// (schema 142): its parent in that tree and how many of the
// family's types sit under it.
type fitBrowseGroup struct {
	family        string
	marketGroupID int64
	parentGroupID int64
	name          string
	typeCount     int
}

// fitBrowseType is one type listed in a family's Browse tree.
type fitBrowseType struct {
	family        string
	typeID        int64
	marketGroupID int64
	name          string
}

// fitBrowseFamilies is the order the Browse tree's families are
// built in; each published market type lands in at most one.
var fitBrowseFamilies = []string{
	fitview.FitFamilyHigh, fitview.FitFamilyMedium, fitview.FitFamilyLow,
	fitview.FitFamilyRig, fitview.FitFamilySubsystem,
	fitview.FitFamilyDrone, fitview.FitFamilyFighter, "charge",
}

// The dump's categories of ammunition and other charges, and of
// structure modules and rigs (not for ships, so not in the tree).
const (
	sdeCategoryCharge          = 8
	sdeCategoryStructureModule = 66
)

// buildFitBrowse sorts the dump's published market types into the
// workbench's slot families (the same order fitview.FitSlotFamilyOf
// classifies in, plus charges) and lays out, per family, the market
// groups that hold them. The top of a family's tree is the level
// under the market's own roots: a root that lists no types itself
// (Ship Equipment, Ammunition & Charges) gives way to its children,
// and while the top is a single group without types of its own, its
// children stand in for it.
func buildFitBrowse(parsed *parsedSDE) ([]fitBrowseGroup, []fitBrowseType) {
	slotEffects := map[int64]string{
		fit.EffectRigSlot:       fitview.FitFamilyRig,
		fit.EffectSubsystemSlot: fitview.FitFamilySubsystem,
		fit.EffectHiPower:       fitview.FitFamilyHigh,
		fit.EffectMedPower:      fitview.FitFamilyMedium,
		fit.EffectLoPower:       fitview.FitFamilyLow,
	}
	effectFam := map[int64]map[string]bool{}
	for _, te := range parsed.typeEffects {
		if fam, ok := slotEffects[te.effectID]; ok {
			if effectFam[te.typeID] == nil {
				effectFam[te.typeID] = map[string]bool{}
			}
			effectFam[te.typeID][fam] = true
		}
	}
	squadron := map[int64]bool{}
	structureFighter := map[int64]bool{}
	droneBW := map[int64]bool{}
	for _, a := range parsed.typeAttrs {
		if a.value <= 0 {
			continue
		}
		switch a.attributeID {
		case fit.AttrFighterSquadronMaxSize:
			squadron[a.typeID] = true
		case 2740, 2741, 2742: // structure fighter slots
			structureFighter[a.typeID] = true
		case fit.AttrDroneBandwidthUsed:
			droneBW[a.typeID] = true
		}
	}
	groupCat := make(map[int64]int64, len(parsed.groups))
	for _, g := range parsed.groups {
		groupCat[g.groupID] = g.categoryID
	}
	familyOf := func(t sdeTypeRow) string {
		if groupCat[t.groupID] == sdeCategoryStructureModule {
			return ""
		}
		fams := effectFam[t.typeID]
		switch {
		case fams[fitview.FitFamilyRig]:
			return fitview.FitFamilyRig
		case fams[fitview.FitFamilySubsystem]:
			return fitview.FitFamilySubsystem
		case squadron[t.typeID]:
			if structureFighter[t.typeID] {
				return ""
			}
			return fitview.FitFamilyFighter
		case droneBW[t.typeID]:
			return fitview.FitFamilyDrone
		case fams[fitview.FitFamilyHigh]:
			return fitview.FitFamilyHigh
		case fams[fitview.FitFamilyMedium]:
			return fitview.FitFamilyMedium
		case fams[fitview.FitFamilyLow]:
			return fitview.FitFamilyLow
		case groupCat[t.groupID] == sdeCategoryCharge:
			return "charge"
		}
		return ""
	}

	mg := make(map[int64]sdeMarketGroupRow, len(parsed.marketGroups))
	for _, g := range parsed.marketGroups {
		mg[g.marketGroupID] = g
	}
	var types []fitBrowseType
	counts := map[string]map[int64]int{} // family -> group -> types under it
	for _, t := range parsed.types {
		if t.published == 0 || t.marketGroupID == 0 {
			continue
		}
		if _, ok := mg[t.marketGroupID]; !ok {
			continue
		}
		fam := familyOf(t)
		if fam == "" {
			continue
		}
		types = append(types, fitBrowseType{family: fam, typeID: t.typeID, marketGroupID: t.marketGroupID, name: t.name})
		if counts[fam] == nil {
			counts[fam] = map[int64]int{}
		}
		// Walk to the top; the guard stops a malformed parent loop.
		for id, hops := t.marketGroupID, 0; id != 0 && hops < 16; hops++ {
			g, ok := mg[id]
			if !ok {
				break
			}
			counts[fam][id]++
			id = g.parentGroupID
		}
	}

	var groups []fitBrowseGroup
	for _, fam := range fitBrowseFamilies {
		c := counts[fam]
		if len(c) == 0 {
			continue
		}
		direct := map[int64]bool{}
		for _, t := range types {
			if t.family == fam {
				direct[t.marketGroupID] = true
			}
		}
		children := map[int64][]int64{}
		for id := range c {
			p := mg[id].parentGroupID
			if _, in := c[p]; !in {
				p = 0
			}
			children[p] = append(children[p], id)
		}
		// Roots without types of their own give way to their
		// children, then single-child tops do the same.
		var tops []int64
		for _, id := range children[0] {
			if direct[id] || len(children[id]) == 0 {
				tops = append(tops, id)
				continue
			}
			tops = append(tops, children[id]...)
		}
		for len(tops) == 1 && !direct[tops[0]] && len(children[tops[0]]) > 0 {
			tops = children[tops[0]]
		}
		children[-1] = tops
		var walk func(parent, as int64)
		walk = func(parent, as int64) {
			kids := children[parent]
			sort.Slice(kids, func(i, j int) bool { return mg[kids[i]].name < mg[kids[j]].name })
			for _, id := range kids {
				groups = append(groups, fitBrowseGroup{family: fam, marketGroupID: id, parentGroupID: as, name: mg[id].name, typeCount: c[id]})
				walk(id, id)
			}
		}
		walk(-1, 0)
	}
	return groups, types
}
