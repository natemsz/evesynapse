package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// assetCharLink is one entry of the Assets page character switcher.
type assetCharLink struct {
	ID     int64
	Name   string
	Active bool
	Tags   string
}

// assetRow is one item stack in a location table.
type assetRow struct {
	Name     string
	TypeID   int64
	Quantity string // thousands-separated
	Note     string // "BPC", "singleton", or ""
}

// assetLocation is one location block: its resolved title plus the
// largest stacks parked there.
type assetLocation struct {
	Title      string
	Loc        placeRef // Title classified for the link policy
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
		app.render(ctx, w, http.StatusOK, "assets.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("assets: list characters for user %d: %v", userID, err)
		data.Error = "Could not load asset data; check the server log."
		app.render(ctx, w, http.StatusOK, "assets.html", data)
		return
	}
	if len(characters) == 0 {
		app.render(ctx, w, http.StatusOK, "assets.html", data)
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
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want != 0 && pick(want) {
		// An explicit pick becomes the session's acting
		// character (see pickCharacter in character.go).
		putSessionCharID(app.sessions, ctx, active.CharacterID)
		app.sessions.Put(ctx, sessionCharacterName, active.Name)
	} else if sid := sessionCharID(app.sessions, ctx); sid == 0 || !pick(sid) {
		active = characters[0]
	}
	for _, ch := range characters {
		data.AssetsChars = append(data.AssetsChars, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
		})
	}

	// A ?q= search runs across every linked character instead of
	// rendering one character's hangars.
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		data.AssetsSearch = app.searchAssets(ctx, characters, q)
		app.render(ctx, w, http.StatusOK, "assets.html", data)
		return
	}

	view := &assetsView{CharacterName: active.Name}
	data.Assets = view

	var items []esi.Asset
	if err := app.esi.GetCached(ctx, active, esi.SnapAssets, &items); err != nil {
		logging.Errorf("assets: load for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapAssets}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(ctx, w, http.StatusOK, "assets.html", data)
		return
	}

	view.Loaded = true
	view.Stacks = len(items)
	for _, it := range items {
		view.TotalItems += it.Quantity
	}
	view.Locations = app.buildAssetLocations(ctx, items)

	app.render(ctx, w, http.StatusOK, "assets.html", data)
}

// buildAssetLocations groups asset stacks by location, resolves
// type and location names, and sorts biggest-first at both levels.
func (app *Application) buildAssetLocations(ctx context.Context, items []esi.Asset) []assetLocation {
	return app.buildAssetLocationsWith(ctx, items, nil, nil)
}

// buildAssetLocationsWith is buildAssetLocations with two
// corporation-cluster additions, both nil for the character page
// (byte-identical behavior there):
//   - extraTitles names locations from outside the place cache —
//     the corp's own structures, which the corp assets endpoint
//     reports as location_type "item"/"other" with no structure
//     label, so the structure IDs must be recognised explicitly;
//   - nameOverrides replaces type names with player-given item
//     names for singletons (named ships, renamed containers).
func (app *Application) buildAssetLocationsWith(ctx context.Context, items []esi.Asset, extraTitles map[int64]string, nameOverrides map[int64]string) []assetLocation {
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
	// Types no local tier can name yet become current-page wants
	// on a page render (a no-op in worker contexts).
	{
		seen := make(map[int64]bool, len(typeIDs))
		for _, id := range typeIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if _, ok := names[id]; !ok {
				app.notePageWantFromContext(ctx, pageWantTypeDescription, id)
			}
		}
	}
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
	placeMemo := make(map[int64]placeRef)
	for locID, entries := range byLoc {
		loc := assetLocation{Title: app.assetLocationTitle(ctx, locID, locType[locID], itemType, nameOf, extraTitles)}
		loc.Loc = app.linkPlaceMemo(ctx, placeMemo, locID, loc.Title)

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
			name := nameOf(it.TypeID)
			if override, ok := nameOverrides[it.ItemID]; ok && override != "" {
				name = override
			}
			loc.Items = append(loc.Items, assetRow{
				Name:     name,
				TypeID:   it.TypeID,
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
// cache); player structures show the worker-resolved name once it
// lands (structures.go), and otherwise stay honest "Structure
// #<id>" unless extraTitles names them
// (the corporation cluster passes the corp's own structures);
// items inside another owned item (a ship, a container) are
// labelled with the parent's type name.
func (app *Application) assetLocationTitle(ctx context.Context, locID int64, locType string, itemType map[int64]int64, nameOf func(int64) string, extraTitles map[int64]string) string {
	if title, ok := extraTitles[locID]; ok {
		return title
	}
	switch locType {
	case "station":
		if name, ok := app.esi.CachedPlaceName(ctx, locID); ok {
			return name
		}
		app.notePageWantFromContext(ctx, pageWantPlace, locID)
		return fmt.Sprintf("Station #%d", locID)
	case "solar_system":
		if name, ok := app.esi.CachedPlaceName(ctx, locID); ok {
			return name
		}
		app.notePageWantFromContext(ctx, pageWantPlace, locID)
		return fmt.Sprintf("System #%d", locID)
	case "structure":
		if name := app.resolvedStructureTitle(ctx, locID); name != "" {
			return name
		}
		app.notePageWantFromContext(ctx, pageWantStructure, locID)
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

// assetSearchChar is one character's slice of a cross-character
// search: the location blocks holding that character's matching
// stacks.
type assetSearchChar struct {
	CharacterID   int64
	CharacterName string
	Locations     []assetLocation
	Stacks        int // matching stacks across all locations
}

// assetsSearchView is the Assets page's cross-character search
// result. Syncing names the characters whose first asset sync
// has not landed yet — they are reported as not included, never
// silently absent from the answer.
type assetsSearchView struct {
	Query       string
	Results     []assetSearchChar
	TotalStacks int
	Syncing     []string
}

// searchAssets answers "which of my characters has an X, and
// where is it": it scans every linked character's cached asset
// snapshot for stacks whose item name contains the query
// (case-insensitive) and groups the matches by character and
// location. It reads snapshot rows directly and never fetches —
// a character whose first sync has not landed is reported as
// not included rather than fetched on the spot.
func (app *Application) searchAssets(ctx context.Context, characters []db.Character, query string) *assetsSearchView {
	view := &assetsSearchView{Query: query}
	needle := strings.ToLower(query)
	for _, ch := range characters {
		snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapAssets})
		if errors.Is(serr, sql.ErrNoRows) {
			view.Syncing = append(view.Syncing, ch.Name)
			continue
		}
		if serr != nil {
			logging.Errorf("assets search: read snapshot for character %d: %v", ch.CharacterID, serr)
			continue
		}
		var items []esi.Asset
		if err := json.Unmarshal([]byte(snap.Payload), &items); err != nil {
			logging.Errorf("assets search: decode snapshot for character %d: %v", ch.CharacterID, err)
			continue
		}
		found := app.searchCharacterAssets(ctx, items, needle)
		if found == nil {
			continue
		}
		found.CharacterID = ch.CharacterID
		found.CharacterName = ch.Name
		view.Results = append(view.Results, *found)
		view.TotalStacks += found.Stacks
	}
	sort.Slice(view.Results, func(i, j int) bool {
		if view.Results[i].Stacks != view.Results[j].Stacks {
			return view.Results[i].Stacks > view.Results[j].Stacks
		}
		return view.Results[i].CharacterName < view.Results[j].CharacterName
	})
	return view
}

// searchCharacterAssets groups one character's stacks whose item
// name contains needle by location — same titles, same ordering,
// same per-location cap as the full Assets page, so a search
// result reads like the page it points to. Returns nil when
// nothing matches. Name resolution covers the character's whole
// hangar (not just the matches) so "Inside: <container>" labels
// resolve the same way they do on the Assets page.
func (app *Application) searchCharacterAssets(ctx context.Context, items []esi.Asset, needle string) *assetSearchChar {
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
	stacks := 0
	for _, it := range items {
		if !strings.Contains(strings.ToLower(nameOf(it.TypeID)), needle) {
			continue
		}
		byLoc[it.LocationID] = append(byLoc[it.LocationID], it)
		if _, ok := locType[it.LocationID]; !ok {
			locType[it.LocationID] = it.LocationType
		}
		stacks++
	}
	if stacks == 0 {
		return nil
	}

	locations := make([]assetLocation, 0, len(byLoc))
	placeMemo := make(map[int64]placeRef)
	for locID, entries := range byLoc {
		loc := assetLocation{Title: app.assetLocationTitle(ctx, locID, locType[locID], itemType, nameOf, nil)}
		loc.Loc = app.linkPlaceMemo(ctx, placeMemo, locID, loc.Title)
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
				TypeID:   it.TypeID,
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
	return &assetSearchChar{Locations: locations, Stacks: stacks}
}
