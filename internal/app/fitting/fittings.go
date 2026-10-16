package fitting

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Fittings page: saved ship fittings for one character, items
// grouped by slot category. Data comes from the fittings snapshot;
// every name resolves from the local caches only.
// ---------------------------------------------------------------------------

// fittingEntry is one saved fitting: ship, name, grouped items.
// Source says where the fit lives: "local" for the user's
// EveSynapse fits (LocalID set, IsPublic marking the published
// ones) or "eve" for a character's in-game fittings (FittingID
// set).
type fittingEntry struct {
	FittingID  int64
	LocalID    int64
	Name       string
	ShipType   string
	ShipTypeID int64
	Groups     []fitview.FittingGroup
	Source     string
	IsPublic   bool
}

// fittingsView is the Fittings page body: the user's EveSynapse
// fits and the active character's EVE fits in one list, with a
// source filter (Source: all, public, evesynapse or eve) and the
// per-source counts the filter chips carry. Loaded and Warming
// describe the EVE snapshot only.
type fittingsView struct {
	CharacterID   int64
	CharacterName string
	Loaded        bool
	Warming       bool
	Source        string
	CountAll      int
	CountPublic   int
	CountLocal    int
	CountEVE      int
	Fittings      []fittingEntry
}

// fittingSlotCategories maps ESI item flags to display groups, in
// display order. Flags are matched by prefix ("HiSlot0".."HiSlot7"
// and friends); anything unlisted lands in "Other".
var fittingSlotCategories = []struct {
	Prefix string
	Label  string
}{
	{"HiSlot", "High slots"},
	{"MedSlot", "Medium slots"},
	{"LoSlot", "Low slots"},
	{"RigSlot", "Rig slots"},
	{"SubSystemSlot", "Subsystem slots"},
	{"DroneBay", "Drone bay"},
	{"Cargo", "Cargo hold"},
	{"FuelBay", "Fuel bay"},
}

// fittingSlotCategory returns the display group for an ESI fitting
// flag ("HiSlot3" → "High slots", "Implant" → "Other", ...).
func fittingSlotCategory(flag string) string {
	for _, cat := range fittingSlotCategories {
		if strings.HasPrefix(flag, cat.Prefix) {
			return cat.Label
		}
	}
	return "Other"
}

// handleFittings renders the fit simulator page: one of the
// signed-in user's characters (switchable via ?character=), or
// the anonymous All-V view without a session. The saved-fits
// list lives on its own page (/fittings/saved/).
func (app *tool) handleFittings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	data.HeadMeta = app.deps.HeadMeta(module.PageMeta{Title: "Ship Workbench", Description: "Fit EVE Online ships in the browser workbench: simulated stats, saved fittings and a priced shopping list for the whole fit.", CanonicalPath: "/fittings/"})

	characters, active, links, err := app.deps.PickCharacter(ctx, r, "/fittings/")
	if err != nil {
		logging.Errorf("fittings: list characters: %v", err)
		data.Error = "Could not load fitting data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "fittings.html", data)
		return
	}
	if links == nil {
		app.attachFitEditor(ctx, r, data, nil, db.Character{}, nil)
		if data.FitEditor != nil {
			data.FitEditor.Anonymous = app.deps.UserID(ctx) == 0
		}
		app.deps.Render(ctx, w, http.StatusOK, "fittings.html", data)
		return
	}
	data.FittingsChars = links

	// The simulator resolves the ?esi= "View stats" deep link
	// against the character's EVE fittings; a missing snapshot
	// just means the link won't resolve.
	var fittings esi.Fittings
	if err := app.deps.StoredSnapshot(ctx, active, esi.SnapFittings, &fittings); err != nil {
		logging.Errorf("fittings: load for character %d: %v", active.CharacterID, err)
	}
	app.attachFitEditor(ctx, r, data, characters, active, fittings)
	app.deps.Render(ctx, w, http.StatusOK, "fittings.html", data)
}

// handleFittingsSaved renders the "My Loadouts" page: the user's
// EveSynapse fits and the active character's EVE fittings in one
// list, items grouped by slot category, with a ?source= filter
// (public, evesynapse or eve; anything else is all). EveSynapse
// fits are the user's own (ListLocalFittings, newest first; the
// autosave draft is workbench scratch and stays off the page).
// Data comes from stored rows and the fittings snapshot; every
// name resolves from the local caches only.
func (app *tool) handleFittingsSaved(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	source := r.URL.Query().Get("source")
	switch source {
	case "public", "evesynapse", "eve":
	default:
		source = "all"
	}
	view := &fittingsView{Source: source}
	data.Fittings = view

	// The user's EveSynapse fits.
	var localEntries []fittingEntry
	if userID := app.deps.UserID(ctx); userID > 0 {
		rows, err := app.queries.ListLocalFittings(ctx, userID)
		if err != nil {
			logging.Errorf("fittings: list local for user %d: %v", userID, err)
		} else {
			localEntries = app.buildLocalFittingEntries(ctx, rows)
		}
	}
	for _, e := range localEntries {
		if e.IsPublic {
			view.CountPublic++
		}
	}
	view.CountLocal = len(localEntries)

	// The active character's EVE fittings.
	var eveEntries []fittingEntry
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/fittings/saved/")
	if err != nil {
		logging.Errorf("fittings: list characters: %v", err)
		data.Error = "Could not load fitting data; check the server log."
	} else if links != nil {
		data.FittingsChars = links
		view.CharacterID = active.CharacterID
		view.CharacterName = active.Name

		var fittings esi.Fittings
		if err := app.deps.StoredSnapshot(ctx, active, esi.SnapFittings, &fittings); err != nil {
			logging.Errorf("fittings: load for character %d: %v", active.CharacterID, err)
			// No snapshot row at all = cold start: the worker is still
			// importing this character, which the Sync page shows live.
			if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapFittings}); errors.Is(serr, sql.ErrNoRows) {
				view.Warming = true
			}
		} else {
			view.Loaded = true
			eveEntries = app.buildFittingEntries(ctx, fittings)
		}
	}
	view.CountEVE = len(eveEntries)
	view.CountAll = view.CountLocal + view.CountEVE

	switch source {
	case "public":
		for _, e := range localEntries {
			if e.IsPublic {
				view.Fittings = append(view.Fittings, e)
			}
		}
	case "evesynapse":
		view.Fittings = localEntries
	case "eve":
		view.Fittings = eveEntries
	default:
		view.Fittings = append(localEntries, eveEntries...)
	}
	app.deps.Render(ctx, w, http.StatusOK, "fittings_saved.html", data)
}

// buildLocalFittingEntries turns the user's local fitting rows
// into page entries: names from one cache pass, slot families
// from one SDE snapshot over the union of every fit's types, so
// the grouping matches the public fit page's sections. Drafts
// are skipped (the workbench's scratch row, not a loadout).
func (app *tool) buildLocalFittingEntries(ctx context.Context, rows []db.ListLocalFittingsRow) []fittingEntry {
	docs := make(map[int64]*fitview.FitDoc, len(rows))
	typeIDs := make([]int64, 0, len(rows)*8)
	for _, row := range rows {
		if row.IsDraft {
			continue
		}
		var doc fitview.FitDoc
		if err := json.Unmarshal([]byte(row.ItemsJson), &doc); err != nil {
			continue
		}
		stored := doc
		docs[row.ID] = &stored
		typeIDs = append(typeIDs, row.ShipTypeID)
		for _, it := range doc.Items {
			typeIDs = append(typeIDs, it.TypeID)
		}
	}
	names := app.deps.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok && n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", typeID)
	}
	familyOf := map[int64]string{}
	if snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs); err == nil && snap != nil {
		for _, id := range typeIDs {
			familyOf[id] = fitview.FitSlotFamilyOf(snap, id)
		}
	}

	entries := make([]fittingEntry, 0, len(docs))
	for _, row := range rows {
		doc, ok := docs[row.ID]
		if !ok {
			continue
		}
		entries = append(entries, fittingEntry{
			LocalID: row.ID, Name: row.Name,
			ShipType: nameOf(row.ShipTypeID), ShipTypeID: row.ShipTypeID,
			Groups: localFitGroups(doc, familyOf, nameOf),
			Source: "local", IsPublic: row.IsPublic,
		})
	}
	return entries
}

// localFitGroups groups one local fit document's items by slot
// family in the public sections' display order; charges ride
// with their weapon as a plain item line, as on the public page.
func localFitGroups(doc *fitview.FitDoc, familyOf map[int64]string, nameOf func(int64) string) []fitview.FittingGroup {
	familyLabel := map[string]string{
		fitview.FitFamilyHigh: "High slots", fitview.FitFamilyMedium: "Medium slots",
		fitview.FitFamilyLow: "Low slots", fitview.FitFamilyRig: "Rig slots",
		fitview.FitFamilySubsystem: "Subsystem slots", fitview.FitFamilyDrone: "Drone bay",
		fitview.FitFamilyCargo: "Cargo hold",
	}
	byFamily := map[string][]fitview.FittingItemRow{}
	for _, it := range doc.Items {
		family := familyOf[it.TypeID]
		if _, ok := familyLabel[family]; !ok {
			family = "other"
		}
		byFamily[family] = append(byFamily[family], fitview.FittingItemRow{
			Name: nameOf(it.TypeID), TypeID: it.TypeID,
			Quantity: fmt.Sprintf("%d", it.Qty),
		})
	}
	ordered := []string{fitview.FitFamilyHigh, fitview.FitFamilyMedium, fitview.FitFamilyLow,
		fitview.FitFamilyRig, fitview.FitFamilySubsystem, fitview.FitFamilyDrone, fitview.FitFamilyCargo, "other"}
	var groups []fitview.FittingGroup
	for _, family := range ordered {
		items, ok := byFamily[family]
		if !ok {
			continue
		}
		label := familyLabel[family]
		if family == "other" {
			label = "Other"
		}
		groups = append(groups, fitview.FittingGroup{Label: label, Items: items})
	}
	return groups
}

// buildFittingEntries groups one character's EVE fittings by slot
// category, resolving every ship and item name from the local
// caches. One cache-only name pass covers the whole list.
func (app *tool) buildFittingEntries(ctx context.Context, fittings esi.Fittings) []fittingEntry {
	typeIDs := make([]int64, 0, len(fittings))
	for _, f := range fittings {
		typeIDs = append(typeIDs, f.ShipTypeID)
		for _, it := range f.Items {
			typeIDs = append(typeIDs, it.TypeID)
		}
	}
	names := app.deps.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok {
			return n
		}
		return fmt.Sprintf("Type #%d", typeID)
	}

	entries := make([]fittingEntry, 0, len(fittings))
	for _, f := range fittings {
		entry := fittingEntry{FittingID: f.FittingID, Name: f.Name, ShipType: nameOf(f.ShipTypeID), ShipTypeID: f.ShipTypeID, Source: "eve"}

		byLabel := make(map[string][]fitview.FittingItemRow)
		for _, it := range f.Items {
			label := fittingSlotCategory(it.Flag)
			byLabel[label] = append(byLabel[label], fitview.FittingItemRow{
				Name:     nameOf(it.TypeID),
				TypeID:   it.TypeID,
				Quantity: esi.FormatInt(it.Quantity),
			})
		}
		for _, cat := range fittingSlotCategories {
			items, ok := byLabel[cat.Label]
			if !ok {
				continue
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
			entry.Groups = append(entry.Groups, fitview.FittingGroup{Label: cat.Label, Items: items})
		}
		if items, ok := byLabel["Other"]; ok {
			sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
			entry.Groups = append(entry.Groups, fitview.FittingGroup{Label: "Other", Items: items})
		}
		entries = append(entries, entry)
	}
	return entries
}
