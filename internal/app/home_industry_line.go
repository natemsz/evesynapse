package app

import (
	"context"
	"sort"
	"strconv"
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
	Name        string
	TypeID      int64
	Qty         string
	Art         string // the type's render on the EVE image server
	ArtFallback string // the type's icon, swapped in when no render exists
}

type prodLineRow struct {
	Product       string
	ProductTypeID int64 // 0 when the SDE maps no product
	State         string
	Note          string // "Ready for delivery" / "Delivers in 5h" / "Needs a restart" / "healthy"
	Chip          string // the state as a chip beside the product name: "Due" / "Needs a restart" / "healthy"
	Art           string // the product's render on the EVE image server
	ArtFallback   string // the product's icon, swapped in when no render exists
	HasBar        bool   // the line's job has both a start and an end to measure against
	BarPct        int    // the job's elapsed share of its start-to-end span, 0 to 100
	BarLabel      string // the timing line under the bar: the note for a due line, "Ends in ..." for a healthy one
	Runner        string // largest size: who runs the line's next job, and when it ends
	Missing       []prodLineBuyRow
}

// typeArtURLs follows the item page's art pattern: the type's
// render on the EVE image server first, its icon as the fallback
// the templates swap in when the type has no render.
func typeArtURLs(typeID int64, size int) (string, string) {
	id := strconv.FormatInt(typeID, 10)
	sz := strconv.Itoa(size)
	return "https://images.evetech.net/types/" + id + "/render?size=" + sz,
		"https://images.evetech.net/types/" + id + "/icon?size=" + sz
}

// prodLineBar fills the row's progress bar from a job's stored
// start and end: the share of the span already elapsed by now.
// A job missing either stamp, or with an end not after its
// start, leaves the row without a bar.
func prodLineBar(row *prodLineRow, startRaw, endRaw string, now time.Time) {
	start, okS := parseRFC3339(startRaw)
	end, okE := parseRFC3339(endRaw)
	if !okS || !okE || !end.After(start) {
		return
	}
	pct := int(now.Sub(start) * 100 / end.Sub(start))
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	row.HasBar, row.BarPct = true, pct
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
		dueStart, readyStart, readyEnd := "", "", ""
		runnerName, runnerEnds, runnerStart := "", "", ""
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
					if readyStart == "" {
						readyStart, readyEnd = j.StartDate, j.EndDate
					}
				case "active":
					running = true
					if (j.ActivityID == 1 || j.ActivityID == 11) && !end.IsZero() &&
						end.Sub(now) <= prodLineDueWithin {
						if dueEnd.IsZero() || end.Before(dueEnd) {
							dueEnd, dueStart = end, j.StartDate
						}
					}
					if !end.IsZero() && (runnerEnd.IsZero() || end.Before(runnerEnd)) {
						runnerEnd, runnerName, runnerEnds, runnerStart = end, b.ch.Name, j.EndDate, j.StartDate
					}
				case "paused":
					running = true
					if runnerEnd.IsZero() {
						runnerEnd, runnerName, runnerEnds, runnerStart = end, b.ch.Name, j.EndDate, j.StartDate
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
		// The face: the state as a chip, and a progress bar
		// measured on the job that set the state, from its
		// stored start and end. A due line's bar carries its
		// note as the timing label; a healthy line's bar is
		// labelled with the time left on its next job.
		switch row.State {
		case prodLineDue:
			row.Chip = "Due"
			row.BarLabel = row.Note
			if dueReady {
				prodLineBar(&row, readyStart, readyEnd, now)
			} else {
				prodLineBar(&row, dueStart, dueEnd.Format(time.RFC3339), now)
			}
		case prodLineHealthy:
			row.Chip = "healthy"
			prodLineBar(&row, runnerStart, runnerEnds, now)
			if row.HasBar {
				row.BarLabel = "Ends in " + humanDuration(time.Until(runnerEnd).Round(time.Minute))
			}
		default:
			row.Chip = row.Note
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
		if judged[i].row.ProductTypeID != 0 {
			judged[i].row.Art, judged[i].row.ArtFallback = typeArtURLs(judged[i].row.ProductTypeID, 64)
		}
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
						art, artFallback := typeArtURLs(id, 64)
						rows = append(rows, prodLineBuyRow{Name: name, TypeID: id, Qty: esi.FormatInt(left), Art: art, ArtFallback: artFallback})
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
