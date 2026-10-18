package fitting

import (
	"fmt"
	"net/http"
	"strings"

	"evesynapse/internal/fit"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// handleFitSolveImplants serves POST /fittings/solve-implants/:
// the posted fit is over its CPU or powergrid budget, so search
// the hardwiring ladder for the smallest implants that make it
// legal and answer with the picks. Nothing is applied here — the
// editor applies a pick by writing it into the document's
// implant overrides (the ordinary simulate path), which is a
// simulation overlay: the pilot's real clone is never touched.
func (app *tool) handleFitSolveImplants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := readFitRequest(w, r)
	if !ok {
		return
	}
	if req.Pilot != 0 {
		if _, owned := app.fitPilotLabel(ctx, app.deps.UserID(ctx), req.Pilot); !owned {
			writeFitJSON(w, map[string]any{"ok": false, "error": "That pilot isn't one of your characters."})
			return
		}
	}
	doc := &req.FitDoc
	if doc.ShipTypeID <= 0 {
		writeFitJSON(w, map[string]any{"ok": false, "error": "Pick a ship first."})
		return
	}

	rows, err := app.queries.ListFitResourceImplants(ctx)
	if err != nil {
		logging.Errorf("fittings solve: resource implants: %v", err)
		writeFitJSON(w, map[string]any{"ok": false, "error": "Couldn't read the implant data."})
		return
	}
	implantIDs, _, _, _ := app.fv.FitPilotImplants(ctx, req.Pilot, req.Clone)
	typeIDs := fitview.FitDocTypeIDs(doc)
	typeIDs = append(typeIDs, implantIDs...)
	for _, id := range doc.Implants {
		if id > 0 {
			typeIDs = append(typeIDs, id)
		}
	}
	candNames := make(map[int64]string, len(rows))
	for _, row := range rows {
		typeIDs = append(typeIDs, row.TypeID)
		candNames[row.TypeID] = row.Name
	}
	snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs)
	if err != nil {
		logging.Errorf("fittings solve: load snapshot for ship %d: %v", doc.ShipTypeID, err)
		writeFitJSON(w, map[string]any{"ok": false, "error": "Something went wrong reading the ship data."})
		return
	}
	if len(snap.Attrs[doc.ShipTypeID]) == 0 {
		writeFitJSON(w, map[string]any{"ok": false, "error": "The ship and module data is still downloading — give it a little while and try again."})
		return
	}
	// Bound the document's fitted quantities before the
	// solver expands it per instance and re-prices trials.
	fitview.SanitizeFitDoc(doc, snap)

	// The fit's effective implant set (clone plus the document's
	// overrides) and each implant's slot, the solver's base.
	slots := fitview.FitResolveImplantSlots(snap, implantIDs, doc.Implants)
	base := make([]int64, 0, len(slots))
	baseSlots := make(map[int]int64, len(slots))
	for _, s := range slots {
		if s.TypeID > 0 {
			base = append(base, s.TypeID)
			baseSlots[s.Slot] = s.TypeID
		}
	}
	levels, _ := app.fv.FitPilotLevels(ctx, req.Pilot, snap, doc)
	engineItems := solveEngineItems(snap, doc)
	compute := func(implants []int64) *fit.Result {
		return fit.Compute(snap, doc.ShipTypeID, engineItems, levels, doc.Charges, implants)
	}
	baseRes := compute(base)
	overCPU := baseRes.CPUUsed > baseRes.CPUMax+1e-6
	overPG := baseRes.PowergridUsed > baseRes.PowergridMax+1e-6
	if !overCPU && !overPG {
		writeFitJSON(w, map[string]any{"ok": true, "solved": false,
			"note": "This fit already fits — no implant needed."})
		return
	}

	typicals := app.deps.TypicalSells(ctx)
	trueValues := app.deps.TrueValues(ctx)
	candidates := make([]fit.ResourceImplant, 0, len(rows))
	strongest := make(map[int64]float64, 2)
	ladderSlots := make(map[int64]map[int]bool, 2)
	for _, row := range rows {
		if row.Bonus <= 0 {
			continue
		}
		price, _ := fitview.FitUnitPrice(typicals, trueValues, row.TypeID)
		candidates = append(candidates, fit.ResourceImplant{
			TypeID: row.TypeID, Slot: int(row.Slot), Resource: row.ResourceAttr,
			Bonus: row.Bonus, Price: price,
		})
		if row.Bonus > strongest[row.ResourceAttr] {
			strongest[row.ResourceAttr] = row.Bonus
		}
		if ladderSlots[row.ResourceAttr] == nil {
			ladderSlots[row.ResourceAttr] = make(map[int]bool)
		}
		ladderSlots[row.ResourceAttr][int(row.Slot)] = true
	}

	picks, unsolved := fit.SolveImplants(compute, base, baseSlots, candidates)
	if len(unsolved) > 0 {
		writeFitJSON(w, map[string]any{"ok": true, "solved": false,
			"note": solveFailureNote(baseRes, unsolved, strongest, ladderSlots)})
		return
	}
	out := make([]map[string]any, 0, len(picks))
	for _, p := range picks {
		price, known := fitview.FitUnitPrice(typicals, trueValues, p.TypeID)
		pick := map[string]any{
			"slot": p.Slot, "typeId": p.TypeID, "name": candNames[p.TypeID],
			"resource": solveResourceName(p.Resource), "bonus": p.Bonus,
			"price": price, "priceKnown": known,
		}
		if held, ok := baseSlots[p.Slot]; ok && held != p.TypeID {
			pick["replacesId"] = held
			pick["replacesName"] = app.deps.TypeName(ctx, held)
		}
		out = append(out, pick)
	}
	writeFitJSON(w, map[string]any{"ok": true, "solved": true, "picks": out})
}

// solveEngineItems expands a document into engine inputs exactly
// as BuildFitSimView does (fitted families only, one input per
// module state) so the solver's trials price the same fit the
// workbench renders.
func solveEngineItems(snap *fit.Snapshot, doc *fitview.FitDoc) []fit.ItemInput {
	engineItems := make([]fit.ItemInput, 0, len(doc.Items))
	for _, it := range doc.Items {
		fam := fitview.FitSlotFamilyOf(snap, it.TypeID)
		if !fitview.FitFamilyFitted(fam) {
			continue
		}
		byState := make(map[string]int, 2)
		var order []string
		for i := 0; i < it.Qty; i++ {
			st := ""
			if i < len(it.States) {
				st = it.States[i]
			}
			if _, ok := byState[st]; !ok {
				order = append(order, st)
			}
			byState[st]++
		}
		for _, st := range order {
			engineItems = append(engineItems, fit.ItemInput{TypeID: it.TypeID, Quantity: byState[st], State: st})
		}
	}
	return engineItems
}

// solveResourceName is the plain-language resource label.
func solveResourceName(resource int64) string {
	if resource == fit.ResourceCPU {
		return "CPU"
	}
	return "Powergrid"
}

// solveFailureNote explains, per unsolved resource, how far over
// the fit sits and what the strongest hardwiring adds — or that
// the two ladders collide in a single slot.
func solveFailureNote(baseRes *fit.Result, unsolved []int64, strongest map[int64]float64, ladderSlots map[int64]map[int]bool) string {
	if len(unsolved) == 2 {
		cpuSlots, pgSlots := ladderSlots[fit.ResourceCPU], ladderSlots[fit.ResourcePG]
		if len(cpuSlots) == 1 && len(pgSlots) == 1 {
			for s := range cpuSlots {
				if pgSlots[s] {
					return fmt.Sprintf("No implant makes this fit legal: CPU and powergrid hardwirings both plug into slot %d, and one slot can't hold both fixes.", s)
				}
			}
		}
	}
	parts := make([]string, 0, len(unsolved))
	for _, resource := range unsolved {
		switch resource {
		case fit.ResourceCPU:
			part := fmt.Sprintf("CPU is over by %s tf", fitview.FitFmt1(baseRes.CPUUsed-baseRes.CPUMax))
			if best := strongest[resource]; best > 0 {
				part += fmt.Sprintf(" and the strongest CPU hardwiring adds %s%%", fitview.FitFmt1(best))
			}
			parts = append(parts, part)
		case fit.ResourcePG:
			part := fmt.Sprintf("powergrid is over by %s MW", fitview.FitFmt1(baseRes.PowergridUsed-baseRes.PowergridMax))
			if best := strongest[resource]; best > 0 {
				part += fmt.Sprintf(" and the strongest powergrid hardwiring adds %s%%", fitview.FitFmt1(best))
			}
			parts = append(parts, part)
		}
	}
	return "No implant makes this fit legal: " + strings.Join(parts, "; ") + "."
}
