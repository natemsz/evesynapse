package app

import (
	"math"
	"sort"

	"evesynapse/internal/itemsheet"
)

// sdeVariationColumn is one column of a variation family's compare
// table (schema 141).
type sdeVariationColumn struct {
	rootTypeID  int64
	ord         int16
	attributeID int64
}

// sdeVariationRow is one family member with its column values, NaN
// where it has none.
type sdeVariationRow struct {
	typeID      int64
	rootTypeID  int64
	metaGroupID int64
	metaLevel   float64
	vals        []float64
	tier        string // a ladder step's label (Level 3, Mid-grade); "" reads the meta group
}

// buildVariations groups the dump's published types into variation
// families (a base type and the types naming it as their base, plus
// the implant and booster ladders of ladderFamilies) and
// picks each family's compare columns, so the item page reads one
// family's table by key instead of every member's attributes.
// Families of one type get no rows.
func buildVariations(parsed *parsedSDE) ([]sdeVariationColumn, []sdeVariationRow) {
	published := make(map[int64]bool, len(parsed.types))
	for _, t := range parsed.types {
		if t.published != 0 {
			published[t.typeID] = true
		}
	}
	families := map[int64][]int64{}
	metaGroup := map[int64]int64{}
	for _, m := range parsed.metaTypes {
		if !published[m.typeID] {
			continue
		}
		root := m.parentTypeID
		if root == 0 {
			root = m.typeID
		}
		if !published[root] {
			continue
		}
		metaGroup[m.typeID] = m.metaGroupID
		families[root] = append(families[root], m.typeID)
	}
	member := map[int64]bool{}
	for root, ms := range families {
		// A base type is in invMetaTypes as its own tier only
		// sometimes; it always belongs to its family.
		found := false
		for _, id := range ms {
			if id == root {
				found = true
				break
			}
		}
		if !found {
			ms = append(ms, root)
			families[root] = ms
		}
		if len(ms) < 2 {
			delete(families, root)
			continue
		}
		for _, id := range ms {
			member[id] = true
		}
	}
	ladders, steps := ladderFamilies(parsed, published, member)
	for root, ms := range ladders {
		families[root] = ms
		for _, id := range ms {
			member[id] = true
		}
	}
	if len(families) == 0 {
		return nil, nil
	}

	info := make(map[int64]itemsheet.AttrInfo, len(parsed.attrTypes))
	for _, a := range parsed.attrTypes {
		info[a.attributeID] = itemsheet.AttrInfo{CategoryID: a.categoryID, Shown: a.published != 0 && a.displayName != ""}
	}
	values := make(map[int64]map[int64]float64, len(member))
	for _, ta := range parsed.typeAttrs {
		if !member[ta.typeID] {
			continue
		}
		m := values[ta.typeID]
		if m == nil {
			m = map[int64]float64{}
			values[ta.typeID] = m
		}
		m[ta.attributeID] = ta.value
	}

	roots := make([]int64, 0, len(families))
	for root := range families {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })

	var cols []sdeVariationColumn
	var rows []sdeVariationRow
	for _, root := range roots {
		ms := families[root]
		sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
		attrs := itemsheet.VariationColumns(ms, values, info)
		for i, id := range attrs {
			cols = append(cols, sdeVariationColumn{rootTypeID: root, ord: int16(i), attributeID: id})
		}
		for _, id := range ms {
			vals := make([]float64, len(attrs))
			for i, a := range attrs {
				v, ok := values[id][a]
				if !ok {
					v = math.NaN()
				}
				vals[i] = v
			}
			mg := metaGroup[id]
			if mg == 0 {
				mg = 1 // a base type with no tier row is Tech I
			}
			row := sdeVariationRow{typeID: id, rootTypeID: root, metaGroupID: mg, metaLevel: values[id][itemsheet.AttrMetaLevel], vals: vals}
			if st, ok := steps[id]; ok {
				// A ladder reads in strength order under its own labels.
				row.metaLevel, row.tier = st.rank, st.tier
			}
			rows = append(rows, row)
		}
	}
	return cols, rows
}
