package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/tags"
)

// ---------------------------------------------------------------------------
// The Assets page as a three-pane matrix: a station directory on
// the left (stored per-location values, schema 119), the selected
// place's stacks in the middle under the hangar divisions, and a
// ship inspector on the right when an assembled ship is selected.
//
// State lives in the query string so every view is a link: scope
// (all characters, or a comma-separated set of character IDs),
// loc, tab, can (a drilled-into container), ship (the inspected
// ship), q (account search) and sort. Saved quick filters
// (below) store a q/tab/scope triple under a name the user chose.
// ---------------------------------------------------------------------------

// maxMatrixRows caps one matrix render, the doctrine bound for a
// page: the most valuable stacks show, the rest are counted.
const maxMatrixRows = 100

// assetDirEntry is one row of the station directory: a place the
// account holds things in, with its folded value (schema 119) or,
// for a character whose first fold has not landed yet, counts
// folded from the snapshot at render with no value claimed.
type assetDirEntry struct {
	LocationID int64
	All        bool // the global row: every place in scope summed
	Title      string
	Loc        placeRef
	SecKnown   bool
	SecText    string
	SecTier    int
	Region     string
	Stacks     int64
	Ships      int64
	Items      int64
	Value      float64
	volume     float64 // folded top-level volume; the matrix header reads it
	ValueText  string
	HasValue   bool  // at least one stack here has a house price
	Unpriced   int64 // stacks no house price could value
	ShareClass string
	Selected   bool
	Href       string
}

// assetCharOption is one row of the character scope checklist:
// the character, its custom tags as chips, and the URL that
// toggles it in or out of the scope.
type assetCharOption struct {
	ID         int64
	Name       string
	Image      string
	Tags       []string // the character's custom tags, split for chips
	TagsText   string   // the stored tag string, for the search filter
	Checked    bool
	ToggleHref string
}

// assetScopeSelector is the character scope control: a button
// naming the current selection over a searchable checklist of
// the account's characters.
type assetScopeSelector struct {
	Label   string // "All characters", selected names, or "N characters"
	All     bool
	AllHref string // the reset: every character in scope
	Chars   []assetCharOption
}

// assetTab is one hangar division pill of the matrix.
type assetTab struct {
	ID     string
	Label  string
	Count  int
	Active bool
	Href   string
}

// assetCanCard is one container at the selected place, drawn as a
// folder: what is inside it is one click down.
type assetCanCard struct {
	ItemID    int64
	Name      string
	TypeName  string
	Stacks    int // stacks inside, at any depth
	ValueText string
	Href      string
	HueClass  string // the card icon box's accent, by position in the strip
}

// assetMatrixRow is one stack line of the matrix.
type assetMatrixRow struct {
	ItemID    int64
	OwnerID   int64
	OwnerName string // set when the scope spans characters
	Name      string
	TypeName  string // set when Name is the name its owner gave it
	TypeID    int64
	GroupName string
	IconURL   string
	Pip       string // the tech pip on the icon box (T2, FAC, CAP...), empty for none
	PipClass  string
	Assembled bool // an assembled ship: the row opens the inspector
	ShipHref  string
	ItemHref  string // a plain stack: the row selects it for the inspector's summary
	Selected  bool
	Flag      string // where the stack sits: a hangar, a container, a place
	Where     string // search hits: the containment chain, outermost first
	Note      string // BPC
	// Loadout chips, for ships holding anything.
	HasLoadout      bool
	Inside          int
	High, Mid, Low  int
	Sub, Rig, Cargo int
	Quantity        string
	VolumeText      string
	ValueText       string // empty when no house price knows the stack
	UnpricedNote    string // ships: "n of m stacks priced" when partial
}

// assetSlotRow is one grouped line of the inspector: a module type
// and how many of it are fitted (or stacked, in the cargo section).
type assetSlotRow struct {
	Name      string
	Qty       int64
	ValueText string
	Sub       string // the cargo section's bay label
}

// assetSlotSection is one slot family of the inspector.
type assetSlotSection struct {
	Title string
	Note  string // the ESI flag family, as the design shows it
	Rows  []assetSlotRow
}

// assetInspector is the right pane for one selected row: for an
// assembled ship, its fit by slot family, each module priced, and
// the EFT of the fit built by the fitting tool's own exporter;
// for anything else, the summary panel (Fallback): where the
// stack sits, how much of it there is, and what it is worth.
type assetInspector struct {
	ItemID     int64
	Name       string
	TypeName   string
	GroupName  string
	IconURL    string
	Pip        string
	PipClass   string
	OwnerName  string
	TotalText  string
	PricedNote string
	Sections   []assetSlotSection
	EFT        string
	CloseHref  string
	VolumeText string
	// The summary panel for a non-ship selection.
	Fallback     bool
	FlagText     string
	QuantityText string
	ValueText    string
}

// assetMatrix is the middle pane for one selected place (or for
// every place, when Global).
type assetMatrix struct {
	Global     bool
	Title      string
	CrumbLoc   string   // the selected place's name, for the breadcrumb (Title becomes the can's name inside one)
	Loc        placeRef // Title classified for the link policy
	SecKnown   bool
	SecText    string
	SecTier    int
	Region     string
	Stacks     int64
	VolumeText string
	ValueText  string
	Unpriced   int64
	Tabs       []assetTab
	Cans       []assetCanCard
	CansStacks int
	InCan      string // the drilled-into container's name, when in one
	CanBack    string // href out of the container
	Rows       []assetMatrixRow
	MoreRows   int
	Sort       string
	SortHrefs  map[string]string
	Inspector  *assetInspector
}

// assetSearch is the middle pane in search mode: flat matching
// stacks across the scope, each saying where it is and whose it is.
type assetSearch struct {
	Query       string
	TotalStacks int
	Characters  int // characters with at least one hit
	Only        bool
	Rows        []assetMatrixRow
	MoreRows    int
}

// assetFilterChip is one saved quick filter on the page.
type assetFilterChip struct {
	Name string
	Href string
}

// assetsPageView is the Assets page body.
type assetsPageView struct {
	Loaded     bool
	Warming    bool
	Syncing    []string // characters in scope whose first sync has not landed
	Stacks     int64
	TotalItems int64
	Places     int
	TotalText  string // the account's total estimated value, from the stored fold
	HasTotal   bool
	ActingName string
	ScopeAll   bool
	Selector   assetScopeSelector
	Directory  []assetDirEntry
	Matrix     *assetMatrix
	Search     *assetSearch
	Filters    []assetFilterChip
	// The state the save-filter form carries.
	FormQuery string
	FormTab   string
	FormScope string
}

// assetPageState is the parsed query-string state of the page.
type assetPageState struct {
	scopeAll bool
	scopeIDs []int64 // the selected characters, in account order, when not scopeAll
	loc      string  // "", "all", or a location id
	tab      string
	can      int64
	ship     int64
	item     int64 // a plain stack selected for the inspector's summary panel
	query    string
	sort     string
}

// inScope reports whether a character is in the page's scope.
func (st assetPageState) inScope(id int64) bool {
	if st.scopeAll {
		return true
	}
	for _, s := range st.scopeIDs {
		if s == id {
			return true
		}
	}
	return false
}

// joinIDList renders a character ID set the way the scope
// parameter carries it: comma-separated.
func joinIDList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// scopeParam is the scope parameter's value for the current set.
func (st assetPageState) scopeParam() string { return joinIDList(st.scopeIDs) }

var assetTabs = []struct{ id, label string }{
	{"all", "All Assets"},
	{"ships", "Ship Hangar"},
	{"items", "Item Hangar"},
	{"cans", "Station Cans"},
}

func validAssetTab(id string) bool {
	for _, t := range assetTabs {
		if t.id == id {
			return true
		}
	}
	return false
}

// parseAssetPageState reads the page's query-string state,
// defaulting anything unrecognized to the plain view.
func parseAssetPageState(q url.Values, characters []db.Character, active db.Character) assetPageState {
	st := assetPageState{tab: "all", sort: "value", scopeAll: true}
	if s := q.Get("scope"); s != "" && s != "all" {
		// The scope is a set of character IDs, comma-separated;
		// a lone ID is the one-character case the pills used to
		// link to. Unknown IDs drop out, and a set covering the
		// whole account (or nothing valid) reads as all.
		picked := make(map[int64]bool)
		for _, part := range strings.Split(s, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				picked[id] = true
			}
		}
		for _, ch := range characters {
			if picked[ch.CharacterID] {
				st.scopeIDs = append(st.scopeIDs, ch.CharacterID)
			}
		}
		if len(st.scopeIDs) > 0 && len(st.scopeIDs) < len(characters) {
			st.scopeAll = false
		} else {
			st.scopeIDs = nil
		}
	}
	// The search form's Only box, kept from the old page: it
	// narrows the scope to the acting character.
	if q.Get("only") == "1" {
		st.scopeAll, st.scopeIDs = false, []int64{active.CharacterID}
	}
	st.loc = q.Get("loc")
	if tab := q.Get("tab"); validAssetTab(tab) {
		st.tab = tab
	}
	st.can, _ = strconv.ParseInt(q.Get("can"), 10, 64)
	st.ship, _ = strconv.ParseInt(q.Get("ship"), 10, 64)
	st.item, _ = strconv.ParseInt(q.Get("item"), 10, 64)
	st.query = strings.TrimSpace(q.Get("q"))
	switch q.Get("sort") {
	case "name", "volume", "stacks":
		st.sort = q.Get("sort")
	}
	return st
}

// href builds a page URL from the state's canonical parameters
// with mutations applied (a key set to "" is dropped).
func (st assetPageState) href(mut map[string]string) string {
	v := url.Values{}
	if !st.scopeAll {
		v.Set("scope", st.scopeParam())
	}
	if st.loc != "" {
		v.Set("loc", st.loc)
	}
	if st.tab != "all" {
		v.Set("tab", st.tab)
	}
	if st.sort != "value" {
		v.Set("sort", st.sort)
	}
	if st.query != "" {
		v.Set("q", st.query)
	}
	for k, val := range mut {
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
	}
	if len(v) == 0 {
		return "/assets/"
	}
	// The scope set's commas stay readable in the address bar;
	// a literal comma is a legal query character and decodes
	// back to the same value.
	return "/assets/?" + strings.ReplaceAll(v.Encode(), "%2C", ",")
}

// assetCharData is one character's prepared snapshot: the flat
// list plus its containment index.
type assetCharData struct {
	ch    db.Character
	items []esi.Asset
	ix    *assetIndex
}

// handleAssets renders the Assets page. Asset data comes through
// the snapshot cache and the stored location values; every name
// is resolved from local caches only, so the page never waits on
// ESI.
func (app *Application) handleAssets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	userID := app.userID(ctx)
	if userID == 0 {
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

	// The acting character, exactly as the page has always picked
	// it: an explicit ?character= the user owns wins and pins the
	// session (a view pick does not), then the session character,
	// then the first linked character.
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
		if !viewPick(r) {
			app.pinSessionCharacter(ctx, active.CharacterID, active.Name)
		}
	} else if sid := sessionCharID(app.sessions, ctx); sid == 0 || !pick(sid) {
		active = characters[0]
	}
	for _, ch := range characters {
		data.AssetsChars = append(data.AssetsChars, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
			Image:  portraitURL(ch.CharacterID, 64),
		})
	}

	data.Assets = app.buildAssetsPage(ctx, userID, characters, active, r.URL.Query())
	app.render(ctx, w, http.StatusOK, "assets.html", data)
}

// buildAssetsPage assembles the whole page body for one state.
func (app *Application) buildAssetsPage(ctx context.Context, userID int64, characters []db.Character, active db.Character, q url.Values) *assetsPageView {
	st := parseAssetPageState(q, characters, active)
	view := &assetsPageView{ActingName: active.Name, ScopeAll: st.scopeAll}

	// The characters in scope, in account order.
	var scopeChars []db.Character
	for _, ch := range characters {
		if st.inScope(ch.CharacterID) {
			scopeChars = append(scopeChars, ch)
		}
	}

	// Load the snapshots in scope once; everything on the page
	// reads from these.
	datas := make([]*assetCharData, 0, len(scopeChars))
	for _, ch := range scopeChars {
		items, ok := app.characterAssets(ctx, ch)
		if !ok {
			view.Syncing = append(view.Syncing, ch.Name)
			continue
		}
		datas = append(datas, &assetCharData{ch: ch, items: items, ix: app.newAssetIndex(ctx, items)})
	}
	view.Loaded = len(datas) > 0
	view.Warming = !view.Loaded

	// Shared lookups across the scope: prices (the house order),
	// ship types, stored volumes and group names.
	typicals := app.TypicalSells(ctx)
	trueValues := app.storedTrueValues(ctx)
	typeSet := make(map[int64]bool)
	for _, cd := range datas {
		for _, it := range cd.items {
			typeSet[it.TypeID] = true
		}
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for id := range typeSet {
		typeIDs = append(typeIDs, id)
	}
	lk := assetLookups{
		priceOf: func(typeID int64) (float64, bool) {
			return pickValuationPrice(typicals[typeID], estSellOf(trueValues, typeID))
		},
		shipTypes: make(map[int64]bool),
	}
	if ids, err := app.queries.ListSDEShipTypeIDs(ctx, typeIDs); err != nil {
		logging.Errorf("assets: ship types: %v", err)
	} else {
		for _, id := range ids {
			lk.shipTypes[id] = true
		}
	}
	volumes := make(map[int64]float64)
	if rows, err := app.queries.ListSDETypePhysicsByIDs(ctx, typeIDs); err != nil {
		logging.Errorf("assets: type volumes: %v", err)
	} else {
		for _, row := range rows {
			volumes[row.TypeID] = row.Volume
		}
	}
	lk.volumeOf = func(typeID int64) float64 { return volumes[typeID] }
	groupIDs := app.esi.CachedTypeGroups(ctx, typeIDs)
	uniqGroups := make([]int64, 0, len(groupIDs))
	seenGroup := make(map[int64]bool)
	for _, gid := range groupIDs {
		if gid > 0 && !seenGroup[gid] {
			seenGroup[gid] = true
			uniqGroups = append(uniqGroups, gid)
		}
	}
	groupNames := app.esi.CachedGroupNames(ctx, uniqGroups)
	lk.groupOf = func(typeID int64) string { return groupNames[groupIDs[typeID]] }
	metaGroups := make(map[int64]int)
	if rows, err := app.queries.ListAssetTypeMetaGroups(ctx, typeIDs); err != nil {
		logging.Errorf("assets: meta groups: %v", err)
	} else {
		for _, row := range rows {
			metaGroups[row.TypeID] = int(row.Value)
		}
	}
	lk.metaOf = func(typeID int64) int { return metaGroups[typeID] }
	categories := make(map[int64]int64)
	if rows, err := app.queries.ListSDETypeCategories(ctx, typeIDs); err != nil {
		logging.Errorf("assets: type categories: %v", err)
	} else {
		for _, row := range rows {
			categories[row.TypeID] = row.CategoryID
		}
	}
	lk.categoryOf = func(typeID int64) int64 { return categories[typeID] }

	// The directory, from the stored folds with a counts-only
	// fallback for characters whose first fold has not landed.
	view.Directory = app.buildAssetDirectory(ctx, userID, datas, st, lk)
	for _, e := range view.Directory {
		if e.All {
			view.Stacks, view.TotalItems = e.Stacks, e.Items
			if e.HasValue {
				view.TotalText, view.HasTotal = e.ValueText, true
			}
		} else {
			view.Places++
		}
	}

	// The scope selector: a reset row plus one checklist row per
	// character, each row's link toggling that character in or
	// out of the scope. Toggling from "all characters" starts
	// from the whole account; an empty or complete set reads
	// back as all.
	view.Selector.All = st.scopeAll
	view.Selector.AllHref = st.href(map[string]string{"scope": "", "can": "", "ship": "", "item": ""})
	for _, ch := range characters {
		set := make(map[int64]bool)
		if st.scopeAll {
			for _, other := range characters {
				set[other.CharacterID] = true
			}
		} else {
			for _, id := range st.scopeIDs {
				set[id] = true
			}
		}
		if set[ch.CharacterID] {
			delete(set, ch.CharacterID)
		} else {
			set[ch.CharacterID] = true
		}
		var next []int64
		for _, other := range characters {
			if set[other.CharacterID] {
				next = append(next, other.CharacterID)
			}
		}
		param := ""
		if len(next) > 0 && len(next) < len(characters) {
			param = joinIDList(next)
		}
		charTags := tags.Split(ch.Tags)
		view.Selector.Chars = append(view.Selector.Chars, assetCharOption{
			ID: ch.CharacterID, Name: ch.Name, Image: portraitURL(ch.CharacterID, 64),
			Tags: charTags, TagsText: ch.Tags, Checked: st.inScope(ch.CharacterID),
			ToggleHref: st.href(map[string]string{"scope": param, "can": "", "ship": "", "item": ""}),
		})
	}
	switch {
	case st.scopeAll:
		view.Selector.Label = "All characters"
	case len(scopeChars) <= 2:
		names := make([]string, len(scopeChars))
		for i, ch := range scopeChars {
			names[i] = ch.Name
		}
		view.Selector.Label = strings.Join(names, ", ")
	default:
		view.Selector.Label = fmt.Sprintf("%d characters", len(scopeChars))
	}

	// Saved quick filters.
	for _, f := range app.assetFiltersFor(ctx, userID) {
		view.Filters = append(view.Filters, assetFilterChip{Name: f.Name, Href: f.href()})
	}
	view.FormQuery = st.query
	view.FormTab = st.tab
	if st.scopeAll {
		view.FormScope = "all"
	} else {
		view.FormScope = st.scopeParam()
	}

	if st.query != "" {
		view.Search = app.buildAssetSearch(ctx, datas, st, view.Directory, lk)
		return view
	}
	view.Matrix = app.buildAssetMatrix(ctx, datas, st, view.Directory, lk)
	return view
}

// buildAssetDirectory folds the stored per-location values of the
// characters in scope into directory entries, sorted by value.
// A character with a snapshot but no stored fold yet contributes
// counts folded on the spot, with no value claimed: its places
// show their stacks and ships, and the value lands at the next
// worker cycle.
func (app *Application) buildAssetDirectory(ctx context.Context, userID int64, datas []*assetCharData, st assetPageState, lk assetLookups) []assetDirEntry {
	type agg struct {
		locType string
		value   float64
		priced  int64
		stacks  int64
		items   int64
		ships   int64
		volume  float64
	}
	byLoc := make(map[int64]*agg)
	var locOrder []int64
	add := func(locID int64, locType string, v assetLocValue) {
		a, ok := byLoc[locID]
		if !ok {
			a = &agg{locType: locType}
			byLoc[locID] = a
			locOrder = append(locOrder, locID)
		}
		a.value += v.EstValue
		a.priced += v.PricedStacks
		a.stacks += v.TotalStacks
		a.items += v.TotalItems
		a.ships += v.Ships
		a.volume += v.Volume
	}

	foldedChars := make(map[int64]bool)
	if rows, err := app.queries.ListAssetLocationValuesByUser(ctx, userID); err != nil {
		logging.Errorf("assets: list location values for user %d: %v", userID, err)
	} else {
		inScope := make(map[int64]bool)
		for _, cd := range datas {
			inScope[cd.ch.CharacterID] = true
		}
		for _, row := range rows {
			if !inScope[row.CharacterID] {
				continue
			}
			foldedChars[row.CharacterID] = true
			add(row.LocationID, row.LocationType, assetLocValue{
				LocationID: row.LocationID, LocationType: row.LocationType,
				EstValue: row.EstValue, PricedStacks: row.PricedStacks,
				TotalStacks: row.TotalStacks, TotalItems: row.TotalItems,
				Ships: row.Ships, Volume: row.Volume,
			})
		}
	}
	noPrice := func(int64) (float64, bool) { return 0, false }
	for _, cd := range datas {
		if foldedChars[cd.ch.CharacterID] {
			continue
		}
		for _, v := range foldAssetLocationValues(cd.items, func(id int64) bool { return lk.shipTypes[id] }, noPrice, lk.volumeOf) {
			add(v.LocationID, v.LocationType, v)
		}
	}

	placeMemo := make(map[int64]placeRef)
	entries := make([]assetDirEntry, 0, len(byLoc))
	var maxValue float64
	for _, locID := range locOrder {
		a := byLoc[locID]
		if a.value > maxValue {
			maxValue = a.value
		}
		title := app.assetLocationTitle(ctx, locID, a.locType, nil, func(typeID int64) string {
			return fmt.Sprintf("Type #%d", typeID)
		}, nil)
		// Titles read type names from the same caches the old page
		// used; an item-parented place cannot occur at the top
		// level, so the name lookup above is never the answer.
		e := assetDirEntry{
			LocationID: locID, Title: title,
			Stacks: a.stacks, Ships: a.ships, Items: a.items,
			Value: a.value, volume: a.volume, HasValue: a.priced > 0, Unpriced: a.stacks - a.priced,
		}
		e.Loc = app.linkPlaceMemo(ctx, placeMemo, locID, title)
		if a.priced > 0 {
			e.ValueText = assetISK(a.value)
		}
		if sysID, ok := app.assetLocationSystem(ctx, locID, a.locType); ok {
			if text, tier, known := app.SystemSecurity(ctx, sysID); known {
				e.SecKnown, e.SecText, e.SecTier = true, text, tier
			}
			if region, ok := app.SystemRegion(ctx, sysID); ok {
				e.Region = region
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Value != entries[j].Value {
			return entries[i].Value > entries[j].Value
		}
		if entries[i].Stacks != entries[j].Stacks {
			return entries[i].Stacks > entries[j].Stacks
		}
		return entries[i].Title < entries[j].Title
	})
	for i := range entries {
		if maxValue > 0 && entries[i].Value > 0 {
			entries[i].ShareClass = pctWidthClass(entries[i].Value / maxValue)
		} else {
			entries[i].ShareClass = "pw-0"
		}
		entries[i].Href = st.href(map[string]string{
			"loc": strconv.FormatInt(entries[i].LocationID, 10), "can": "", "ship": "", "item": "", "q": "",
		})
	}

	// The global row leads: every place in scope, summed.
	global := assetDirEntry{All: true, Title: "All Locations", Loc: placeRef{Name: "All Locations"}, Href: st.href(map[string]string{"loc": "all", "can": "", "ship": "", "item": "", "q": ""})}
	for _, e := range entries {
		global.Stacks += e.Stacks
		global.Ships += e.Ships
		global.Items += e.Items
		global.Value += e.Value
		global.volume += e.volume
		global.Unpriced += e.Unpriced
		if e.HasValue {
			global.HasValue = true
		}
	}
	if global.HasValue {
		global.ValueText = assetISK(global.Value)
	}
	return append([]assetDirEntry{global}, entries...)
}

// assetLocationSystem resolves a place to its solar system: a
// system is itself, a station reads its SDE row, a structure
// reads its stored context. Anything else has no known system.
func (app *Application) assetLocationSystem(ctx context.Context, locID int64, locType string) (int64, bool) {
	switch locType {
	case "solar_system":
		return locID, true
	case "station":
		if st, err := app.queries.GetSDEStation(ctx, locID); err == nil && st.SystemID > 0 {
			return st.SystemID, true
		}
	case "structure":
		if sc, err := app.queries.GetStructureContext(ctx, locID); err == nil && sc.SystemID > 0 {
			return sc.SystemID, true
		}
	}
	return 0, false
}

// assetISK formats a value the way the design's figures read:
// billions to two decimals, millions to one.
func assetISK(v float64) string {
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", v/1_000_000_000)
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 10_000:
		return fmt.Sprintf("%.1fK", v/1_000)
	case v == 0:
		return "0"
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// assetVolume formats a volume in m3: whole numbers with
// thousands separators at any real size, one decimal below ten.
func assetVolume(v float64) string {
	if v > 0 && v < 10 {
		return fmt.Sprintf("%.1f m³", v)
	}
	return esi.FormatInt(int64(math.Round(v))) + " m³"
}
