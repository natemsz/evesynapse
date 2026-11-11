package itemsheet

import "sort"

// Attribute ids the variation table treats specially.
const (
	attrCPU        = 50
	attrPowergrid  = 30
	attrTechLevel  = 422
	AttrMetaLevel  = 633 // the game's variation order
	attrMetaGroup  = 1692
	maxVariantCols = 4
)

// AttrInfo is what the variation column picker knows about one
// attribute.
type AttrInfo struct {
	CategoryID int64
	Shown      bool // published, with a display name
}

// headlineOrder ranks sections for the variation table: what a module
// does comes before what it costs to run and fit.
var headlineOrder = map[string]int{
	"Weapon": 0, "Shield": 1, "Armor": 2, "Structure": 3, "Navigation": 4,
	"Targeting": 5, "Drones": 6, "Miscellaneous": 7, "Capacitor": 8, "Fitting": 9,
}

// VariationColumns picks the attributes a variation family's compare
// table shows: CPU and powergrid when any member uses them, then up to
// four more shown attributes that at least two members carry with
// different values, the ones saying what the module does first, and
// never two that read the same for every member.
// values maps member type id to attribute id to value.
func VariationColumns(members []int64, values map[int64]map[int64]float64, info map[int64]AttrInfo) []int64 {
	type cand struct {
		id   int64
		rank int
	}
	seen := map[int64][]float64{}
	for _, m := range members {
		for id, v := range values[m] {
			seen[id] = append(seen[id], v)
		}
	}
	var cols []int64
	for _, id := range []int64{attrCPU, attrPowergrid} {
		if vs := seen[id]; len(vs) > 0 && anyNonZero(vs) {
			cols = append(cols, id)
		}
	}
	var cands []cand
	for id, vs := range seen {
		if id == attrCPU || id == attrPowergrid || id == attrTechLevel || id == AttrMetaLevel || id == attrMetaGroup {
			continue
		}
		in, ok := info[id]
		if !ok || !in.Shown || len(vs) < 2 || !differ(vs) {
			continue
		}
		section, shown := sectionOf(in.CategoryID, id)
		if !shown {
			continue
		}
		cands = append(cands, cand{id: id, rank: headlineOrder[section]})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].id < cands[j].id
	})
	// A column that reads the same as one already chosen, member for
	// member (a damage control's four shield resistances), adds
	// nothing: the next attribute gets its place.
	var picked [][]float64
	vector := func(id int64) []float64 {
		vs := make([]float64, len(members))
		for i, m := range members {
			v, ok := values[m][id]
			if !ok {
				v = -1
			}
			vs[i] = v
		}
		return vs
	}
	added := 0
	for _, c := range cands {
		if added == maxVariantCols {
			break
		}
		vec := vector(c.id)
		dup := false
		for _, p := range picked {
			if equalFloats(p, vec) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		picked = append(picked, vec)
		cols = append(cols, c.id)
		added++
	}
	return cols
}

func equalFloats(a, b []float64) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

func anyNonZero(vs []float64) bool {
	for _, v := range vs {
		if v != 0 {
			return true
		}
	}
	return false
}

func differ(vs []float64) bool {
	for _, v := range vs[1:] {
		if v != vs[0] {
			return true
		}
	}
	return false
}
