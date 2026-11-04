package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Industry pages: Blueprints (/industry/, the blueprint
// library), My Jobs (/industry/jobs/, active jobs plus the last 90
// days of completed, via include_completed), and Mining Ledger
// (/industry/mining/, the recent mining ledger). Each page fills
// only its own section and renders from snapshots only.
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
	ProductTypeID   int64  // product type, when the SDE maps one (links to the build planner)
	ProductCategory string // category of the produced item
	Pinned          bool   // the account has pinned this blueprint as a production line
}

// industryView is the shared body of the three Industry pages:
// each handler fills only its own section's fields.
type industryView struct {
	CharacterName string
	CharacterID   int64
	Blueprints    econSectionState
	BlueprintRows []blueprintRow
	BlueprintsCut int
}

// maxIndustryRows caps each section table (corp-ledger precedent).
const maxIndustryRows = 100

// handleIndustry is the Blueprints page: the blueprint library
// alone at the section's root route.
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

	view := &industryView{CharacterName: active.Name, CharacterID: active.CharacterID}
	data.Industry = view

	app.fillBlueprints(ctx, active.CharacterID, view, app.blueprintPinSet(ctx, app.userID(ctx)))

	app.render(ctx, w, http.StatusOK, "industry.html", data)
}

// handleIndustryJobs is the My Jobs page: the character's industry
// jobs alone.
func (app *Application) handleIndustryJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	characters, _, links, err := app.pickCharacter(ctx, r, "/industry/jobs/")
	if err != nil {
		logging.Errorf("industry jobs: list characters: %v", err)
		data.Error = "Could not load industry data; check the server log."
		app.render(ctx, w, http.StatusOK, "industry_jobs.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "industry_jobs.html", data)
		return
	}
	data.IndustryChars = links

	data.JobsPage = app.buildJobsView(ctx, r, characters)

	app.render(ctx, w, http.StatusOK, "industry_jobs.html", data)
}

// handleIndustryMining is the Mining Ledger page: the character's
// mining ledger alone.
func (app *Application) handleIndustryMining(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	characters, _, links, err := app.pickCharacter(ctx, r, "/industry/mining/")
	if err != nil {
		logging.Errorf("industry mining: list characters: %v", err)
		data.Error = "Could not load industry data; check the server log."
		app.render(ctx, w, http.StatusOK, "industry_mining.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "industry_mining.html", data)
		return
	}
	data.IndustryChars = links

	data.MiningPage = app.buildMiningView(ctx, r, characters)

	app.render(ctx, w, http.StatusOK, "industry_mining.html", data)
}

// fillBlueprints loads the blueprint library snapshot and shapes
// every row, walking the stored product map (sde_blueprints) for
// the produced item and its category. pins is the account's pin
// set, marking which rows carry a production-line pin.
func (app *Application) fillBlueprints(ctx context.Context, characterID int64, view *industryView, pins map[int64]bool) {
	var blueprints esi.Blueprints
	view.Blueprints = app.econSection(ctx, characterID, esi.SnapBlueprints, &blueprints)
	if !view.Blueprints.Loaded {
		return
	}
	// Blueprint products for categorization
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
		// Product info for categorization
		if productID, ok := bpProducts[bp.TypeID]; ok {
			row.ProductTypeID = productID
			row.ProductName = app.typeNameOrID(ctx, productID)
			// Get product group for categorization
			if sdeType, err := app.queries.GetSDEType(ctx, productID); err == nil {
				if group, err := app.queries.GetSDEGroup(ctx, sdeType.GroupID); err == nil {
					row.ProductCategory = group.Name
				}
			}
		}
		row.Pinned = pins[bp.TypeID]
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
