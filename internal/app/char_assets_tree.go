package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Assets, nested. ESI lists every asset flat, with the id of where it
// is: a station, a structure, a system, or another asset (the ship or
// container it is inside).
//
// Here every asset is filed under the place its outermost container
// is in. The Loki is a row of the station's block, and what is inside
// it opens beneath its row. Containers inside ships nest the same way.
// ---------------------------------------------------------------------------

// assetNestMax bounds how deep containment is followed. EVE's own
// nesting stops well short of it; the bound is what makes a damaged
// or looping snapshot harmless.
const assetNestMax = 12

// assetIndex is one character's assets arranged by containment.
type assetIndex struct {
	byID     map[int64]esi.Asset
	children map[int64][]esi.Asset // an asset's id -> the assets directly inside it
	names    map[int64]string      // type id -> name, as far as local data knows
	given    map[int64]string      // an asset's id -> the name its owner gave it (ships, containers)
}

func (app *Application) newAssetIndex(ctx context.Context, items []esi.Asset) *assetIndex {
	ix := &assetIndex{
		byID:     make(map[int64]esi.Asset, len(items)),
		children: map[int64][]esi.Asset{},
	}
	typeIDs := make([]int64, 0, len(items))
	for _, it := range items {
		ix.byID[it.ItemID] = it
		typeIDs = append(typeIDs, it.TypeID)
	}
	for _, it := range items {
		if _, inside := ix.byID[it.LocationID]; inside && it.LocationID != it.ItemID {
			ix.children[it.LocationID] = append(ix.children[it.LocationID], it)
		}
	}
	ix.names = app.esi.CachedTypeNames(ctx, typeIDs)
	// Player-given names, where the worker has fetched them
	// (char_assets_names_worker.go). Only singletons can have one.
	ix.given = map[int64]string{}
	var singles []int64
	for _, it := range items {
		if it.IsSingleton {
			singles = append(singles, it.ItemID)
		}
	}
	if len(singles) > 0 {
		if rows, err := app.queries.ListItemNamesByIDs(ctx, singles); err != nil {
			logging.Errorf("assets: list item names: %v", err)
		} else {
			for _, row := range rows {
				if row.Name != "" {
					ix.given[row.ItemID] = row.Name
				}
			}
		}
	}
	// Types no local tier can name yet become current-page wants on a
	// page render (a no-op elsewhere).
	seen := make(map[int64]bool, len(typeIDs))
	for _, id := range typeIDs {
		if !seen[id] {
			seen[id] = true
			if _, ok := ix.names[id]; !ok {
				app.notePageWantFromContext(ctx, pageWantTypeDescription, id)
			}
		}
	}
	return ix
}

func (ix *assetIndex) nameOf(typeID int64) string {
	if n, ok := ix.names[typeID]; ok {
		return n
	}
	return fmt.Sprintf("Type #%d", typeID)
}

// label is how an asset is named on the page: the name its owner gave
// it when there is one (with its type beside it), else its type.
func (ix *assetIndex) label(it esi.Asset) (name, typeName string) {
	if given := ix.given[it.ItemID]; given != "" {
		return given, ix.nameOf(it.TypeID)
	}
	return ix.nameOf(it.TypeID), ""
}

// topLevel reports whether an asset sits directly in a place, not
// inside another asset the character owns.
func (ix *assetIndex) topLevel(it esi.Asset) bool {
	_, inside := ix.byID[it.LocationID]
	return !inside || it.LocationID == it.ItemID
}

// place finds where an asset really is: the location of its
// outermost container. containers lists what it is inside, outermost
// first (empty for a top-level asset).
func (ix *assetIndex) place(it esi.Asset) (locID int64, locType string, containers []esi.Asset) {
	cur := it
	for depth := 0; depth < assetNestMax; depth++ {
		parent, inside := ix.byID[cur.LocationID]
		if !inside || parent.ItemID == cur.ItemID {
			break
		}
		containers = append([]esi.Asset{parent}, containers...)
		cur = parent
	}
	return cur.LocationID, cur.LocationType, containers
}

// inside counts every stack within an asset, at any depth.
func (ix *assetIndex) inside(it esi.Asset, depth int) int {
	if depth >= assetNestMax {
		return 0
	}
	n := 0
	for _, child := range ix.children[it.ItemID] {
		n += 1 + ix.inside(child, depth+1)
	}
	return n
}

// sortAssets orders a level: what holds other things first (the
// fullest leading), then the biggest stacks, then by name. A ship is
// a stack of one and would otherwise sink below every pile of ore.
func (ix *assetIndex) sortAssets(list []esi.Asset) []esi.Asset {
	sorted := append([]esi.Asset(nil), list...)
	held := make(map[int64]int, len(sorted))
	for _, it := range sorted {
		held[it.ItemID] = ix.inside(it, 0)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if held[a.ItemID] != held[b.ItemID] {
			return held[a.ItemID] > held[b.ItemID]
		}
		if a.Quantity != b.Quantity {
			return a.Quantity > b.Quantity
		}
		if na, nb := ix.nameOf(a.TypeID), ix.nameOf(b.TypeID); na != nb {
			return na < nb
		}
		return a.ItemID < b.ItemID
	})
	return sorted
}

// rows builds the display rows for one level and, beneath each row,
// whatever it contains. Each level is capped like the page always
// was; more reports how many stacks at this level were left out.
func (ix *assetIndex) rows(list []esi.Asset, depth int) (rows []assetRow, more int) {
	sorted := ix.sortAssets(list)
	if len(sorted) > maxAssetRowsPerLocation {
		more = len(sorted) - maxAssetRowsPerLocation
		sorted = sorted[:maxAssetRowsPerLocation]
	}
	for _, it := range sorted {
		row := assetRow{
			TypeID:   it.TypeID,
			Quantity: esi.FormatInt(it.Quantity),
			Note:     assetNote(it),
		}
		row.Name, row.TypeName = ix.label(it)
		if kids := ix.children[it.ItemID]; len(kids) > 0 && depth+1 < assetNestMax {
			row.Inside = ix.inside(it, depth)
			row.Children, row.MoreInside = ix.rows(kids, depth+1)
		}
		rows = append(rows, row)
	}
	return rows, more
}

// buildNestedAssetLocations is the character Assets page's layout:
// one block per place, each asset under the place its outermost
// container is in, contents nested beneath their container.
func (app *Application) buildNestedAssetLocations(ctx context.Context, items []esi.Asset) []assetLocation {
	ix := app.newAssetIndex(ctx, items)

	byLoc := map[int64][]esi.Asset{}
	locType := map[int64]string{}
	for _, it := range items {
		if !ix.topLevel(it) {
			continue
		}
		byLoc[it.LocationID] = append(byLoc[it.LocationID], it)
		if _, ok := locType[it.LocationID]; !ok {
			locType[it.LocationID] = it.LocationType
		}
	}

	locations := make([]assetLocation, 0, len(byLoc))
	placeMemo := make(map[int64]placeRef)
	for locID, top := range byLoc {
		loc := assetLocation{Title: app.assetLocationTitle(ctx, locID, locType[locID], nil, ix.nameOf, nil)}
		loc.Loc = app.linkPlaceMemo(ctx, placeMemo, locID, loc.Title)
		loc.Items, loc.MoreStacks = ix.rows(top, 0)
		loc.Stacks = len(top)
		for _, it := range top {
			loc.Stacks += ix.inside(it, 0)
		}
		locations = append(locations, loc)
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i].Stacks != locations[j].Stacks {
			return locations[i].Stacks > locations[j].Stacks
		}
		return locations[i].Title < locations[j].Title
	})
	return locations
}

// searchNestedAssets is one character's part of a search: the stacks
// whose name contains needle, each filed under the place it really is
// in, with what it is inside named beside it ("Loki" for cargo in a
// docked Loki). Nil when nothing matches.
func (app *Application) searchNestedAssets(ctx context.Context, items []esi.Asset, needle string) *assetSearchChar {
	ix := app.newAssetIndex(ctx, items)

	type found struct {
		it    esi.Asset
		where string
	}
	byLoc := map[int64][]found{}
	locType := map[int64]string{}
	stacks := 0
	for _, it := range items {
		// A hit is a match on what the thing is or on what its owner
		// called it.
		if !strings.Contains(strings.ToLower(ix.nameOf(it.TypeID)), needle) &&
			!strings.Contains(strings.ToLower(ix.given[it.ItemID]), needle) {
			continue
		}
		locID, kind, containers := ix.place(it)
		names := make([]string, 0, len(containers))
		for _, c := range containers {
			name, _ := ix.label(c)
			names = append(names, name)
		}
		byLoc[locID] = append(byLoc[locID], found{it: it, where: strings.Join(names, " › ")})
		if _, ok := locType[locID]; !ok {
			locType[locID] = kind
		}
		stacks++
	}
	if stacks == 0 {
		return nil
	}

	locations := make([]assetLocation, 0, len(byLoc))
	placeMemo := make(map[int64]placeRef)
	for locID, hits := range byLoc {
		loc := assetLocation{Title: app.assetLocationTitle(ctx, locID, locType[locID], nil, ix.nameOf, nil), Stacks: len(hits)}
		loc.Loc = app.linkPlaceMemo(ctx, placeMemo, locID, loc.Title)
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].it.Quantity != hits[j].it.Quantity {
				return hits[i].it.Quantity > hits[j].it.Quantity
			}
			if na, nb := ix.nameOf(hits[i].it.TypeID), ix.nameOf(hits[j].it.TypeID); na != nb {
				return na < nb
			}
			return hits[i].where < hits[j].where
		})
		if len(hits) > maxAssetRowsPerLocation {
			loc.MoreStacks = len(hits) - maxAssetRowsPerLocation
			hits = hits[:maxAssetRowsPerLocation]
		}
		for _, h := range hits {
			row := assetRow{
				TypeID:   h.it.TypeID,
				Quantity: esi.FormatInt(h.it.Quantity),
				Note:     assetNote(h.it),
				Where:    h.where,
			}
			row.Name, row.TypeName = ix.label(h.it)
			loc.Items = append(loc.Items, row)
		}
		locations = append(locations, loc)
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i].Stacks != locations[j].Stacks {
			return locations[i].Stacks > locations[j].Stacks
		}
		return locations[i].Title < locations[j].Title
	})
	return &assetSearchChar{Locations: locations, Stacks: stacks}
}

// ---------------------------------------------------------------------------
// Suggestions for the search box: the items the account actually
// owns, each with whose it is.
// ---------------------------------------------------------------------------

// assetSuggestLimit is how many suggestions the box shows.
const assetSuggestLimit = 10

// characterAssets reads one character's stored asset list. ok is
// false when there is none yet or it cannot be read.
func (app *Application) characterAssets(ctx context.Context, ch db.Character) (items []esi.Asset, ok bool) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapAssets})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("assets: read snapshot for character %d: %v", ch.CharacterID, err)
		}
		return nil, false
	}
	if err := json.Unmarshal([]byte(snap.Payload), &items); err != nil {
		logging.Errorf("assets: decode snapshot for character %d: %v", ch.CharacterID, err)
		return nil, false
	}
	return items, true
}

// suggestOwnedAssets lists owned item types whose name contains q:
// names that start with it first, then the rest, alphabetical in each.
// The label says whose it is, or how many characters have it.
func (app *Application) suggestOwnedAssets(ctx context.Context, characters []db.Character, q string) []suggestItem {
	out := []suggestItem{}
	needle := strings.ToLower(strings.TrimSpace(q))
	if len(needle) < 2 {
		return out
	}
	owners := map[int64][]string{} // type id -> the characters holding it, in account order
	var typeIDs []int64
	// Singletons, for the names their owners gave them.
	var singles []int64
	singleType, singleOwner := map[int64]int64{}, map[int64]string{}
	for _, ch := range characters {
		items, ok := app.characterAssets(ctx, ch)
		if !ok {
			continue
		}
		has := map[int64]bool{}
		for _, it := range items {
			if it.IsSingleton {
				singles = append(singles, it.ItemID)
				singleType[it.ItemID], singleOwner[it.ItemID] = it.TypeID, ch.Name
			}
			if has[it.TypeID] {
				continue
			}
			has[it.TypeID] = true
			if _, known := owners[it.TypeID]; !known {
				typeIDs = append(typeIDs, it.TypeID)
			}
			owners[it.TypeID] = append(owners[it.TypeID], ch.Name)
		}
	}
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	type match struct {
		id     int64
		name   string
		prefix bool
	}
	var matches []match
	for _, id := range typeIDs {
		name, ok := names[id]
		if !ok {
			continue
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, needle) {
			matches = append(matches, match{id: id, name: name, prefix: strings.HasPrefix(lower, needle)})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].prefix != matches[j].prefix {
			return matches[i].prefix
		}
		return matches[i].name < matches[j].name
	})
	// A ship or container by the name its owner gave it leads the
	// list: someone typing "zoom" means their Zoom Zoom.
	if len(singles) > 0 {
		if rows, err := app.queries.ListItemNamesByIDs(ctx, singles); err == nil {
			sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
			for _, row := range rows {
				if row.Name == "" || !strings.Contains(strings.ToLower(row.Name), needle) || len(out) >= assetSuggestLimit {
					continue
				}
				typeID := singleType[row.ItemID]
				label := singleOwner[row.ItemID]
				if typeName, ok := names[typeID]; ok {
					label = typeName + " · " + label
				}
				out = append(out, suggestItem{ID: typeID, Name: row.Name, Label: label})
			}
		}
	}
	if room := assetSuggestLimit - len(out); len(matches) > room {
		matches = matches[:room]
	}
	for _, m := range matches {
		label := owners[m.id][0]
		if n := len(owners[m.id]); n > 1 {
			label = fmt.Sprintf("%d characters", n)
		}
		out = append(out, suggestItem{ID: m.id, Name: m.name, Label: label})
	}
	return out
}

// handleAssetsSuggest serves GET /assets/suggest?q=&only=: the
// search box's suggestions, from every linked character's assets or,
// with only=1, the acting character's alone.
func (app *Application) handleAssetsSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || userID == 0 {
		writeSuggestJSON(w, nil)
		return
	}
	if r.URL.Query().Get("only") == "1" {
		characters = onlyActingCharacter(characters, sessionCharID(app.sessions, ctx))
	}
	writeSuggestJSON(w, app.suggestOwnedAssets(ctx, characters, r.URL.Query().Get("q")))
}

// onlyActingCharacter narrows the account's characters to the acting
// one (the first, when the session names none of them).
func onlyActingCharacter(characters []db.Character, actingID int64) []db.Character {
	for _, ch := range characters {
		if ch.CharacterID == actingID {
			return []db.Character{ch}
		}
	}
	if len(characters) > 0 {
		return characters[:1]
	}
	return nil
}
