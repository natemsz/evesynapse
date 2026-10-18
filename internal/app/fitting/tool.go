package fitting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// fitEditorView is the editor chrome around the workbench.
type fitEditorView struct {
	PilotID int64
	Pilots  []module.CharLink // the user's characters; the template adds All V
	// CloneID/Clones feed the Clone dropdown next to the pilot
	// picker (copied from the initial Sim; the dropdown itself
	// lives in the chrome so it survives workbench re-renders).
	CloneID     int64
	Clones      []fitview.FitCloneOption
	EVECharID   int64 // active character: the Save-to-EVE target (0 = none)
	EVECharName string
	Description string // fit metadata, editable in the header, autosaved
	Tags        string // comma-separated for the header field; stored as an array
	TagsList    []string
	IsPublic    bool
	Sim         *fitview.FitSimView
	LocalID     int64    // saved fit currently open (0 = unsaved)
	Notes       []string // import notes ("couldn't place ..."), shown once
	LocalFits   []localFitEntry
	// Anonymous serves the editor without a session: All-V
	// skills, no pilots, no save controls, and a sign-in note.
	Anonymous bool
}

// localFitEntry is one saved fit for the your-fits list.
type localFitEntry struct {
	ID          int64
	Name        string
	ShipName    string
	Description string
	Tags        []string
	Updated     string
	IsPublic    bool
	IsDraft     bool
}

// fitSimRequest is the simulate/save/export request body: the fit
// document plus (for simulate) the pilot to compute for.
type fitSimRequest struct {
	fitview.FitDoc
	Pilot int64 `json:"pilot"`
	Clone int64 `json:"clone"` // jump clone ID; 0 = active clone
}

// readFitRequest decodes a fit JSON body (bounded) and sanitizes
// the document.
func readFitRequest(w http.ResponseWriter, r *http.Request) (fitSimRequest, bool) {
	var req fitSimRequest
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That fitting couldn't be read.", http.StatusBadRequest)
		return req, false
	}
	fitview.SanitizeFitDoc(&req.FitDoc)
	return req, true
}

// handleFitSimulate serves POST /fittings/simulate/: run the
// engine for the posted fit and return the workbench fragment.
func (app *tool) handleFitSimulate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := readFitRequest(w, r)
	if !ok {
		return
	}
	userID := app.deps.UserID(ctx)

	pilotLabel := "All V"
	if req.Pilot != 0 {
		label, owned := app.fitPilotLabel(ctx, userID, req.Pilot)
		if !owned {
			http.Error(w, "That pilot isn't one of your characters.", http.StatusBadRequest)
			return
		}
		pilotLabel = label
	}
	view := app.deps.FitBuilder().BuildFitSimView(ctx, &req.FitDoc, req.Pilot, pilotLabel, req.Clone)
	app.deps.RenderFragment(w, "fittings.html", "fit-workbench", view)
}

// handleFitClonesJSON serves GET /fittings/clones.json: the
// Clone dropdown options for one pilot (active clone plus jump
// clones). Lets the editor refresh the dropdown when the pilot
// picker changes without a full page load.
func (app *tool) handleFitClonesJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	pilotID, _ := strconv.ParseInt(r.URL.Query().Get("pilot"), 10, 64)
	if _, owned := app.fitPilotLabel(ctx, userID, pilotID); !owned {
		writeFitJSON(w, map[string]any{"ok": false, "error": "That pilot isn't one of your characters."})
		return
	}
	_, effectiveClone, clones, note := app.deps.FitBuilder().FitPilotImplants(ctx, pilotID, 0)
	opts := make([]map[string]any, 0, len(clones))
	for _, c := range clones {
		label := c.Name
		if c.Implants > 0 {
			label = fmt.Sprintf("%s (%d)", c.Name, c.Implants)
		}
		opts = append(opts, map[string]any{"id": c.ID, "name": label})
	}
	writeFitJSON(w, map[string]any{"ok": true, "clones": opts, "clone": effectiveClone, "note": note})
}

// fitPilotLabel resolves a pilot character ID to its name when
// it belongs to the signed-in user.
func (app *tool) fitPilotLabel(ctx context.Context, userID, pilotID int64) (string, bool) {
	if userID == 0 || pilotID == 0 {
		return "", false
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("fittings sim: list characters for user %d: %v", userID, err)
		return "", false
	}
	for _, ch := range chars {
		if ch.CharacterID == pilotID {
			return ch.Name, true
		}
	}
	return "", false
}

// handleFitPickerJSON serves GET /fittings/picker.json: the
// editor's item feeds (ships, slot families, drones, charges, or
// the shared marketable pool), all local SDE reads.
func (app *tool) handleFitPickerJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	family := q.Get("family")
	query := q.Get("q")
	meta, _ := strconv.ParseInt(q.Get("meta"), 10, 64)
	pilotID, _ := strconv.ParseInt(q.Get("pilot"), 10, 64)
	usableOnly := q.Get("usable") == "1" && pilotID > 0 && app.deps.UserID(ctx) > 0
	if pilotID > 0 {
		if _, owned := app.fitPilotLabel(ctx, app.deps.UserID(ctx), pilotID); !owned {
			pilotID = 0
			usableOnly = false
		}
	}

	// pilotCanUse answers, for the rows given, whether the pilot can
	// actually use each: every skill requirement (transitively) met by
	// their levels. Nil when that can't be told (skills not warmed).
	pilotCanUse := func(rows []module.SuggestItem) func(int64) bool {
		var skills esi.Skills
		if !app.deps.LoadCorpSnapshot(ctx, pilotID, esi.SnapSkills, &skills) {
			return nil
		}
		levels := fitview.ActiveSkillLevels(skills)
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		// Requirement closure over a few rounds (module -> skill -> skill).
		reqs := make(map[int64][]db.SdeRequirement)
		pending := ids
		for round := 0; round < 4 && len(pending) > 0; round++ {
			batch, err := app.queries.ListSDERequirementsByTypes(ctx, pending)
			if err != nil {
				logging.Errorf("fittings picker: requirements: %v", err)
				return nil
			}
			var next []int64
			seen := make(map[int64]bool)
			for _, b := range batch {
				reqs[b.TypeID] = append(reqs[b.TypeID], b)
				if !seen[b.SkillTypeID] {
					seen[b.SkillTypeID] = true
					next = append(next, b.SkillTypeID)
				}
			}
			pending = next
		}
		usable := func(typeID int64) bool {
			seen := make(map[int64]bool)
			queue := []int64{typeID}
			for len(queue) > 0 {
				id := queue[0]
				queue = queue[1:]
				if seen[id] {
					continue
				}
				seen[id] = true
				for _, req := range reqs[id] {
					if int64(levels[req.SkillTypeID]) < req.Level {
						return false
					}
					queue = append(queue, req.SkillTypeID)
				}
			}
			return true
		}
		return usable
	}
	// usableByPilot filters suggestions to what the pilot can use.
	usableByPilot := func(rows []module.SuggestItem) []module.SuggestItem {
		if !usableOnly || len(rows) == 0 {
			return rows
		}
		usable := pilotCanUse(rows)
		if usable == nil {
			return rows // skills not warmed yet: don't hide everything
		}
		out := rows[:0]
		for _, row := range rows {
			if usable(row.ID) {
				out = append(out, row)
			}
		}
		return out
	}

	slotKinds := []struct {
		family string
		effect int64
		kind   string
		lim    int64
	}{
		{fitview.FitFamilyHigh, fit.EffectHiPower, "high", 8},
		{fitview.FitFamilyMedium, fit.EffectMedPower, "medium", 8},
		{fitview.FitFamilyLow, fit.EffectLoPower, "low", 8},
		{fitview.FitFamilyRig, fit.EffectRigSlot, "rig", 6},
		{fitview.FitFamilySubsystem, fit.EffectSubsystemSlot, "subsystem", 6},
	}
	querySlotFamily := func(family, kind string, lim int64) []module.SuggestItem {
		effectID := fitview.FitSlotEffectByFamily[family]
		rows, err := app.queries.ListFitSlotTypes(ctx, db.ListFitSlotTypesParams{
			EffectID: effectID, Q: query, Lim: lim, Meta: meta,
		})
		if err != nil {
			logging.Errorf("fittings picker: family %s: %v", family, err)
			return nil
		}
		out := make([]module.SuggestItem, 0, len(rows))
		for _, row := range rows {
			out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: kind})
		}
		return out
	}

	out := []module.SuggestItem{}
	switch {
	case family == "all":
		// Unified search: ships plus every module family, grouped
		// by kind client-side. Charges are deliberately excluded:
		// without a fitted weapon they cannot be placed.
		if rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: query, Lim: 6}); err == nil {
			for _, row := range rows {
				out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "ship"})
			}
		} else {
			logging.Errorf("fittings picker: ships: %v", err)
		}
		for _, sk := range slotKinds {
			out = append(out, querySlotFamily(sk.family, sk.kind, sk.lim)...)
		}
		if rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 6, Meta: meta}); err == nil {
			for _, row := range rows {
				out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "drone"})
			}
		} else {
			logging.Errorf("fittings picker: drones: %v", err)
		}
	case family == "ship":
		rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: query, Lim: 12})
		if err != nil {
			logging.Errorf("fittings picker: ships: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "ship"})
		}
	case family == fitview.FitFamilyDrone:
		rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 25, Meta: meta})
		if err != nil {
			logging.Errorf("fittings picker: drones: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "drone"})
		}
	case family == "implant":
		// One slot's implants. Every one lists; those the pilot
		// can't use yet are marked rather than hidden.
		slot, _ := strconv.ParseInt(q.Get("slot"), 10, 64)
		if slot < 1 || slot > fitview.FitImplantSlots {
			break
		}
		rows, err := app.queries.ListFitImplantTypes(ctx, db.ListFitImplantTypesParams{Slot: slot, Q: query, Lim: 60})
		if err != nil {
			logging.Errorf("fittings picker: implants: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "implant"})
		}
		if _, owned := app.fitPilotLabel(ctx, app.deps.UserID(ctx), pilotID); owned && len(out) > 0 {
			if usable := pilotCanUse(out); usable != nil {
				for i := range out {
					out[i].Missing = !usable(out[i].ID)
				}
			}
		}
		app.deps.WriteSuggest(w, out)
		return
	case family == "charge":
		weaponID, _ := strconv.ParseInt(q.Get("weapon"), 10, 64)
		if weaponID > 0 {
			needle := strings.ToLower(strings.TrimSpace(query))
			for _, row := range app.deps.FitBuilder().FitChargeCandidates(ctx, weaponID) {
				if needle != "" && !strings.Contains(strings.ToLower(row.Name), needle) {
					continue
				}
				out = append(out, module.SuggestItem{ID: row.TypeID, Name: row.Name, Kind: "charge"})
			}
		}
	default:
		if _, isSlot := fitview.FitSlotEffectByFamily[family]; isSlot {
			out = append(out, querySlotFamily(family, family, 25)...)
			break
		}
		app.deps.WriteSuggest(w, usableByPilot(app.deps.SuggestTypes(ctx, query, "market", 12)))
		return
	}
	app.deps.WriteSuggest(w, usableByPilot(out))
}

// handleFitSave serves POST /fittings/save/: create or update one
// of the user's local fittings. The editor autosaves through this
// same path: id 0 upserts the user's single draft row (never
// spawning duplicates), id > 0 updates that row, and promote flips
// a draft into a named fit when the Save button names it.
// Description and tags ride inside the fit document; is_public is
// the community-fit flag (drafts are never public).
func (app *tool) handleFitSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in before saving a fit.", http.StatusForbidden)
		return
	}
	var req struct {
		ID          int64          `json:"id"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Tags        string         `json:"tags"`
		IsPublic    bool           `json:"isPublic"`
		Promote     bool           `json:"promote"`
		Fit         fitview.FitDoc `json:"fit"`
	}
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That fitting couldn't be read.", http.StatusBadRequest)
		return
	}
	fitview.SanitizeFitDoc(&req.Fit)
	if req.Fit.ShipTypeID <= 0 {
		http.Error(w, "Pick a ship before saving the fit.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(req.Fit.Name)
	}
	if name == "" {
		name = "Unnamed fit"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	description := strings.TrimSpace(req.Description)
	if len(description) > 500 {
		description = description[:500]
	}
	req.Fit.Name = name
	req.Fit.Description = description
	req.Fit.Tags = fitview.ParseFitTags(req.Tags)
	raw, err := json.Marshal(req.Fit)
	if err != nil {
		http.Error(w, "That fitting couldn't be saved.", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()

	id := req.ID
	isDraft := false
	isPublic := false
	if id > 0 {
		row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: id, UserID: userID})
		if err != nil {
			http.Error(w, "That saved fit couldn't be found.", http.StatusNotFound)
			return
		}
		isDraft = row.IsDraft
		if isDraft && req.Promote && name != "Unnamed fit" {
			isDraft = false // the Save button names the draft: it becomes a real fit
		}
		if !isDraft {
			isPublic = req.IsPublic
			if !app.deps.ModuleEnabled("publicfits") {
				// Making a fit public is the Public Fits module's surface;
				// without it a saved fit keeps the flag it had.
				isPublic = row.IsPublic
			}
		}
		if err := app.queries.UpdateLocalFitting(ctx, db.UpdateLocalFittingParams{
			Name: name, ShipTypeID: req.Fit.ShipTypeID, ItemsJson: string(raw),
			IsPublic: isPublic, IsDraft: isDraft, UpdatedAt: now, ID: id, UserID: userID,
		}); err != nil {
			logging.Errorf("fittings save: update %d: %v", id, err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
	} else if draft, derr := app.queries.GetUserDraftFitting(ctx, userID); derr == nil {
		// Autosave reuses the one draft row; it never spawns
		// duplicates.
		id = draft.ID
		if err := app.queries.UpdateLocalFitting(ctx, db.UpdateLocalFittingParams{
			Name: name, ShipTypeID: req.Fit.ShipTypeID, ItemsJson: string(raw),
			IsPublic: false, IsDraft: true, UpdatedAt: now, ID: id, UserID: userID,
		}); err != nil {
			logging.Errorf("fittings save: update draft %d: %v", id, err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
		isDraft = true
	} else {
		isDraft = !(req.Promote && name != "Unnamed fit")
		if !isDraft {
			isPublic = req.IsPublic && app.deps.ModuleEnabled("publicfits")
		}
		row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: userID, Name: name, ShipTypeID: req.Fit.ShipTypeID,
			ItemsJson: string(raw), IsPublic: isPublic, IsDraft: isDraft,
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			logging.Errorf("fittings save: create: %v", err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
		id = row.ID
	}
	// The browser's max-price filter and sorts read the stored
	// figure, priced here at write time from the guide mirror.
	// Unknown reads as 0, which also clears any stale figure.
	if err := app.queries.SetLocalFittingPrice(ctx, db.SetLocalFittingPriceParams{
		TotalPrice: app.deps.FitBuilder().FitDocPrice(ctx, &req.Fit), ID: id, UserID: userID,
	}); err != nil {
		logging.Errorf("fittings save: price %d: %v", id, err)
	}
	// A public fit's page is built now, on its owner's request,
	// never on a visitor's.
	if isPublic && !isDraft && app.deps.ModuleEnabled("publicfits") {
		app.deps.StorePublicFitPageNow(ctx, id)
	}
	writeFitJSON(w, map[string]any{"id": id, "savedAt": rfc3339(now), "isDraft": isDraft})
}

// writeFitJSON answers a fitting-editor JSON endpoint.
func writeFitJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// esiFittingItem is one line of POST /characters/{id}/fittings/.
type esiFittingItem struct {
	Flag     string `json:"flag"`
	Quantity int64  `json:"quantity"`
	TypeID   int64  `json:"type_id"`
}

// esiFittingBody is POST /characters/{id}/fittings/ (201 answers
// {"fitting_id": ...}).
type esiFittingBody struct {
	Description string           `json:"description"`
	Items       []esiFittingItem `json:"items"`
	Name        string           `json:"name"`
	ShipTypeID  int64            `json:"ship_type_id"`
}

// fitESIFlagFamilies maps editor families to their ESI slot-flag
// prefix and slot count. Drones and cargo ride unindexed flags.
var fitESIFlagFamilies = []struct {
	family string
	prefix string
	slots  int
	label  string
}{
	{fitview.FitFamilyHigh, "HiSlot", 8, "high-slot"},
	{fitview.FitFamilyMedium, "MedSlot", 8, "mid-slot"},
	{fitview.FitFamilyLow, "LoSlot", 8, "low-slot"},
	{fitview.FitFamilyRig, "RigSlot", 3, "rig"},
	{fitview.FitFamilySubsystem, "SubSystemSlot", 4, "subsystem"},
}

// fitESIFittingBody maps the editor document to the ESI fitting
// body: one flag per fitted module instance (HiSlot0-7 and
// friends), charges riding the same flag as their weapon's slot,
// drones to DroneBay and cargo to Cargo. Items beyond the family's
// slot count are an error — ESI has no flag for them, so the
// handler reports it instead of silently dropping them.
func fitESIFittingBody(name string, doc *fitview.FitDoc, familyOf map[int64]string) (esiFittingBody, error) {
	body := esiFittingBody{
		Description: "Created by EveSynapse (https://github.com/natemsz/evesynapse)",
		Name:        name,
		ShipTypeID:  doc.ShipTypeID,
	}
	byFamily := map[string][]int64{}
	for _, it := range doc.Items {
		switch familyOf[it.TypeID] {
		case fitview.FitFamilyHigh, fitview.FitFamilyMedium, fitview.FitFamilyLow, fitview.FitFamilyRig, fitview.FitFamilySubsystem:
			for i := 0; i < it.Qty; i++ {
				byFamily[familyOf[it.TypeID]] = append(byFamily[familyOf[it.TypeID]], it.TypeID)
			}
		}
	}
	// Weapon slots first: charges need the flag of their weapon.
	weaponFlag := map[int64]string{}
	for _, ff := range fitESIFlagFamilies {
		ids := byFamily[ff.family]
		if len(ids) > ff.slots {
			return body, fmt.Errorf("%d %s modules don't fit in %d %s slots — remove %d first",
				len(ids), ff.label, ff.slots, ff.label, len(ids)-ff.slots)
		}
		for i, id := range ids {
			flag := fmt.Sprintf("%s%d", ff.prefix, i)
			body.Items = append(body.Items, esiFittingItem{Flag: flag, Quantity: 1, TypeID: id})
			if _, ok := weaponFlag[id]; !ok {
				weaponFlag[id] = flag
			}
		}
	}
	for _, it := range doc.Items {
		switch familyOf[it.TypeID] {
		case fitview.FitFamilyDrone:
			body.Items = append(body.Items, esiFittingItem{Flag: "DroneBay", Quantity: int64(it.Qty), TypeID: it.TypeID})
		case fitview.FitFamilyCargo:
			body.Items = append(body.Items, esiFittingItem{Flag: "Cargo", Quantity: int64(it.Qty), TypeID: it.TypeID})
		}
	}
	for weapon, charge := range doc.Charges {
		flag, ok := weaponFlag[weapon]
		if !ok {
			continue // weapon not fitted: its charge can't ride along
		}
		body.Items = append(body.Items, esiFittingItem{Flag: flag, Quantity: 1, TypeID: charge})
	}
	return body, nil
}

// esiSaveToEVEHint turns an ESI failure into a plain sentence for
// the editor; token values never appear.
func esiSaveToEVEHint(err error) string {
	var se *esi.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusBadRequest:
			return "EVE rejected the fitting as sent — or the character's fitting list is full"
		case http.StatusForbidden:
			return "this character hasn't granted fitting write access"
		case http.StatusUnprocessableEntity:
			return "EVE couldn't place one of the modules"
		}
		return fmt.Sprintf("EVE answered status %d", se.Code)
	}
	if errors.Is(err, esi.ErrErrorLimit) {
		return "EVE is rate-limiting requests right now"
	}
	return "the request to EVE failed"
}

// fitSaveToEVERequest is POST /fittings/save-to-eve/: the target
// character plus the fit document.
type fitSaveToEVERequest struct {
	fitview.FitDoc
	CharacterID int64 `json:"characterId"`
}

// handleFitSaveToEVE serves POST /fittings/save-to-eve/: map the
// editor document to ESI slot flags and create the fitting in-game
// for one of the signed-in user's characters. A 403 (character
// linked before the write scope existed) is reported as "sign in
// again", never silently swallowed.
func (app *tool) handleFitSaveToEVE(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	var req fitSaveToEVERequest
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeFitJSON(w, map[string]any{"ok": false, "error": "That fitting couldn't be read."})
		return
	}
	fitview.SanitizeFitDoc(&req.FitDoc)
	fail := func(msg string, relink bool) {
		writeFitJSON(w, map[string]any{"ok": false, "error": msg, "relink": relink})
	}

	charName, owned := app.fitPilotLabel(ctx, userID, req.CharacterID)
	if !owned {
		fail("That character isn't one of yours.", false)
		return
	}
	if req.ShipTypeID <= 0 {
		fail("Pick a ship before saving to EVE.", false)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Unnamed fit"
	}

	snap, err := fit.LoadSnapshot(ctx, app.queries, fitview.FitDocTypeIDs(&req.FitDoc))
	if err != nil {
		logging.Errorf("fittings save-to-eve: snapshot: %v", err)
		fail("Could not read the module data; try again.", false)
		return
	}
	familyOf := map[int64]string{}
	for _, it := range req.Items {
		familyOf[it.TypeID] = fitview.FitSlotFamilyOf(snap, it.TypeID)
	}
	esiBody, err := fitESIFittingBody(name, &req.FitDoc, familyOf)
	if err != nil {
		fail(err.Error(), false)
		return
	}

	ch, err := app.queries.GetCharacter(ctx, req.CharacterID)
	if err != nil {
		fail("That character isn't one of yours.", false)
		return
	}
	token, err := app.deps.AccessToken(ctx, ch)
	if err != nil {
		logging.Errorf("fittings save-to-eve: token for character %d: %v", req.CharacterID, err)
		fail("Could not reach EVE for "+charName+". Sign in again if it keeps failing.", true)
		return
	}
	var created struct {
		FittingID int64 `json:"fitting_id"`
	}
	path := fmt.Sprintf("/characters/%d/fittings/", req.CharacterID)
	if err := app.deps.PostESI(ctx, token, path, esiBody, &created); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			fail(charName+" was linked before EveSynapse asked for fitting write access — sign in again to grant it, then save once more.", true)
			return
		}
		logging.Errorf("fittings save-to-eve: ESI POST %s: %v", path, err)
		fail("EVE refused the fitting ("+esiSaveToEVEHint(err)+").", false)
		return
	}

	// Refresh the cached fittings so the new fit shows up in the
	// list without waiting for the next worker cycle.
	if ferr := app.deps.RefreshSnapshot(ctx, ch, esi.SnapFittings); ferr != nil {
		logging.Errorf("fittings save-to-eve: refetch fittings for character %d: %v", req.CharacterID, ferr)
	}
	writeFitJSON(w, map[string]any{
		"ok": true, "fittingId": created.FittingID, "name": name, "characterId": req.CharacterID,
	})
}

// handleFitDelete serves POST /fittings/delete/ (form field id).
func (app *tool) handleFitDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err == nil {
		if id, perr := strconv.ParseInt(r.FormValue("id"), 10, 64); perr == nil && id > 0 {
			if err := app.queries.DeleteLocalFitting(ctx, db.DeleteLocalFittingParams{ID: id, UserID: userID}); err != nil {
				logging.Errorf("fittings delete: %d: %v", id, err)
			}
		}
	}
	http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
}

// fitMineRow is one entry in the your-fits search results.
type fitMineRow struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ShipName string `json:"shipName"`
	Mine     bool   `json:"mine"`
	IsPublic bool   `json:"isPublic"`
	IsDraft  bool   `json:"isDraft"`
	IsESI    bool   `json:"isESI,omitempty"`
	Author   string `json:"author,omitempty"`
}

// handleFitMineJSON serves GET /fittings/mine.json: the user's
// saved fits matching q, plus other users' public fits when
// community=1 (bounded, capped at 20). Drives the your-fits
// search bar on the fitting screen.
func (app *tool) handleFitMineJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in first.", http.StatusForbidden)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeFitJSON(w, []fitMineRow{})
		return
	}
	rows, err := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: q})
	if err != nil {
		logging.Errorf("fittings mine search: %v", err)
		http.Error(w, "That search couldn't run.", http.StatusInternalServerError)
		return
	}
	out := make([]fitMineRow, 0, len(rows)+20)
	for _, row := range rows {
		out = append(out, fitMineRow{
			ID: row.ID, Name: row.Name, ShipName: row.ShipName,
			Mine: true, IsPublic: row.IsPublic, IsDraft: row.IsDraft,
		})
	}
	// ESI fittings for the active character
	// These are the in-game fittings from the character's ESI snapshot.
	if charID := app.deps.SessionCharacterID(ctx); charID != 0 {
		if char, err := app.queries.GetCharacter(ctx, charID); err == nil && char.UserID == userID {
			var fittings esi.Fittings
			if err := app.deps.StoredSnapshot(ctx, char, esi.SnapFittings, &fittings); err == nil {
				qLower := strings.ToLower(q)
				for _, f := range fittings {
					shipName := app.deps.TypeNameOrID(ctx, f.ShipTypeID)
					if strings.Contains(strings.ToLower(f.Name), qLower) || strings.Contains(strings.ToLower(shipName), qLower) {
						out = append(out, fitMineRow{
							ID:   -f.FittingID, // negative ID marks ESI fit
							Name: f.Name, ShipName: shipName,
							Mine: true, IsESI: true,
						})
					}
				}
			}
		}
	}
	if r.URL.Query().Get("community") == "1" {
		pub, err := app.queries.SearchPublicFittings(ctx, db.SearchPublicFittingsParams{UserID: userID, Q: q})
		if err != nil {
			logging.Errorf("fittings community search: %v", err)
		} else {
			for _, row := range pub {
				author := row.AuthorName
				out = append(out, fitMineRow{
					ID: row.ID, Name: row.Name, ShipName: row.ShipName,
					Mine: false, IsPublic: true, Author: author,
				})
			}
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	writeFitJSON(w, out)
}

// handleFitFork serves POST /fittings/fork/: copy another user's
// public fit into the caller's own fittings as a draft. The
// origin is noted in the description; the source fit is never
// touched.
func (app *tool) handleFitFork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in first.", http.StatusForbidden)
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil || req.ID <= 0 {
		http.Error(w, "That fit couldn't be read.", http.StatusBadRequest)
		return
	}
	src, err := app.queries.GetPublicFitting(ctx, req.ID)
	if err != nil {
		http.Error(w, "That fit couldn't be found.", http.StatusNotFound)
		return
	}
	if src.UserID == userID {
		writeFitJSON(w, map[string]any{"id": src.ID})
		return
	}
	var doc fitview.FitDoc
	if err := json.Unmarshal([]byte(src.ItemsJson), &doc); err != nil {
		http.Error(w, "That fit couldn't be read.", http.StatusBadRequest)
		return
	}
	fitview.SanitizeFitDoc(&doc)
	author := src.AuthorName
	origin := fmt.Sprintf("Based on %q by %s.", src.Name, author)
	if doc.Description != "" {
		doc.Description += "\n\n" + origin
	} else {
		doc.Description = origin
	}
	// Forks are always private drafts; the user publishes
	// deliberately via the Make public toggle.
	doc.Tags = nil
	doc.Name = src.Name
	raw, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "That fit couldn't be copied.", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: userID, Name: doc.Name, ShipTypeID: doc.ShipTypeID,
		ItemsJson: string(raw), IsPublic: false, IsDraft: true,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		logging.Errorf("fittings fork %d: %v", req.ID, err)
		http.Error(w, "That fit couldn't be copied.", http.StatusInternalServerError)
		return
	}
	writeFitJSON(w, map[string]any{"id": row.ID})
}

// sessionFitStash carries an imported fit (plus its notes)
// across the import redirect into the page render.
const sessionFitStash = "fit_stash"

// handleFitImport serves POST /fittings/import/ (form field
// eft): parse the pasted EFT text, stash the fit, and land back
// on the editor.
func (app *tool) handleFitImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
		return
	}
	doc, notes := app.deps.FitBuilder().ParseEFT(ctx, r.FormValue("eft"))
	stash, _ := json.Marshal(struct {
		Doc   *fitview.FitDoc `json:"doc"`
		Notes []string        `json:"notes"`
	}{Doc: doc, Notes: notes})
	app.deps.Sessions().Put(ctx, sessionFitStash, string(stash))
	http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
}

// handleFitExport serves POST /fittings/export/: the posted fit
// as EFT text.
func (app *tool) handleFitExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := readFitRequest(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(app.deps.FitBuilder().FormatEFT(ctx, &req.FitDoc)))
}

// attachFitEditor fills PageData.FitEditor: the editor chrome,
// the workbench's first paint, and the user's saved-fit list.
// esiFits is the active character's saved EVE fittings (possibly
// nil), used by the per-fit "View stats" deep link.
func (app *tool) attachFitEditor(ctx context.Context, r *http.Request, data *PageData, characters []db.Character, active db.Character, esiFits esi.Fittings) {
	userID := app.deps.UserID(ctx)
	editor := &fitEditorView{}
	doc := &fitview.FitDoc{Charges: map[int64]int64{}}

	q := r.URL.Query()
	localID, _ := strconv.ParseInt(q.Get("local"), 10, 64)
	esiID, _ := strconv.ParseInt(q.Get("esi"), 10, 64)
	publicID, _ := strconv.ParseInt(q.Get("public"), 10, 64)
	doctrineFitID, _ := strconv.ParseInt(q.Get("doctrine"), 10, 64)
	switch {
	case localID > 0 && userID > 0:
		if row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: localID, UserID: userID}); err == nil {
			var stored fitview.FitDoc
			if json.Unmarshal([]byte(row.ItemsJson), &stored) == nil {
				doc = &stored
				editor.LocalID = row.ID
				editor.IsPublic = row.IsPublic
			}
		}
	case esiID > 0:
		for _, f := range esiFits {
			if f.FittingID == esiID {
				doc = app.deps.FitBuilder().FitDocFromESI(ctx, f)
				break
			}
		}
	case publicID > 0:
		// A public fit. The owner's own opens as their saved fit,
		// so saving edits it in place (and rebuilds its public
		// page); anybody else's opens unsaved: saving makes a copy.
		if row, err := app.queries.GetPublicFitting(ctx, publicID); err == nil {
			var stored fitview.FitDoc
			if json.Unmarshal([]byte(row.ItemsJson), &stored) == nil {
				doc = &stored
				if userID > 0 && row.UserID == userID {
					editor.LocalID = row.ID
					editor.IsPublic = true
				}
			}
		}
	case doctrineFitID > 0 && userID > 0:
		// A fit of one of the account's corporations' doctrines, likewise.
		if stored, ok := app.deps.DoctrineFitDoc(ctx, userID, doctrineFitID); ok {
			doc = stored
		}
	default:
		if raw := app.deps.Sessions().GetString(ctx, sessionFitStash); raw != "" {
			app.deps.Sessions().Remove(ctx, sessionFitStash)
			var stash struct {
				Doc   *fitview.FitDoc `json:"doc"`
				Notes []string        `json:"notes"`
			}
			if json.Unmarshal([]byte(raw), &stash) == nil && stash.Doc != nil {
				doc = stash.Doc
				editor.Notes = stash.Notes
			}
		}
	}
	fitview.SanitizeFitDoc(doc)

	pilotID := int64(0)
	pilotLabel := "All V"
	if active.CharacterID != 0 {
		pilotID = active.CharacterID
		pilotLabel = active.Name
	}
	if want, _ := strconv.ParseInt(q.Get("pilot"), 10, 64); want != 0 {
		for _, ch := range characters {
			if ch.CharacterID == want {
				pilotID, pilotLabel = ch.CharacterID, ch.Name
			}
		}
	}
	editor.PilotID = pilotID
	editor.EVECharID = active.CharacterID
	editor.EVECharName = active.Name
	for _, ch := range characters {
		editor.Pilots = append(editor.Pilots, module.CharLink{
			ID: ch.CharacterID, Name: ch.Name, Active: ch.CharacterID == pilotID,
		})
	}
	cloneID, _ := strconv.ParseInt(q.Get("clone"), 10, 64)
	editor.Sim = app.deps.FitBuilder().BuildFitSimView(ctx, doc, pilotID, pilotLabel, cloneID)
	editor.CloneID = editor.Sim.CloneID
	editor.Clones = editor.Sim.Clones

	editor.Description = doc.Description
	editor.TagsList = doc.Tags
	editor.Tags = strings.Join(doc.Tags, ", ")
	// The editor does not embed the your-fits list (the search bar
	// above loads fits through /fittings/mine.json), so the whole fits
	// table is not pulled for this page. LocalFits stays on the view
	// for API compatibility; listLocalFitEntries remains for the
	// dedicated list consumers.
	data.FitEditor = editor
}

// listLocalFitEntries builds the your-fits list: the user's saved
// fits, newest first, with ship names and the description/tags
// stored inside each fit document.
//
//lint:ignore U1000 nothing calls it since the editor stopped embedding the list; kept for the dedicated list consumers (see attachFitEditor's note)
func (app *tool) listLocalFitEntries(ctx context.Context, userID int64) []localFitEntry {
	rows, err := app.queries.ListLocalFittings(ctx, userID)
	if err != nil {
		logging.Errorf("fittings: list local fits: %v", err)
		return nil
	}
	shipIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		shipIDs = append(shipIDs, row.ShipTypeID)
	}
	names := app.deps.CachedTypeNames(ctx, shipIDs)
	entries := make([]localFitEntry, 0, len(rows))
	for _, row := range rows {
		var stored fitview.FitDoc
		_ = json.Unmarshal([]byte(row.ItemsJson), &stored)
		shipName := fmt.Sprintf("Type #%d", row.ShipTypeID)
		if n, ok := names[row.ShipTypeID]; ok {
			shipName = n
		}
		entries = append(entries, localFitEntry{
			ID:          row.ID,
			Name:        row.Name,
			ShipName:    shipName,
			Description: stored.Description,
			Tags:        stored.Tags,
			Updated:     rfc3339(row.UpdatedAt),
			IsPublic:    row.IsPublic,
			IsDraft:     row.IsDraft,
		})
	}
	return entries
}

// rfc3339 is a timestamp as the front end reads it.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
