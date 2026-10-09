package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Industry page: industry jobs (active plus the last 90 days of
// completed, via include_completed), blueprint library, and the
// recent mining ledger. Sections degrade independently and render
// from snapshots only.
// ---------------------------------------------------------------------------

// industryActivityNames labels ESI industry activity IDs (the RAM
// activity table). Unknown IDs render as "Activity #<id>".
var industryActivityNames = map[int64]string{
	1:  "Manufacturing",
	2:  "Researching Technology",
	3:  "Time Efficiency Research",
	4:  "Material Efficiency Research",
	5:  "Copying",
	6:  "Duplication",
	7:  "Reverse Engineering",
	8:  "Invention",
	9:  "None",
	11: "Reactions",
}

func industryActivityLabel(id int64) string {
	if name, ok := industryActivityNames[id]; ok {
		return name
	}
	return fmt.Sprintf("Activity #%d", id)
}

// industryJobRow is one job line.
type industryJobRow struct {
	Activity        string
	Blueprint       string // blueprint type name
	BlueprintTypeID int64
	Product         string // product type name (invention/manufacturing output), "" when n/a
	ProductTypeID   int64
	Installer       string
	InstallerID     int64
	Facility        placeRef
	Runs            string // "5" or "3 / 5 successful" for finished invention-type runs
	Status          string // humanized
	Ends            string // end date + remaining time while active
	Cost            string
}

// blueprintRow is one blueprint-library line.
type blueprintRow struct {
	Name            string
	TypeID          int64
	Kind            string // "BPO" | "BPC"
	ME              string // "10%"
	TE              string // "20%"
	Runs            string // runs remaining; "∞" for originals
	Location        placeRef
	ProductName     string // what the blueprint produces
	ProductCategory string // category of the produced item
}

// miningRow is one mining-ledger line.
type miningRow struct {
	Date   string
	Ore    string
	TypeID int64
	Qty    string
	System placeRef
}

// industryView is the Industry page body.
type industryView struct {
	CharacterName string
	Jobs          econSectionState
	JobRows       []industryJobRow
	JobsCut       int
	Blueprints    econSectionState
	BlueprintRows []blueprintRow
	BlueprintsCut int
	Mining        econSectionState
	MiningRows    []miningRow
	MiningCut     int
}

// maxIndustryRows caps each section table (corp-ledger precedent).
const maxIndustryRows = 100

func (app *Application) handleIndustry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/industry/")
	if err != nil {
		logging.Errorf("industry: list characters: %v", err)
		data.Error = "Could not load industry data; check the server log."
		app.render(ctx, w, http.StatusOK, "industry.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "industry.html", data)
		return
	}
	data.IndustryChars = links

	view := &industryView{CharacterName: active.Name}
	data.Industry = view

	app.fillIndustryJobs(ctx, active.CharacterID, view)
	app.fillBlueprints(ctx, active.CharacterID, view)
	app.fillMining(ctx, active.CharacterID, view)

	app.render(ctx, w, http.StatusOK, "industry.html", data)
}

func (app *Application) fillIndustryJobs(ctx context.Context, characterID int64, view *industryView) {
	var jobs esi.IndustryJobs
	view.Jobs = app.econSection(ctx, characterID, esi.SnapIndustryJobs, &jobs)
	if !view.Jobs.Loaded {
		return
	}
	rows := make([]industryJobRow, 0, len(jobs))
	for _, j := range jobs {
		row := industryJobRow{
			Activity:        industryActivityLabel(j.ActivityID),
			Blueprint:       app.typeNameOrID(ctx, j.BlueprintTypeID),
			BlueprintTypeID: j.BlueprintTypeID,
			ProductTypeID:   j.ProductTypeID,
			Installer:       app.displayCharacter(ctx, j.InstallerID),
			InstallerID:     j.InstallerID,
			Status:          humanizeEnum(j.Status),
			Cost:            esi.FormatISK(j.Cost),
		}
		if j.ProductTypeID > 0 {
			row.Product = app.typeNameOrID(ctx, j.ProductTypeID)
		}
		facilityID := j.FacilityID
		if facilityID == 0 {
			facilityID = j.StationID
		}
		if facilityID > 0 {
			row.Facility = app.linkPlace(ctx, facilityID, app.econLocationTitle(ctx, facilityID))
		}
		if j.SuccessfulRuns > 0 || j.Status == "delivered" {
			row.Runs = fmt.Sprintf("%d / %s", j.SuccessfulRuns, esi.FormatInt(j.Runs))
		} else {
			row.Runs = esi.FormatInt(j.Runs)
		}
		row.Ends = formatFinish(j.EndDate)
		if j.Status == "active" || j.Status == "paused" {
			if t, err := time.Parse(time.RFC3339, j.EndDate); err == nil {
				if left := time.Until(t); left > 0 {
					row.Ends += " (in " + humanDuration(left) + ")"
				} else {
					row.Ends += " (ready)"
				}
			}
		}
		rows = append(rows, row)
	}
	// Active jobs first (by end date), then the rest newest-ended
	// first: the slice is small, so a stable sort on a computed
	// key string is enough.
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := rows[i].Status == "active", rows[j].Status == "active"
		if ai != aj {
			return ai
		}
		return rows[i].Ends > rows[j].Ends
	})
	if len(rows) > maxIndustryRows {
		view.JobsCut = len(rows) - maxIndustryRows
		rows = rows[:maxIndustryRows]
	}
	view.JobRows = rows
}

func (app *Application) fillBlueprints(ctx context.Context, characterID int64, view *industryView) {
	var blueprints esi.Blueprints
	view.Blueprints = app.econSection(ctx, characterID, esi.SnapBlueprints, &blueprints)
	if !view.Blueprints.Loaded {
		return
	}
	// Blueprint products for categorization (Issue 16)
	bpProducts := map[int64]int64{}
	if prodRows, err := app.queries.ListSDEBlueprintProducts(ctx); err == nil {
		for _, pr := range prodRows {
			bpProducts[pr.BlueprintTypeID] = pr.ProductTypeID
		}
	}

	rows := make([]blueprintRow, 0, len(blueprints))
	for _, bp := range blueprints {
		row := blueprintRow{
			Name:   app.typeNameOrID(ctx, bp.TypeID),
			TypeID: bp.TypeID,
			ME:     fmt.Sprintf("%d%%", bp.MaterialEfficiency),
			TE:     fmt.Sprintf("%d%%", bp.TimeEfficiency),
		}
		// Product info for categorization (Issue 16)
		if productID, ok := bpProducts[bp.TypeID]; ok {
			row.ProductName = app.typeNameOrID(ctx, productID)
			// Get product group for categorization
			if sdeType, err := app.queries.GetSDEType(ctx, productID); err == nil {
				if group, err := app.queries.GetSDEGroup(ctx, sdeType.GroupID); err == nil {
					row.ProductCategory = group.Name
				}
			}
		}
		// ESI semantics: quantity -1 marks an original (runs -1 =
		// unlimited), -2 a copy; anything else is a stack count.
		switch {
		case bp.Quantity == -1:
			row.Kind = "BPO"
			row.Runs = "∞"
		case bp.Quantity == -2:
			row.Kind = "BPC"
			row.Runs = esi.FormatInt(bp.Runs)
		default:
			row.Kind = "BPC"
			row.Runs = esi.FormatInt(bp.Runs)
		}
		if bp.LocationID > 0 {
			row.Location = app.linkPlace(ctx, bp.LocationID, app.econLocationTitle(ctx, bp.LocationID))
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ProductCategory != rows[j].ProductCategory {
			return rows[i].ProductCategory < rows[j].ProductCategory
		}
		return rows[i].Name < rows[j].Name
	})
	if len(rows) > maxIndustryRows {
		view.BlueprintsCut = len(rows) - maxIndustryRows
		rows = rows[:maxIndustryRows]
	}
	view.BlueprintRows = rows
}

func (app *Application) fillMining(ctx context.Context, characterID int64, view *industryView) {
	var ledger esi.MiningLedger
	view.Mining = app.econSection(ctx, characterID, esi.SnapMining, &ledger)
	if !view.Mining.Loaded {
		return
	}
	rows := make([]miningRow, 0, len(ledger))
	for _, m := range ledger {
		rows = append(rows, miningRow{
			Date:   m.Date,
			Ore:    app.typeNameOrID(ctx, m.TypeID),
			TypeID: m.TypeID,
			Qty:    esi.FormatInt(m.Quantity),
			System: app.linkPlace(ctx, m.SolarSystemID, app.locationTitle(ctx, m.SolarSystemID, "solar_system")),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Date != rows[j].Date {
			return rows[i].Date > rows[j].Date // dates are YYYY-MM-DD
		}
		return rows[i].Ore < rows[j].Ore
	})
	if len(rows) > maxIndustryRows {
		view.MiningCut = len(rows) - maxIndustryRows
		rows = rows[:maxIndustryRows]
	}
	view.MiningRows = rows
}
