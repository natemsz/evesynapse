package fitview

import (
	"context"

	"evesynapse/internal/fit"
	"evesynapse/internal/humanize"
)

// FitPickStat is what a search or picker row says about a type
// before it is fitted: its fitting cost, one headline figure, its
// meta tier and its price.
type FitPickStat struct {
	CPU         float64 `json:"cpu,omitempty"`
	PG          float64 `json:"pg,omitempty"`
	Calibration float64 `json:"cal,omitempty"`
	Stat        string  `json:"stat,omitempty"`    // "Optimal 24 km"
	StatKind    string  `json:"statk,omitempty"`   // the stat's glyph kind (FitGlyphKind)
	Tier        string  `json:"tier,omitempty"`    // "" for Tech I
	Price       string  `json:"price,omitempty"`   // compact ISK, "" when unpriced
	Estimate    bool    `json:"est,omitempty"`     // price is a stored estimate, not a swept sell
	Only        string  `json:"only,omitempty"`    // the hulls it is limited to, when the current one is not among them
	SubSlot     int64   `json:"subslot,omitempty"` // a subsystem's slot kind (one per kind on a hull)
}

// FitPickStats reads the stats of a bounded list of types (one
// result page of the module search) in one snapshot load. The
// headline is the first row the fitted tooltip would show, read
// off the type's own attributes (no pilot, no hull). Prices follow
// the fit total's order (FitUnitPrice). Given a hull, a row the hull
// cannot take carries the hulls it is limited to (Only).
func (app *Builder) FitPickStats(ctx context.Context, typeIDs []int64, shipTypeID int64) map[int64]FitPickStat {
	out := make(map[int64]FitPickStat, len(typeIDs))
	if len(typeIDs) == 0 {
		return out
	}
	load := typeIDs
	if shipTypeID > 0 {
		load = append(append(make([]int64, 0, len(typeIDs)+1), typeIDs...), shipTypeID)
	}
	snap, err := fit.LoadSnapshot(ctx, app.queries, load)
	if err != nil || snap == nil {
		return out
	}
	tiers := map[int64]int64{}
	if rows, err := app.queries.ListSDEMetaGroupsByTypes(ctx, typeIDs); err == nil {
		for _, r := range rows {
			tiers[r.TypeID] = r.MetaGroupID
		}
	}
	typicals := app.env.TypicalSells(ctx)
	trueValues := app.env.TrueValues(ctx)
	doc := &FitDoc{}
	empty := &fit.Result{}
	for _, id := range typeIDs {
		attrs := snap.Attrs[id]
		if attrs == nil {
			continue
		}
		st := FitPickStat{
			CPU:         attrs[fit.AttrCPU],
			PG:          attrs[fit.AttrPower],
			Calibration: attrs[fit.AttrUpgradeCost],
			SubSlot:     int64(attrs[fit.AttrSubSystemSlot]),
		}
		for _, l := range FitTipLines(empty, snap, doc, id, "", attrs) {
			if l.Label != "CPU" && l.Label != "PG" { // the row states those itself
				st.Stat = l.Label + " " + l.Value
				st.StatKind = l.Kind
				break
			}
		}
		if name := FitVisualMetaName(int(tiers[id])); name != "Tech I" {
			st.Tier = name
		}
		if v, ok := typicals[id]; ok && v > 0 {
			st.Price = humanize.ISKCompact(v)
		} else if v, ok := trueValues[id]; ok && v > 0 {
			st.Price, st.Estimate = humanize.ISKCompact(v), true
		}
		if shipTypeID > 0 {
			if r := fit.RestrictionFor(snap, shipTypeID, id); r != nil {
				st.Only = app.FitRestrictionLabel(ctx, *r)
			}
		}
		out[id] = st
	}
	return out
}
