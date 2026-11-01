package app

import (
	"context"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// The Production Lines widget: the account's pinned blueprints
// (blueprint_pins, schema 118, set from the Blueprints page) and
// the state of each line, judged from the per-character industry
// job snapshots the home bundle already loads. A line is due when
// one of its jobs is ready, or a manufacturing job on it finishes
// within a day; it needs a restart when no job on it is running;
// otherwise it is healthy. The buy list is one run of materials
// for every due and idle line (the stored SDE recipe), less what
// the stored asset snapshots already hold across the account.
// How much shows follows the widget's own stored size: the
// smallest sizes list only the lines needing attention, the
// default size adds every line and the buy list, the largest adds
// each attention line's own missing materials and which character
// is running a line.
// ---------------------------------------------------------------------------

// prodLineDueWithin is how close a running manufacturing job has
// to be to finishing for its line to count as due rather than
// healthy: a day, the same horizon the briefing uses.
const prodLineDueWithin = 24 * time.Hour

// prodLine states.
const (
	prodLineDue     = "due"
	prodLineRestart = "restart"
	prodLineHealthy = "healthy"
)

type prodLineBuyRow struct {
	Name   string
	TypeID int64
	Qty    string
}

type prodLineRow struct {
	Product       string
	ProductTypeID int64 // 0 when the SDE maps no product
	State         string
	Note          string // "Ready for delivery" / "Delivers in 5h" / "Needs a restart" / "healthy"
	Runner        string // largest size: who runs the line's next job, and when it ends
	Missing       []prodLineBuyRow
}

type prodLineWidget struct {
	HasPins   bool
	JobsKnown bool // at least one character's job snapshot is loaded
	Lines     []prodLineRow
	Attention int // due + needs-restart lines
	QuietNote string
	Buy       []prodLineBuyRow
	ShowBuy   bool
}

// buildProdLine shapes the widget from the account's pins, the
// bundles' job and asset snapshots, and the stored SDE product
// and recipe maps. gw and gh are the widget's stored grid size.
func (app *Application) buildProdLine(ctx context.Context, userID int64, bundles []*charSnaps, gw, gh int) *prodLineWidget {
	out := &prodLineWidget{}
	area := gw * gh
	out.ShowBuy = area >= 4
	detailed := area >= 6

	pinRows, err := app.queries.ListBlueprintPins(ctx, userID)
	if err != nil || len(pinRows) == 0 {
		return out
	}
	out.HasPins = true

	for _, b := range bundles {
		if b.jobsKnown {
			out.JobsKnown = true
			break
		}
	}
	if !out.JobsKnown {
		return out
	}

	// Products for the pinned blueprints.
	products := map[int64]int64{}
	if rows, err := app.queries.ListSDEBlueprintProducts(ctx); err == nil {
		for _, r := range rows {
			products[r.BlueprintTypeID] = r.ProductTypeID
		}
	}

	// Judge every pin against every character's jobs.
	now := time.Now()
	type judgement struct {
		bp  int64
		row prodLineRow
		end time.Time // soonest due end, for ordering due lines
	}
	judged := make([]judgement, 0, len(pinRows))
	var attentionBps []int64
	for _, pin := range pinRows {
		bp := pin.BlueprintTypeID
		var dueEnd time.Time
		dueReady, running := false, false
		runnerName, runnerEnds := "", ""
		var runnerEnd time.Time
		for _, b := range bundles {
			if !b.jobsKnown {
				continue
			}
			for _, j := range b.jobs {
				if j.BlueprintTypeID != bp {
					continue
				}
				end, _ := parseRFC3339(j.EndDate)
				switch j.Status {
				case "ready":
					dueReady = true
					running = true
				case "active":
					running = true
					if (j.ActivityID == 1 || j.ActivityID == 11) && !end.IsZero() &&
						end.Sub(now) <= prodLineDueWithin {
						if dueEnd.IsZero() || end.Before(dueEnd) {
							dueEnd = end
						}
					}
					if !end.IsZero() && (runnerEnd.IsZero() || end.Before(runnerEnd)) {
						runnerEnd, runnerName, runnerEnds = end, b.ch.Name, j.EndDate
					}
				case "paused":
					running = true
					if runnerEnd.IsZero() {
						runnerEnd, runnerName, runnerEnds = end, b.ch.Name, j.EndDate
					}
				}
			}
		}
		row := prodLineRow{ProductTypeID: products[bp]}
		switch {
		case dueReady:
			row.State, row.Note = prodLineDue, "Ready for delivery"
		case !dueEnd.IsZero():
			row.State = prodLineDue
			if dueEnd.After(now) {
				row.Note = "Delivers in " + humanDuration(time.Until(dueEnd).Round(time.Minute))
			} else {
				row.Note = "Ready for delivery"
			}
		case running:
			row.State, row.Note = prodLineHealthy, "healthy"
		default:
			row.State, row.Note = prodLineRestart, "Needs a restart"
		}
		if row.State != prodLineHealthy {
			out.Attention++
			attentionBps = append(attentionBps, bp)
		}
		if detailed && runnerName != "" {
			row.Runner = runnerName + " · ends " + formatFinish(runnerEnds)
		}
		judged = append(judged, judgement{bp: bp, row: row, end: dueEnd})
	}

	// Names for every product (blueprint name as the fallback) in
	// one batched lookup.
	nameIDs := make([]int64, 0, 2*len(pinRows))
	for _, pin := range pinRows {
		nameIDs = append(nameIDs, pin.BlueprintTypeID)
		if pid := products[pin.BlueprintTypeID]; pid != 0 {
			nameIDs = append(nameIDs, pid)
		}
	}
	names := app.esi.CachedTypeNames(ctx, nameIDs)
	for i := range judged {
		name := names[products[judged[i].bp]]
		if name == "" {
			name = names[judged[i].bp]
		}
		if name == "" {
			name = "Blueprint " + esi.FormatInt(judged[i].bp)
		}
		judged[i].row.Product = name
	}

	// Attention first: due lines by soonest delivery, then lines
	// needing a restart, then healthy lines, each by product name.
	rank := func(s string) int {
		switch s {
		case prodLineDue:
			return 0
		case prodLineRestart:
			return 1
		}
		return 2
	}
	sort.SliceStable(judged, func(i, j int) bool {
		ri, rj := rank(judged[i].row.State), rank(judged[j].row.State)
		if ri != rj {
			return ri < rj
		}
		if judged[i].row.State == prodLineDue && !judged[i].end.Equal(judged[j].end) {
			return judged[i].end.Before(judged[j].end)
		}
		return judged[i].row.Product < judged[j].row.Product
	})

	// The buy list: one run of each attention line's recipe, less
	// what the stored asset snapshots hold across the account (the
	// build planner's netting). Where no asset snapshot is loaded
	// the full need is listed; a partial picture only ever nets
	// what it can see.
	if out.ShowBuy && len(attentionBps) > 0 {
		stock := map[int64]int64{}
		for _, b := range bundles {
			if !b.assetsKnown {
				continue
			}
			for _, a := range b.assets {
				if a.Quantity > 0 {
					stock[a.TypeID] += a.Quantity
				}
			}
		}
		if mats, err := app.queries.ListSDEBlueprintMaterialsForBlueprints(ctx, attentionBps); err == nil {
			need := map[int64]int64{}
			perLine := map[int64]map[int64]int64{}
			for _, m := range mats {
				need[m.MaterialTypeID] += m.Quantity
				if detailed {
					if perLine[m.BlueprintTypeID] == nil {
						perLine[m.BlueprintTypeID] = map[int64]int64{}
					}
					perLine[m.BlueprintTypeID][m.MaterialTypeID] += m.Quantity
				}
			}
			matIDs := make([]int64, 0, len(need))
			for id := range need {
				matIDs = append(matIDs, id)
			}
			matNames := app.esi.CachedTypeNames(ctx, matIDs)
			buyRows := func(lineNeed map[int64]int64) []prodLineBuyRow {
				ids := make([]int64, 0, len(lineNeed))
				for id := range lineNeed {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return matNames[ids[i]] < matNames[ids[j]] })
				rows := make([]prodLineBuyRow, 0, len(ids))
				for _, id := range ids {
					if left := lineNeed[id] - stock[id]; left > 0 {
						name := matNames[id]
						if name == "" {
							name = "Item " + esi.FormatInt(id)
						}
						rows = append(rows, prodLineBuyRow{Name: name, TypeID: id, Qty: esi.FormatInt(left)})
					}
				}
				return rows
			}
			out.Buy = buyRows(need)
			if detailed {
				for i := range judged {
					if lineNeed := perLine[judged[i].bp]; lineNeed != nil {
						judged[i].row.Missing = buyRows(lineNeed)
					}
				}
			}
		}
	}

	for _, j := range judged {
		out.Lines = append(out.Lines, j.row)
	}
	if area < 4 {
		lines := out.Lines[:0]
		for _, l := range out.Lines {
			if l.State != prodLineHealthy {
				lines = append(lines, l)
			}
		}
		out.Lines = lines
		if out.Attention == 0 {
			out.QuietNote = "All pinned lines are running."
		}
	}
	return out
}
