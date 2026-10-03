package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Fittings page: saved ship fittings for one character, items
// grouped by slot category. Data comes from the fittings snapshot;
// every name resolves from the local caches only.
// ---------------------------------------------------------------------------

// fittingItemRow is one fitted item line.
type fittingItemRow struct {
	Name     string
	TypeID   int64
	Quantity string // thousands-separated
}

// fittingGroup is one slot-category block of a fitting.
type fittingGroup struct {
	Label string
	Items []fittingItemRow
}

// fittingEntry is one saved fitting: ship, name, grouped items.
type fittingEntry struct {
	FittingID  int64
	Name       string
	ShipType   string
	ShipTypeID int64
	Groups     []fittingGroup
}

// fittingsView is the Fittings page body for one character.
type fittingsView struct {
	CharacterID   int64
	CharacterName string
	Loaded        bool
	Warming       bool
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

// handleFittings renders the Fittings page for one of the signed-in
// user's characters (switchable via ?character=).
func (app *Application) handleFittings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/fittings/")
	if err != nil {
		log.Printf("fittings: list characters: %v", err)
		data.Error = "Could not load fitting data; check the server log."
		app.render(ctx, w, http.StatusOK, "fittings.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "fittings.html", data)
		return
	}
	data.FittingsChars = links

	view := &fittingsView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.Fittings = view

	var fittings esi.Fittings
	if err := app.esi.GetCached(ctx, active, esi.SnapFittings, &fittings); err != nil {
		log.Printf("fittings: load for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapFittings}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(ctx, w, http.StatusOK, "fittings.html", data)
		return
	}
	view.Loaded = true

	// One cache-only name pass covers ship types and every item.
	typeIDs := make([]int64, 0, len(fittings))
	for _, f := range fittings {
		typeIDs = append(typeIDs, f.ShipTypeID)
		for _, it := range f.Items {
			typeIDs = append(typeIDs, it.TypeID)
		}
	}
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok {
			return n
		}
		return fmt.Sprintf("Type #%d", typeID)
	}

	for _, f := range fittings {
		entry := fittingEntry{FittingID: f.FittingID, Name: f.Name, ShipType: nameOf(f.ShipTypeID), ShipTypeID: f.ShipTypeID}

		byLabel := make(map[string][]fittingItemRow)
		for _, it := range f.Items {
			label := fittingSlotCategory(it.Flag)
			byLabel[label] = append(byLabel[label], fittingItemRow{
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
			entry.Groups = append(entry.Groups, fittingGroup{Label: cat.Label, Items: items})
		}
		if items, ok := byLabel["Other"]; ok {
			sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
			entry.Groups = append(entry.Groups, fittingGroup{Label: "Other", Items: items})
		}
		view.Fittings = append(view.Fittings, entry)
	}

	app.render(ctx, w, http.StatusOK, "fittings.html", data)
}
