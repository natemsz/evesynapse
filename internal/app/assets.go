package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// assetCharLink is one entry of the Assets page character switcher.
type assetCharLink struct {
	ID     int64
	Name   string
	Active bool
}

// assetRow is one item stack in a location table.
type assetRow struct {
	Name     string
	Quantity string // thousands-separated
	Note     string // "BPC", "singleton", or ""
}

// assetLocation is one location block: its resolved title plus the
// largest stacks parked there.
type assetLocation struct {
	Title      string
	Items      []assetRow
	MoreStacks int // stacks hidden past the per-location cap
}

// assetsView is the Assets page body for one character. Loaded is
// false when the snapshot could not be produced at all; Warming
// marks the cold-start case (no snapshot yet — the worker is still
// importing), which gets friendlier copy than a hard failure.
type assetsView struct {
	CharacterName string
	Loaded        bool
	Warming       bool
	Stacks        int
	TotalItems    int64
	Locations     []assetLocation
}

// maxAssetRowsPerLocation caps each location table so a hangar with
// thousands of stacks stays a page, not a scroll marathon.
const maxAssetRowsPerLocation = 25

// handleAssets renders the Assets page for one of the signed-in
// user's characters (switchable via ?character=). Asset data comes
// through the snapshot cache, and every name is resolved from local
// caches only — the worker pre-warms snapshots and names, so this
// page never waits on ESI name lookups.
func (app *Application) handleAssets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		app.render(w, http.StatusOK, "assets.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("assets: list characters for user %d: %v", userID, err)
		data.Error = "Could not load asset data; check the server log."
		app.render(w, http.StatusOK, "assets.html", data)
		return
	}
	if len(characters) == 0 {
		app.render(w, http.StatusOK, "assets.html", data)
		return
	}

	// Active character: an explicit ?character= the user owns wins,
	// then the session character, then the first linked character.
	active := characters[0]
	pick := func(id int64) bool {
		for _, ch := range characters {
			if ch.CharacterID == id {
				active = ch
				return true
			}
		}
		return false
	}
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want == 0 || !pick(want) {
		if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid == 0 || !pick(sid) {
			active = characters[0]
		}
	}
	for _, ch := range characters {
		data.AssetsChars = append(data.AssetsChars, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
		})
	}

	view := &assetsView{CharacterName: active.Name}
	data.Assets = view

	var items []esi.Asset
	if err := app.esi.GetCached(ctx, active, esi.SnapAssets, &items); err != nil {
		log.Printf("assets: load for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapAssets}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(w, http.StatusOK, "assets.html", data)
		return
	}

	view.Loaded = true
	view.Stacks = len(items)
	for _, it := range items {
		view.TotalItems += it.Quantity
	}
	view.Locations = app.buildAssetLocations(ctx, items)

	app.render(w, http.StatusOK, "assets.html", data)
}

// buildAssetLocations groups asset stacks by location, resolves
// type and location names, and sorts biggest-first at both levels.
func (app *Application) buildAssetLocations(ctx context.Context, items []esi.Asset) []assetLocation {
	// One name-resolution pass covers item types AND the parent
	// items other items live inside (their type IDs are in the same
	// payload). Cache-only: unresolved names render as "Type #<id>"
	// and fill in as the worker's warm-up pass resolves them.
	typeIDs := make([]int64, 0, len(items))
	itemType := make(map[int64]int64, len(items))
	for _, it := range items {
		typeIDs = append(typeIDs, it.TypeID)
		itemType[it.ItemID] = it.TypeID
	}
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok {
			return n
		}
		return fmt.Sprintf("Type #%d", typeID)
	}

	byLoc := make(map[int64][]esi.Asset)
	locType := make(map[int64]string)
	for _, it := range items {
		byLoc[it.LocationID] = append(byLoc[it.LocationID], it)
		if _, ok := locType[it.LocationID]; !ok {
			locType[it.LocationID] = it.LocationType
		}
	}

	locations := make([]assetLocation, 0, len(byLoc))
	for locID, entries := range byLoc {
		loc := assetLocation{Title: app.assetLocationTitle(ctx, locID, locType[locID], itemType, nameOf)}

		sorted := append([]esi.Asset(nil), entries...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Quantity != sorted[j].Quantity {
				return sorted[i].Quantity > sorted[j].Quantity
			}
			return nameOf(sorted[i].TypeID) < nameOf(sorted[j].TypeID)
		})
		if len(sorted) > maxAssetRowsPerLocation {
			loc.MoreStacks = len(sorted) - maxAssetRowsPerLocation
			sorted = sorted[:maxAssetRowsPerLocation]
		}
		for _, it := range sorted {
			loc.Items = append(loc.Items, assetRow{
				Name:     nameOf(it.TypeID),
				Quantity: esi.FormatInt(it.Quantity),
				Note:     assetNote(it),
			})
		}
		locations = append(locations, loc)
	}

	sort.Slice(locations, func(i, j int) bool {
		ti := len(locations[i].Items) + locations[i].MoreStacks
		tj := len(locations[j].Items) + locations[j].MoreStacks
		if ti != tj {
			return ti > tj
		}
		return locations[i].Title < locations[j].Title
	})
	return locations
}

// assetLocationTitle turns a (location_id, location_type) pair into
// a display title. Station and solar-system names come from the
// local caches (SDE tables first, then the worker-warmed place
// cache); player structures
// cannot be named without an ESI scope this app does not hold, so
// they stay honest "Structure #<id>"; items inside another owned
// item (a ship, a container) are labelled with the parent's type
// name.
func (app *Application) assetLocationTitle(ctx context.Context, locID int64, locType string, itemType map[int64]int64, nameOf func(int64) string) string {
	switch locType {
	case "station":
		if name, ok := app.esi.CachedPlaceName(ctx, locID); ok {
			return name
		}
		return fmt.Sprintf("Station #%d", locID)
	case "solar_system":
		if name, ok := app.esi.CachedPlaceName(ctx, locID); ok {
			return name
		}
		return fmt.Sprintf("System #%d", locID)
	case "structure":
		return fmt.Sprintf("Structure #%d", locID)
	default: // "other", "item", anything unexpected
		if parentType, ok := itemType[locID]; ok {
			return "Inside: " + nameOf(parentType)
		}
		return "In space / other"
	}
}

// assetNote labels the special stack states worth a glance: blueprint
// copies and one-off singletons (fitted ships, assembled hulls).
func assetNote(it esi.Asset) string {
	switch {
	case it.IsBlueprintCopy:
		return "BPC"
	case it.IsSingleton && it.Quantity == 1:
		return "singleton"
	}
	return ""
}
