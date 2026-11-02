package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"evesynapse/internal/app/contracts"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
)

// ---------------------------------------------------------------------------
// The matrix pane, the ship inspector and the search pane of the
// Assets page (char_assets_matrix.go has the handler, the state
// and the directory).
// ---------------------------------------------------------------------------

// assetLookups bundles the scope-wide lookups the panes share:
// the house price of a type, its stored volume, whether it is a
// ship, and its group name.
type assetLookups struct {
	priceOf    func(int64) (float64, bool)
	volumeOf   func(int64) float64
	shipTypes  map[int64]bool
	groupOf    func(int64) string
	metaOf     func(int64) int   // the SDE meta group (attribute 1692), 0 when unknown
	categoryOf func(int64) int64 // the SDE category of a type, 0 when unknown
}

// assetRowIconURL is the picture for one asset row: a ship
// leads with its render, and every other type resolves by SDE
// category, which is what decides whether the image server
// serves a picture at all (blueprints serve /bp, SKINs serve
// nothing and get the placeholder).
func assetRowIconURL(lk assetLookups, typeID int64) string {
	if lk.shipTypes[typeID] {
		return "https://images.evetech.net/types/" + strconv.FormatInt(typeID, 10) + "/render?size=64"
	}
	var categoryID int64
	if lk.categoryOf != nil {
		categoryID = lk.categoryOf(typeID)
	}
	return contracts.TypeCategoryIconURL(typeID, categoryID, 64)
}

// assetPip is the tech pip on a row's icon box, from the type's
// SDE meta group; capital hulls read CAP, as the design draws
// them. An empty label means no pip.
func assetPip(lk assetLookups, typeID int64, groupName string) (string, string) {
	switch groupName {
	case "Dreadnought", "Carrier", "Supercarrier", "Titan", "Force Auxiliary", "Capital Industrial Ship":
		return "CAP", "cap"
	}
	if lk.metaOf == nil {
		return "", ""
	}
	switch lk.metaOf(typeID) {
	case 2:
		return "T2", "t2"
	case 14:
		return "T3", "t3"
	case 4:
		return "FAC", "fac"
	case 6:
		return "DS", "ds"
	case 5:
		return "OFF", "off"
	case 3:
		return "STORY", "story"
	case 1:
		return "T1", "t1"
	}
	return "", ""
}

// assetFlagFamily sorts an asset location flag into the slot
// family the inspector groups by. ESI spells the families
// HiSlot/MedSlot/LoSlot/RigSlot/SubSystemSlot; anything else on a
// ship is cargo of one bay or another.
func assetFlagFamily(flag string) string {
	switch {
	case strings.HasPrefix(flag, "SubSystem"):
		return "sub"
	case strings.HasPrefix(flag, "HiSlot"):
		return "high"
	case strings.HasPrefix(flag, "MedSlot"), strings.HasPrefix(flag, "MidSlot"):
		return "mid"
	case strings.HasPrefix(flag, "LoSlot"), strings.HasPrefix(flag, "LowSlot"):
		return "low"
	case strings.HasPrefix(flag, "RigSlot"):
		return "rig"
	default:
		return "cargo"
	}
}

// humanAssetFlag renders a cargo-family flag the way a player
// reads it: "DroneBay" becomes "Drone Bay".
func humanAssetFlag(flag string) string {
	switch flag {
	case "Cargo":
		return "Cargo"
	case "DroneBay":
		return "Drone Bay"
	case "FuelBay":
		return "Fuel Bay"
	case "FleetHangar":
		return "Fleet Hangar"
	case "ShipHangar":
		return "Ship Hangar"
	case "FighterBay":
		return "Fighter Bay"
	case "SubSystemBay":
		return "Subsystem Bay"
	default:
		return flag
	}
}

// subtreeValue sums a stack and everything inside it at the
// given prices: the value itself, how many of its stacks a price
// could value, and how many stacks it holds in all.
func (ix *assetIndex) subtreeValue(it esi.Asset, price func(int64) (float64, bool)) (float64, int, int) {
	type node struct {
		it    esi.Asset
		depth int
	}
	value, priced, total := 0.0, 0, 0
	stack := []node{{it, 0}}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		total++
		if unit, ok := price(n.it.TypeID); ok {
			priced++
			value += unit * float64(n.it.Quantity)
		}
		if n.depth < assetNestMax {
			for _, child := range ix.children[n.it.ItemID] {
				stack = append(stack, node{child, n.depth + 1})
			}
		}
	}
	return value, priced, total
}

// assetRowCand is a matrix row before sorting and capping, with
// the numbers its sort keys read.
type assetRowCand struct {
	row      assetMatrixRow
	value    float64
	hasValue bool
	volume   float64
	division string // ships | items | cans
}

// buildAssetMatrix assembles the middle pane: it resolves the
// selected place (a drilled-into container or an inspected ship
// can move it), collects the rows from every character in scope,
// and attaches the inspector when a ship is open.
func (app *Application) buildAssetMatrix(ctx context.Context, datas []*assetCharData, st assetPageState, directory []assetDirEntry, lk assetLookups) *assetMatrix {
	titles := make(map[int64]string)
	entries := make(map[int64]*assetDirEntry)
	var globalEntry *assetDirEntry
	for i := range directory {
		e := &directory[i]
		if e.All {
			globalEntry = e
			continue
		}
		titles[e.LocationID] = e.Title
		entries[e.LocationID] = e
	}

	find := func(itemID int64) (*assetCharData, esi.Asset, bool) {
		for _, cd := range datas {
			if it, ok := cd.ix.byID[itemID]; ok {
				return cd, it, true
			}
		}
		return nil, esi.Asset{}, false
	}

	// A drilled-into container and an inspected ship both anchor
	// the view to the place they sit in.
	var canCD *assetCharData
	var canItem esi.Asset
	canPlace := int64(0)
	if st.can != 0 {
		if cd, it, ok := find(st.can); ok && !lk.shipTypes[it.TypeID] {
			canCD, canItem = cd, it
			canPlace, _, _ = cd.ix.place(it)
		}
	}
	var shipCD *assetCharData
	var shipItem esi.Asset
	shipPlace := int64(0)
	if st.ship != 0 {
		if cd, it, ok := find(st.ship); ok && lk.shipTypes[it.TypeID] && it.IsSingleton {
			shipCD, shipItem = cd, it
			shipPlace, _, _ = cd.ix.place(it)
		}
	}
	// A selected plain stack anchors the view the same way,
	// unless it sits inside a ship (those ride the ship's row).
	var itemCD *assetCharData
	var itemSel esi.Asset
	itemPlace := int64(0)
	if st.item != 0 && shipCD == nil {
		if cd, it, ok := find(st.item); ok {
			place, _, containers := cd.ix.place(it)
			insideShip := false
			for _, c := range containers {
				if lk.shipTypes[c.TypeID] {
					insideShip = true
				}
			}
			if !insideShip {
				itemCD, itemSel = cd, it
				itemPlace = place
			}
		}
	}

	global := false
	var sel *assetDirEntry
	switch {
	case canCD != nil:
		sel = entries[canPlace]
	case shipCD != nil && st.loc == "":
		sel = entries[shipPlace]
	case itemCD != nil && st.loc == "":
		sel = entries[itemPlace]
	case st.loc == "all":
		global = true
	case st.loc != "":
		if id, err := strconv.ParseInt(st.loc, 10, 64); err == nil {
			sel = entries[id]
		}
		if sel == nil {
			global = true // an unknown place falls back to the global view
		}
	default:
		for i := range directory {
			if !directory[i].All {
				sel = &directory[i]
				break
			}
		}
		if sel == nil {
			global = true
		}
	}
	for i := range directory {
		directory[i].Selected = (global && directory[i].All) || (sel != nil && directory[i].LocationID == sel.LocationID && !directory[i].All)
	}

	m := &assetMatrix{Global: global, Sort: st.sort, SortHrefs: map[string]string{}}
	for _, key := range []string{"value", "name", "volume", "stacks"} {
		mut := map[string]string{"sort": key}
		if key == "value" {
			mut["sort"] = ""
		}
		m.SortHrefs[key] = st.href(mut)
	}

	locFilter := int64(0)
	if sel != nil {
		locFilter = sel.LocationID
		m.Title = sel.Title
		m.CrumbLoc = sel.Title
		m.Loc = sel.Loc
		m.SecKnown, m.SecText, m.SecTier, m.Region = sel.SecKnown, sel.SecText, sel.SecTier, sel.Region
		m.Stacks, m.Unpriced = sel.Stacks, sel.Unpriced
		m.VolumeText = assetVolume(sel.volume)
		if sel.HasValue {
			m.ValueText = assetISK(sel.Value)
		}
	} else if globalEntry != nil {
		m.Title = "All Locations"
		m.CrumbLoc = "All Locations"
		m.Loc = globalEntry.Loc
		m.Stacks, m.Unpriced = globalEntry.Stacks, globalEntry.Unpriced
		m.VolumeText = assetVolume(globalEntry.volume)
		if globalEntry.HasValue {
			m.ValueText = assetISK(globalEntry.Value)
		}
	}

	showOwner := len(datas) > 1
	var cands []assetRowCand
	var cans []assetCanCard
	for _, cd := range datas {
		r, c := collectCharRows(cd, locFilter, st.can, global, titles, st, lk, showOwner)
		cands = append(cands, r...)
		cans = append(cans, c...)
	}
	sort.Slice(cans, func(i, j int) bool { return cans[i].Name < cans[j].Name })
	for i := range cans {
		cans[i].HueClass = fmt.Sprintf("can-h%d", i%4)
	}
	m.Cans = cans
	for _, c := range cans {
		m.CansStacks += c.Stacks
	}

	counts := map[string]int{}
	for _, c := range cands {
		counts[c.division]++
	}
	counts["cans"] = len(cans)
	total := len(cands)

	if canCD != nil {
		// Drilled into one container: the header describes the
		// container's contents, not the whole place.
		name, _ := canCD.ix.label(canItem)
		m.InCan = name
		m.CanBack = st.href(map[string]string{"can": "", "ship": "", "item": ""})
		m.Title = name
		m.Loc = placeRef{Name: name}
		m.Stacks = int64(total)
		value, priced, stacks := canCD.ix.subtreeValue(canItem, lk.priceOf)
		if unit, ok := lk.priceOf(canItem.TypeID); ok {
			value -= unit * float64(canItem.Quantity)
			priced--
		}
		stacks--
		m.Unpriced = int64(stacks - priced)
		if priced > 0 {
			m.ValueText = assetISK(value)
		} else {
			m.ValueText = ""
		}
		vol := 0.0
		for _, c := range cands {
			vol += c.volume
		}
		m.VolumeText = assetVolume(vol)
	}
	for _, t := range assetTabs {
		n := total
		if t.id != "all" {
			n = counts[t.id]
		}
		m.Tabs = append(m.Tabs, assetTab{
			ID: t.id, Label: t.label, Count: n, Active: st.tab == t.id,
			Href: st.href(map[string]string{"tab": t.id, "can": "", "ship": "", "item": ""}),
		})
	}

	var filtered []assetRowCand
	for _, c := range cands {
		if st.tab == "all" || c.division == st.tab {
			filtered = append(filtered, c)
		}
	}
	sortAssetRows(filtered, st.sort)
	if len(filtered) > maxMatrixRows {
		m.MoreRows = len(filtered) - maxMatrixRows
		filtered = filtered[:maxMatrixRows]
	}
	for _, c := range filtered {
		m.Rows = append(m.Rows, c.row)
	}

	if shipCD != nil && (global || (sel != nil && shipPlace == sel.LocationID)) {
		m.Inspector = app.buildAssetInspector(ctx, shipCD, shipItem, st, lk)
		for i := range m.Rows {
			if m.Rows[i].ItemID == shipItem.ItemID {
				m.Rows[i].Selected = true
			}
		}
	}

	// A selected plain stack gets the summary panel: where it
	// sits, how much of it there is, and what it is worth. It is
	// built from the row itself, so the panel and the table can
	// never disagree.
	if itemCD != nil && m.Inspector == nil && (global || (sel != nil && itemPlace == sel.LocationID)) {
		for i := range m.Rows {
			if m.Rows[i].ItemID != itemSel.ItemID {
				continue
			}
			m.Rows[i].Selected = true
			row := m.Rows[i]
			name, typeName := itemCD.ix.label(itemSel)
			insp := &assetInspector{
				Fallback:  true,
				ItemID:    itemSel.ItemID,
				Name:      name,
				TypeName:  typeName,
				GroupName: row.GroupName,
				IconURL:   row.IconURL,
				CloseHref: st.href(map[string]string{"item": ""}),
				ValueText: row.ValueText,
			}
			insp.Pip, insp.PipClass = assetPip(lk, itemSel.TypeID, row.GroupName)
			if showOwner {
				insp.OwnerName = itemCD.ch.Name
			}
			insp.QuantityText = esi.FormatInt(itemSel.Quantity)
			insp.VolumeText = row.VolumeText
			m.Inspector = insp
		}
	}
	return m
}

// sortAssetRows orders matrix candidates by the state's sort:
// value (priced first, richest first), name, volume, or stacks
// inside. Names break every tie so renders are stable.
func sortAssetRows(rows []assetRowCand, by string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch by {
		case "name":
			if a.row.Name != b.row.Name {
				return a.row.Name < b.row.Name
			}
		case "volume":
			if a.volume != b.volume {
				return a.volume > b.volume
			}
		case "stacks":
			if a.row.Inside != b.row.Inside {
				return a.row.Inside > b.row.Inside
			}
		default: // value
			if a.hasValue != b.hasValue {
				return a.hasValue
			}
			if a.value != b.value {
				return a.value > b.value
			}
		}
		if a.row.Name != b.row.Name {
			return a.row.Name < b.row.Name
		}
		return a.row.ItemID < b.row.ItemID
	})
}

// collectCharRows turns one character's stacks at one place (or
// at every place, when locFilter is 0) into matrix candidates and
// container cards. Inside a ship is not a row: it rides the
// ship's loadout chips and its inspector. Inside a container is
// a row under the cans division.
func collectCharRows(cd *assetCharData, locFilter, canFilter int64, global bool, titles map[int64]string, st assetPageState, lk assetLookups, showOwner bool) ([]assetRowCand, []assetCanCard) {
	var rows []assetRowCand
	var cans []assetCanCard
	base := func(it esi.Asset, locID int64) assetMatrixRow {
		name, typeName := cd.ix.label(it)
		row := assetMatrixRow{
			ItemID: it.ItemID, OwnerID: cd.ch.CharacterID,
			Name: name, TypeName: typeName, TypeID: it.TypeID,
			GroupName: lk.groupOf(it.TypeID),
			Quantity:  esi.FormatInt(it.Quantity),
			Note:      assetNote(it),
			ItemHref: st.href(map[string]string{
				"item": strconv.FormatInt(it.ItemID, 10), "ship": "",
				"loc": strconv.FormatInt(locID, 10),
			}),
		}
		row.Pip, row.PipClass = assetPip(lk, it.TypeID, row.GroupName)
		row.IconURL = assetRowIconURL(lk, it.TypeID)
		if showOwner {
			row.OwnerName = cd.ch.Name
		}
		return row
	}
	ownValue := func(it esi.Asset) (float64, bool, float64) {
		vol := lk.volumeOf(it.TypeID) * float64(it.Quantity)
		if unit, ok := lk.priceOf(it.TypeID); ok {
			return unit * float64(it.Quantity), true, vol
		}
		return 0, false, vol
	}

	for _, it := range cd.items {
		locID, _, containers := cd.ix.place(it)
		if locFilter != 0 && locID != locFilter {
			continue
		}
		inCan := false
		if canFilter != 0 {
			for _, c := range containers {
				if c.ItemID == canFilter {
					inCan = true
				}
			}
		}

		// Container cards: top-level containers, or, drilled in,
		// the containers directly inside the open one.
		if !lk.shipTypes[it.TypeID] && len(cd.ix.children[it.ItemID]) > 0 {
			isCard := false
			if canFilter == 0 && len(containers) == 0 {
				isCard = true
			}
			if canFilter != 0 && len(containers) == 1 && containers[0].ItemID == canFilter {
				isCard = true
			}
			if isCard {
				name, typeName := cd.ix.label(it)
				val, _, _ := cd.ix.subtreeValue(it, lk.priceOf)
				own, _, _ := ownValue(it)
				card := assetCanCard{
					ItemID: it.ItemID, Name: name, TypeName: typeName,
					Stacks: cd.ix.inside(it, 0),
					Href:   st.href(map[string]string{"can": strconv.FormatInt(it.ItemID, 10), "ship": "", "loc": strconv.FormatInt(locID, 10)}),
				}
				if inside := val - own; inside > 0 {
					card.ValueText = assetISK(inside)
				}
				cans = append(cans, card)
				continue // the card stands for the container in this view
			}
		}
		if canFilter != 0 && !inCan {
			continue
		}

		switch {
		case len(containers) == 0:
			// Top level: a ship, or a plain stack. A container
			// with anything in it became a card above; an empty
			// one is a plain stack like any other.
			if lk.shipTypes[it.TypeID] {
				rows = append(rows, shipRowCand(cd, it, locID, st, lk, base(it, locID)))
			} else {
				rows = append(rows, plainRowCand(it, "items", lk, base(it, locID)))
			}
		default:
			outer := containers[0]
			if lk.shipTypes[outer.TypeID] && canFilter == 0 {
				continue // inside a ship: the ship's row carries it
			}
			rows = append(rows, plainRowCand(it, "cans", lk, base(it, locID)))
		}
	}
	return rows, cans
}

// plainRowCand values a plain stack: its own quantity at the
// house price, its own volume.
func plainRowCand(it esi.Asset, division string, lk assetLookups, row assetMatrixRow) assetRowCand {
	cand := assetRowCand{row: row, division: division}
	if unit, ok := lk.priceOf(it.TypeID); ok {
		cand.value = unit * float64(it.Quantity)
		cand.hasValue = true
		cand.row.ValueText = assetISK(cand.value)
	}
	cand.volume = lk.volumeOf(it.TypeID) * float64(it.Quantity)
	cand.row.VolumeText = assetVolume(cand.volume)
	return cand
}

// shipRowCand builds an assembled or packaged ship's row: the
// loadout chips from what sits directly inside it, and the
// hull-plus-contents value.
func shipRowCand(cd *assetCharData, it esi.Asset, locID int64, st assetPageState, lk assetLookups, row assetMatrixRow) assetRowCand {
	cand := assetRowCand{row: row, division: "ships"}
	value, priced, _ := cd.ix.subtreeValue(it, lk.priceOf)
	if priced > 0 {
		cand.value, cand.hasValue = value, true
		cand.row.ValueText = assetISK(value)
	}
	cand.volume = lk.volumeOf(it.TypeID) * float64(it.Quantity)
	cand.row.VolumeText = assetVolume(cand.volume)
	if it.IsSingleton && it.Quantity == 1 {
		cand.row.Assembled = true
		cand.row.ShipHref = st.href(map[string]string{
			"ship": strconv.FormatInt(it.ItemID, 10),
			"loc":  strconv.FormatInt(locID, 10), "can": "",
		})
	}
	if kids := cd.ix.children[it.ItemID]; len(kids) > 0 {
		cand.row.HasLoadout = true
		cand.row.Inside = cd.ix.inside(it, 0)
		for _, kid := range kids {
			switch assetFlagFamily(kid.LocationFlag) {
			case "sub":
				cand.row.Sub += int(kid.Quantity)
			case "high":
				cand.row.High += int(kid.Quantity)
			case "mid":
				cand.row.Mid += int(kid.Quantity)
			case "low":
				cand.row.Low += int(kid.Quantity)
			case "rig":
				cand.row.Rig += int(kid.Quantity)
			default:
				cand.row.Cargo++
			}
		}
	}
	return cand
}

// buildAssetInspector assembles the right pane for one assembled
// ship: its fit grouped by slot family with per-type values, the
// hull-plus-fitted total, and the fit's EFT.
func (app *Application) buildAssetInspector(ctx context.Context, cd *assetCharData, ship esi.Asset, st assetPageState, lk assetLookups) *assetInspector {
	name, typeName := cd.ix.label(ship)
	insp := &assetInspector{
		ItemID:    ship.ItemID,
		Name:      name,
		TypeName:  typeName,
		GroupName: lk.groupOf(ship.TypeID),
		IconURL:   "https://images.evetech.net/types/" + strconv.FormatInt(ship.TypeID, 10) + "/render?size=128",
		OwnerName: cd.ch.Name,
		CloseHref: st.href(map[string]string{"ship": "", "item": ""}),
	}
	insp.Pip, insp.PipClass = assetPip(lk, ship.TypeID, insp.GroupName)
	insp.VolumeText = assetVolume(lk.volumeOf(ship.TypeID))

	total, priced, stacks := cd.ix.subtreeValue(ship, lk.priceOf)
	if priced > 0 {
		insp.TotalText = assetISK(total)
	}
	if priced < stacks {
		insp.PricedNote = fmt.Sprintf("Priced from %d of %d stacks; the rest have no house price yet.", priced, stacks)
	}

	// Group the ship's direct contents by slot family, then by
	// type within the family, in first-seen order.
	type group struct {
		typeID int64
		qty    int64
		sub    string
	}
	families := []struct {
		id, title, note string
	}{
		{"sub", "Subsystem Slots", ""},
		{"high", "High Slots", ""},
		{"mid", "Mid Slots", ""},
		{"low", "Low Slots", ""},
		{"rig", "Rig Slots", ""},
		{"cargo", "Cargo, Drone Bay and Fuel", ""},
	}
	byFamily := make(map[string][]*group)
	seen := make(map[string]map[int64]*group)
	kids := append([]esi.Asset(nil), cd.ix.children[ship.ItemID]...)
	sort.SliceStable(kids, func(i, j int) bool {
		return cd.ix.nameOf(kids[i].TypeID) < cd.ix.nameOf(kids[j].TypeID)
	})
	for _, kid := range kids {
		fam := assetFlagFamily(kid.LocationFlag)
		if seen[fam] == nil {
			seen[fam] = make(map[int64]*group)
		}
		g, ok := seen[fam][kid.TypeID]
		if !ok {
			g = &group{typeID: kid.TypeID}
			if fam == "cargo" {
				g.sub = humanAssetFlag(kid.LocationFlag)
			}
			seen[fam][kid.TypeID] = g
			byFamily[fam] = append(byFamily[fam], g)
		}
		g.qty += kid.Quantity
	}
	for _, fam := range families {
		groups := byFamily[fam.id]
		if len(groups) == 0 {
			continue
		}
		sec := assetSlotSection{Title: fam.title, Note: fam.note}
		if fam.id == "sub" {
			sec.Title = fmt.Sprintf("Subsystem Slots (%d)", len(groups))
		}
		if fam.id == "cargo" {
			sec.Note = fmt.Sprintf("%d Stacks", len(groups))
		}
		for _, g := range groups {
			row := assetSlotRow{Name: cd.ix.nameOf(g.typeID), Qty: g.qty, Sub: g.sub}
			if unit, ok := lk.priceOf(g.typeID); ok {
				row.ValueText = assetISK(unit * float64(g.qty))
			}
			sec.Rows = append(sec.Rows, row)
		}
		insp.Sections = append(insp.Sections, sec)
	}

	insp.EFT = app.assetEFT(ctx, cd, ship)
	return insp
}

// assetEFT renders one docked ship's fit as EFT through the
// fitting tool's own exporter: the fitted modules by slot flag,
// plus the drone bay and cargo stacks. Loaded charges are not
// part of the asset snapshot, so no charge rides a module line.
func (app *Application) assetEFT(ctx context.Context, cd *assetCharData, ship esi.Asset) string {
	name, _ := cd.ix.label(ship)
	doc := &fitview.FitDoc{Name: name, ShipTypeID: ship.TypeID}
	kids := append([]esi.Asset(nil), cd.ix.children[ship.ItemID]...)
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].LocationFlag != kids[j].LocationFlag {
			return kids[i].LocationFlag < kids[j].LocationFlag
		}
		return cd.ix.nameOf(kids[i].TypeID) < cd.ix.nameOf(kids[j].TypeID)
	})
	for _, kid := range kids {
		switch assetFlagFamily(kid.LocationFlag) {
		case "sub", "high", "mid", "low", "rig":
			doc.Items = append(doc.Items, fitview.FitDocItem{TypeID: kid.TypeID, Qty: int(kid.Quantity)})
		default:
			if kid.LocationFlag == "Cargo" || kid.LocationFlag == "DroneBay" {
				doc.Items = append(doc.Items, fitview.FitDocItem{TypeID: kid.TypeID, Qty: int(kid.Quantity)})
			}
		}
	}
	return app.fitBuilder().FormatEFT(ctx, doc)
}

// buildAssetSearch assembles the search pane: every stack in
// scope whose type name or given name matches, flat, each saying
// which place it is in, what it is inside, and whose it is. The
// old page grouped these by character and place; the matrix
// carries the same facts on the row.
func (app *Application) buildAssetSearch(ctx context.Context, datas []*assetCharData, st assetPageState, directory []assetDirEntry, lk assetLookups) *assetSearch {
	needle := strings.ToLower(st.query)
	titles := make(map[int64]string)
	for _, e := range directory {
		if !e.All {
			titles[e.LocationID] = e.Title
		}
	}
	view := &assetSearch{Query: st.query, Only: !st.scopeAll}
	hitChars := make(map[int64]bool)
	var cands []assetRowCand
	for _, cd := range datas {
		for _, it := range cd.items {
			if !strings.Contains(strings.ToLower(cd.ix.nameOf(it.TypeID)), needle) &&
				!strings.Contains(strings.ToLower(cd.ix.given[it.ItemID]), needle) {
				continue
			}
			hitChars[cd.ch.CharacterID] = true
			locID, _, containers := cd.ix.place(it)
			name, typeName := cd.ix.label(it)
			row := assetMatrixRow{
				ItemID: it.ItemID, OwnerID: cd.ch.CharacterID, OwnerName: cd.ch.Name,
				Name: name, TypeName: typeName, TypeID: it.TypeID,
				GroupName: lk.groupOf(it.TypeID),
				Quantity:  esi.FormatInt(it.Quantity),
				Note:      assetNote(it),
				ItemHref: st.href(map[string]string{
					"item": strconv.FormatInt(it.ItemID, 10), "ship": "", "q": "",
					"loc": strconv.FormatInt(locID, 10),
				}),
			}
			row.Pip, row.PipClass = assetPip(lk, it.TypeID, row.GroupName)
			row.IconURL = assetRowIconURL(lk, it.TypeID)
			if len(containers) > 0 {
				names := make([]string, 0, len(containers))
				for _, c := range containers {
					n, _ := cd.ix.label(c)
					names = append(names, n)
				}
				row.Where = strings.Join(names, " › ")
			}
			cand := assetRowCand{row: row}
			if lk.shipTypes[it.TypeID] && len(containers) == 0 {
				value, priced, _ := cd.ix.subtreeValue(it, lk.priceOf)
				if priced > 0 {
					cand.value, cand.hasValue = value, true
					cand.row.ValueText = assetISK(value)
				}
				if it.IsSingleton && it.Quantity == 1 {
					cand.row.Assembled = true
					cand.row.ShipHref = st.href(map[string]string{
						"ship": strconv.FormatInt(it.ItemID, 10), "q": "",
						"loc": strconv.FormatInt(locID, 10),
					})
				}
			} else if unit, ok := lk.priceOf(it.TypeID); ok {
				cand.value = unit * float64(it.Quantity)
				cand.hasValue = true
				cand.row.ValueText = assetISK(cand.value)
			}
			cand.volume = lk.volumeOf(it.TypeID) * float64(it.Quantity)
			cand.row.VolumeText = assetVolume(cand.volume)
			cands = append(cands, cand)
		}
	}
	view.TotalStacks = len(cands)
	view.Characters = len(hitChars)
	sortAssetRows(cands, "value")
	if len(cands) > maxMatrixRows {
		view.MoreRows = len(cands) - maxMatrixRows
		cands = cands[:maxMatrixRows]
	}
	for _, c := range cands {
		view.Rows = append(view.Rows, c.row)
	}
	return view
}
