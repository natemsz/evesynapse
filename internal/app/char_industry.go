package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
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
	ItemID          int64
	MEValue         int64
	TEValue         int64
	Original        bool
	Href            string // opens it in the inspector
	On              bool
	// Build is the stored verdict (schema 122): "ready" when the
	// owner has the skills and one run's materials on hand,
	// "materials" when only materials are short, "skills" when a
	// skill is missing, "" with no verdict. BuildNote says why.
	Build     string
	BuildNote string
	// OwnerID and OwnerName name the owning character; the name is
	// shown in the all-characters view.
	OwnerID   int64
	OwnerName string
}

// blueprintMaterial is one input per run at the blueprint's ME.
type blueprintMaterial struct {
	TypeID int64
	Name   string
	Base   string // the blueprint's own figure, before ME
	PerRun string // after this blueprint's ME
}

// blueprintInspector is the open blueprint: its own facts, what one
// run takes and makes at its research levels, and the way to plan it.
type blueprintInspector struct {
	Row        blueprintRow
	Makes      string // "1 Rifter per run"
	TimePerRun string // at this blueprint's TE
	BaseTime   string
	Materials  []blueprintMaterial
	CloseHref  string
}

// industryView is the shared body of the three Industry pages:
// each handler fills only its own section's fields.
type industryView struct {
	CharacterName string
	CharacterID   int64
	All           bool // every character's blueprints, judged for the account
	Blueprints    econSectionState
	BlueprintRows []blueprintRow
	BPTiles       []jobTile
	BPCats        []jobFilter // product categories, the directory
	BPKinds       []jobFilter // originals, copies
	BPInsp        *blueprintInspector
	BPDepth       int
	BPDirHref     string
	BPAllOn       bool
	BPAllHref     string
	BPTotal       int
	BlueprintsCut int
}

// maxIndustryRows caps each section table (corp-ledger precedent).
const maxIndustryRows = 100

// handleIndustry is the Blueprints page: the blueprint library
// alone at the section's root route.
func (app *Application) handleIndustry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	characters, active, links, err := app.pickCharacter(ctx, r, "/industry/")
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

	// "All characters" lists every character's blueprints, and its
	// build verdicts are the account's (any character's skills,
	// every hangar's materials); one character reads its own.
	view := &industryView{CharacterName: active.Name, CharacterID: active.CharacterID}
	owners := []db.Character{active}
	if r.URL.Query().Get("character") == "all" {
		view.All, view.CharacterName = true, "All characters"
		owners = characters
		for i := range links {
			links[i].Active = false
		}
	}
	data.IndustryChars = append([]assetCharLink{{Name: "All characters", Href: "/industry/?character=all", Active: view.All}}, links...)
	data.Industry = view

	app.fillBlueprints(ctx, r, owners, view, app.blueprintPinSet(ctx, app.userID(ctx)))

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
func (app *Application) fillBlueprints(ctx context.Context, r *http.Request, owners []db.Character, view *industryView, pins map[int64]bool) {
	var blueprints esi.Blueprints
	ownerOf := map[int64]db.Character{} // by item id
	for i, ch := range owners {
		var bps esi.Blueprints
		st := app.econSection(ctx, ch.CharacterID, esi.SnapBlueprints, &bps)
		if i == 0 || (st.Loaded && !view.Blueprints.Loaded) {
			view.Blueprints = st
		}
		if !st.Loaded {
			continue
		}
		for _, bp := range bps {
			ownerOf[bp.ItemID] = ch
		}
		blueprints = append(blueprints, bps...)
	}
	if !view.Blueprints.Loaded {
		return
	}
	// Blueprint products for categorization, bounded to the
	// blueprints this character actually owns.
	bpProducts := map[int64]int64{}
	ownedIDs := make([]int64, 0, len(blueprints))
	seenBP := map[int64]bool{}
	for _, bp := range blueprints {
		if !seenBP[bp.TypeID] {
			seenBP[bp.TypeID] = true
			ownedIDs = append(ownedIDs, bp.TypeID)
		}
	}
	if len(ownedIDs) > 0 {
		if prodRows, err := app.queries.ListSDEBlueprintProductsForBlueprints(ctx, ownedIDs); err == nil {
			for _, pr := range prodRows {
				bpProducts[pr.BlueprintTypeID] = pr.ProductTypeID
			}
		}
	}

	// Product categories: one lookup per product group, not per row.
	groupOf := map[int64]string{}
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
				name, ok := groupOf[sdeType.GroupID]
				if !ok {
					if group, err := app.queries.GetSDEGroup(ctx, sdeType.GroupID); err == nil {
						name = group.Name
					}
					groupOf[sdeType.GroupID] = name
				}
				row.ProductCategory = name
			}
		}
		row.Pinned = pins[bp.TypeID]
		row.OwnerID = ownerOf[bp.ItemID].CharacterID
		if view.All {
			row.OwnerName = ownerOf[bp.ItemID].Name
		}
		row.ItemID, row.MEValue, row.TEValue = bp.ItemID, bp.MaterialEfficiency, bp.TimeEfficiency
		row.Original = bp.Quantity == -1
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
	scope := strconv.FormatInt(view.CharacterID, 10)
	if view.All {
		scope = "all"
	}
	app.shapeBlueprints(ctx, r, scope, view, rows)
}

// shapeBlueprints builds the library's tiles, directory and filters
// over every blueprint, then the filtered list and the inspector.
func (app *Application) shapeBlueprints(ctx context.Context, r *http.Request, scope string, view *industryView, rows []blueprintRow) {
	q := r.URL.Query()
	cat, kind := q.Get("cat"), q.Get("kind")
	pick, _ := strconv.ParseInt(q.Get("bp"), 10, 64)
	href := func(c, k string, bp int64, list bool) string {
		v := url.Values{}
		v.Set("character", scope)
		if list {
			v.Set("list", "1")
		}
		v.Set("cat", c)
		v.Set("kind", k)
		if bp != 0 {
			v.Set("bp", strconv.FormatInt(bp, 10))
		}
		return withQuery("/industry/", v)
	}
	originals, copies, researched := 0, 0, 0
	catCount := map[string]int{}
	var catOrder []string
	for _, row := range rows {
		if row.Original {
			originals++
			if row.MEValue >= 10 && row.TEValue >= 20 {
				researched++
			}
		} else {
			copies++
		}
		key := row.ProductCategory
		if _, seen := catCount[key]; !seen {
			catOrder = append(catOrder, key)
		}
		catCount[key]++
	}
	view.BPTotal = len(rows)
	view.BPTiles = []jobTile{
		{Label: "Originals", Value: strconv.Itoa(originals), Sub: "unlimited runs", Href: href(cat, "bpo", 0, true), On: kind == "bpo"},
		{Label: "Copies", Value: strconv.Itoa(copies), Sub: "limited runs", Href: href(cat, "bpc", 0, true), On: kind == "bpc"},
		{Label: "Fully researched", Value: strconv.Itoa(researched), Sub: "originals at ME 10 and TE 20"},
		{Label: "Product groups", Value: strconv.Itoa(len(catOrder)), Sub: "what the library builds"},
	}
	view.BPAllOn = cat == ""
	view.BPAllHref = href("", kind, 0, true)
	view.BPDirHref = href("", "", 0, false)
	for _, c := range catOrder {
		label := c
		if label == "" {
			label = "Other"
		}
		key := c
		if key == "" {
			key = "-"
		}
		view.BPCats = append(view.BPCats, jobFilter{Label: label, Count: catCount[c], Href: href(key, kind, 0, true), On: cat == key})
	}
	view.BPKinds = []jobFilter{
		{Label: "All", Count: len(rows), Href: href(cat, "", 0, true), On: kind == ""},
		{Label: "Originals", Count: originals, Href: href(cat, "bpo", 0, true), On: kind == "bpo"},
		{Label: "Copies", Count: copies, Href: href(cat, "bpc", 0, true), On: kind == "bpc"},
	}
	var picked []blueprintRow
	for _, row := range rows {
		rc := row.ProductCategory
		if rc == "" {
			rc = "-"
		}
		if (cat != "" && rc != cat) || (kind == "bpo" && !row.Original) || (kind == "bpc" && row.Original) {
			continue
		}
		row.Href = href(cat, kind, row.ItemID, true)
		row.On = row.ItemID == pick
		picked = append(picked, row)
	}
	if len(picked) > maxIndustryRows {
		view.BlueprintsCut = len(picked) - maxIndustryRows
		picked = picked[:maxIndustryRows]
	}
	app.attachBlueprintBuild(ctx, view.All, picked)
	view.BlueprintRows = picked
	view.BPDepth = 1
	if q.Get("list") != "1" && cat == "" && kind == "" {
		view.BPDepth = 0
	}
	if pick != 0 {
		for _, row := range rows {
			if row.ItemID != pick {
				continue
			}
			row.Href = href(cat, kind, row.ItemID, true)
			one := []blueprintRow{row}
			app.attachBlueprintBuild(ctx, view.All, one)
			row = one[0]
			view.BPInsp = app.blueprintInspectorFor(ctx, row, href(cat, kind, 0, true))
			view.BPDepth = 2
			break
		}
	}
}

// blueprintInspectorFor reads what one run takes and makes from the
// static data (bounded: one blueprint), at this blueprint's ME and TE,
// before any structure, rig or skill bonus.
func (app *Application) blueprintInspectorFor(ctx context.Context, row blueprintRow, closeHref string) *blueprintInspector {
	in := &blueprintInspector{Row: row, CloseHref: closeHref}
	if bp, err := app.queries.GetSDEBlueprint(ctx, row.TypeID); err == nil {
		if row.ProductName != "" {
			in.Makes = fmt.Sprintf("%s %s per run", esi.FormatInt(bp.ProductQuantity), row.ProductName)
		}
		if bp.ManufacturingTimeSeconds > 0 {
			base := time.Duration(bp.ManufacturingTimeSeconds) * time.Second
			in.BaseTime = humanDuration(base)
			in.TimePerRun = humanDuration(time.Duration(float64(base) * (1 - float64(row.TEValue)/100)))
		}
	}
	if mats, err := app.queries.ListSDEBlueprintMaterials(ctx, row.TypeID); err == nil {
		for _, m := range mats {
			per := int64(math.Ceil(float64(m.Quantity) * (1 - float64(row.MEValue)/100)))
			if per < 1 {
				per = 1
			}
			in.Materials = append(in.Materials, blueprintMaterial{TypeID: m.MaterialTypeID, Name: app.typeNameOrID(ctx, m.MaterialTypeID), Base: esi.FormatInt(m.Quantity), PerRun: esi.FormatInt(per)})
		}
	}
	return in
}

// attachBlueprintBuild reads the stored build verdicts for the rows
// a page shows (bounded by the table cap) and words them: the
// owner's verdict for one character, the account's for all.
func (app *Application) attachBlueprintBuild(ctx context.Context, all bool, rows []blueprintRow) {
	userID := app.userID(ctx)
	if len(rows) == 0 || userID == 0 {
		return
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ItemID
	}
	verdicts, err := app.queries.ListBlueprintBuildable(ctx, db.ListBlueprintBuildableParams{UserID: userID, ItemIds: ids})
	if err != nil {
		logging.Errorf("industry: build verdicts for user %d: %v", userID, err)
		return
	}
	byItem := make(map[int64]db.ListBlueprintBuildableRow, len(verdicts))
	for _, v := range verdicts {
		byItem[v.ItemID] = v
	}
	for i := range rows {
		v, ok := byItem[rows[i].ItemID]
		if !ok {
			continue
		}
		skills, mats, missing, short := v.HasSkills, v.HasMaterials, v.MissingSkills, v.ShortMaterials
		if all {
			skills, mats, missing, short = v.AnySkills, v.AnyMaterials, v.AnyMissingSkills, v.AnyShortMaterials
		}
		switch {
		case !skills:
			rows[i].Build, rows[i].BuildNote = "skills", "Needs "+missing
		case !mats && all:
			rows[i].Build = "materials"
			rows[i].BuildNote = fmt.Sprintf("A character has the skills. Short %d of %d materials for one run across your hangars", short, v.TotalMaterials)
		case !mats:
			rows[i].Build = "materials"
			rows[i].BuildNote = fmt.Sprintf("Skills trained. Short %d of %d materials for one run in this character's hangars", short, v.TotalMaterials)
		default:
			rows[i].Build = "ready"
			if all {
				rows[i].BuildNote = "A character has the skills, and one run's materials are across your hangars"
			} else {
				rows[i].BuildNote = "Skills trained and one run's materials on hand"
			}
		}
	}
}
