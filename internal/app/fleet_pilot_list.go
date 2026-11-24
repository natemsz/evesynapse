package app

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
)

// The Composition tab's pilots, three ways: the list (every pilot as a
// card, grouped and filtered), the structure (EVE's fleet window) and,
// for a commander, the table where roles and tags are set. An informal
// fleet often has no structure to speak of, one commander and a single
// squad of a hundred or more, so the list is what it opens on: chips by
// ship class and hull narrow it, and it groups by class, role, system or
// squad. Everything comes from the stored composition; nothing here reads
// the database.

const (
	fleetViewList      = "list"
	fleetViewStructure = "structure"
	fleetViewTable     = "table"

	fleetGroupClass  = "class"
	fleetGroupRole   = "role"
	fleetGroupSystem = "system"
	fleetGroupSquad  = "squad"
	fleetGroupName   = "name"
)

// fleetGroupings are the list's group-by choices, in the order offered.
var fleetGroupings = []struct{ key, label string }{
	{fleetGroupClass, "Ship class"},
	{fleetGroupRole, "Role"},
	{fleetGroupSystem, "System"},
	{fleetGroupSquad, "Squad"},
	{fleetGroupName, "Name only"},
}

// fleetListQuery is how the reader has the pilots laid out: the view,
// what the list groups by, the ship class and hull it keeps, and the
// table's page. It rides every link, the live refresh and a saved edit,
// so the page keeps the reader's choice while the fleet updates.
type fleetListQuery struct {
	View  string
	Group string
	Class string
	Ship  int64
	Page  int
}

func fleetListQueryOf(r *http.Request) fleetListQuery {
	return fleetListQueryFrom(r.URL.Query())
}

func fleetListQueryFrom(v url.Values) fleetListQuery {
	q := fleetListQuery{View: v.Get("view"), Group: v.Get("group"), Class: v.Get("class")}
	q.Ship, _ = strconv.ParseInt(v.Get("ship"), 10, 64)
	q.Page, _ = strconv.Atoi(v.Get("page"))
	return q
}

// values is q as URL values, empty parts left out.
func (q fleetListQuery) values() url.Values {
	v := url.Values{}
	if q.View != "" {
		v.Set("view", q.View)
	}
	if q.Group != "" {
		v.Set("group", q.Group)
	}
	if q.Class != "" {
		v.Set("class", q.Class)
	}
	if q.Ship != 0 {
		v.Set("ship", strconv.FormatInt(q.Ship, 10))
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	return v
}

// query is q as a URL's query, with its "?", or nothing.
func (q fleetListQuery) query() string {
	if v := q.values(); len(v) > 0 {
		return "?" + v.Encode()
	}
	return ""
}

// fleetChip is one choice on the pilots panel: a view, a grouping, a
// ship class or a hull, with the link that picks it.
type fleetChip struct {
	Label  string
	Count  int
	TypeID int64
	Href   string
	On     bool
}

// fleetListGroup is one heading of the list and the pilots under it.
type fleetListGroup struct {
	Name    string
	Pilots  int
	Hulls   []fleetHull // the ships it holds that the viewer can see, most flown first
	Unseen  int         // pilots whose ship this viewer's rank does not show
	Members []fleetMemberRow
}

// fleetIsFlat says the fleet has no structure worth drawing: its pilots
// below fleet command sit in one squad at most. Such a fleet opens on
// the list.
func fleetIsFlat(members []fleetMemberRow) bool {
	squads := map[[2]int64]bool{}
	for _, m := range members {
		if m.WingID > 0 && m.SquadID > 0 {
			squads[[2]int64{m.WingID, m.SquadID}] = true
		}
	}
	return len(squads) <= 1
}

// fleetClasses maps each hull in the fleet to its ship class.
func fleetClasses(comp fleetComposition) map[int64]string {
	out := make(map[int64]string, len(comp.Hulls))
	for _, h := range comp.Hulls {
		out[h.TypeID] = h.Class
	}
	return out
}

const (
	fleetClassUnseen = "Ship not shown"
	fleetClassNone   = "Other ships"
)

// fleetClassOf is a pilot's ship class as the list names it.
func fleetClassOf(m fleetMemberRow, classes map[int64]string) string {
	switch {
	case m.Hidden:
		return fleetClassUnseen
	case classes[m.ShipTypeID] == "":
		return fleetClassNone
	}
	return classes[m.ShipTypeID]
}

// fillFleetList lays out the pilots panel for view: which view shows,
// the links to the others, and for the list and the table the class and
// hull chips and the pilots they keep. rows are every pilot as this
// viewer sees them, base the Composition tab's path.
func fillFleetList(view *fleetLiveView, comp fleetComposition, rows []fleetMemberRow, q fleetListQuery, base string) {
	view.Flat = fleetIsFlat(rows)
	switch {
	case q.View == fleetViewTable && view.CanEdit, q.View == fleetViewList, q.View == fleetViewStructure:
		view.Mode = q.View
	case view.Flat:
		view.Mode = fleetViewList
	default:
		view.Mode = fleetViewStructure
	}
	if !slices.ContainsFunc(fleetGroupings, func(g struct{ key, label string }) bool { return g.key == q.Group }) {
		q.Group = ""
	}
	group := q.Group
	if group == "" {
		group = fleetGroupClass
	}
	link := func(change func(*fleetListQuery)) string {
		next := q
		next.Page = 0
		change(&next)
		return base + next.query()
	}
	modes := []struct{ key, label string }{{fleetViewList, "List"}, {fleetViewStructure, "Structure"}}
	if view.CanEdit {
		modes = append(modes, struct{ key, label string }{fleetViewTable, "Roles and tags"})
	}
	for _, m := range modes {
		key := m.key
		view.Modes = append(view.Modes, fleetChip{Label: m.label, On: view.Mode == key, Href: link(func(n *fleetListQuery) { n.View = key })})
	}
	for _, g := range fleetGroupings {
		key := g.key
		view.Groupings = append(view.Groupings, fleetChip{Label: g.label, On: group == key, Href: link(func(n *fleetListQuery) {
			n.View, n.Group = fleetViewList, key
			if key == fleetGroupClass {
				n.Group = ""
			}
		})})
	}

	// The chips: every class the viewer can see, the most flown first,
	// and the hulls of the class picked.
	classes := fleetClasses(comp)
	classCount, shipCount := map[string]int{}, map[int64]int{}
	shipName := map[int64]string{}
	for _, m := range rows {
		if m.Hidden || m.ShipTypeID == 0 {
			continue
		}
		c := fleetClassOf(m, classes)
		classCount[c]++
		if c == q.Class {
			shipCount[m.ShipTypeID]++
			shipName[m.ShipTypeID] = m.Ship
		}
	}
	if classCount[q.Class] == 0 {
		q.Class, q.Ship = "", 0
	}
	if shipCount[q.Ship] == 0 {
		q.Ship = 0
	}
	view.Classes = append(view.Classes, fleetChip{Label: "All", Count: len(rows), On: q.Class == "", Href: link(func(n *fleetListQuery) { n.Class, n.Ship = "", 0 })})
	for _, c := range fleetSortedCounts(classCount) {
		name := c
		view.Classes = append(view.Classes, fleetChip{Label: name, Count: classCount[name], On: q.Class == name, Href: link(func(n *fleetListQuery) { n.Class, n.Ship = name, 0 })})
	}
	if q.Class != "" && len(shipCount) > 1 {
		ids := make([]int64, 0, len(shipCount))
		for id := range shipCount {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if shipCount[ids[i]] != shipCount[ids[j]] {
				return shipCount[ids[i]] > shipCount[ids[j]]
			}
			return shipName[ids[i]] < shipName[ids[j]]
		})
		for _, id := range ids {
			ship := id
			view.Ships = append(view.Ships, fleetChip{Label: shipName[ship], Count: shipCount[ship], TypeID: ship, On: q.Ship == ship, Href: link(func(n *fleetListQuery) {
				if n.Ship == ship {
					n.Ship = 0
				} else {
					n.Ship = ship
				}
			})})
		}
	}

	// The pilots the chips keep.
	kept := rows
	if q.Class != "" {
		kept = make([]fleetMemberRow, 0, classCount[q.Class])
		for _, m := range rows {
			if !m.Hidden && fleetClassOf(m, classes) == q.Class && (q.Ship == 0 || m.ShipTypeID == q.Ship) {
				kept = append(kept, m)
			}
		}
	}
	view.ListShown = len(kept)
	q.View = view.Mode
	view.listQ = q
	view.ListQuery = q.values().Encode()
	if view.Mode == fleetViewList {
		list := kept
		// A fleet spread over systems names each pilot's, unless the
		// list already groups by system.
		if group != fleetGroupSystem && len(comp.Systems) > 1 {
			list = slices.Clone(kept)
			for i := range list {
				list[i].ShowSystem = !list[i].Hidden && list[i].System != ""
			}
		}
		view.List = fleetGroupPilots(list, comp, classes, group)
	}
	view.listKept = kept
}

// fleetGroupPilots groups pilots for the list. Within a group they sort
// by ship, then name, so a class reads hull by hull.
func fleetGroupPilots(rows []fleetMemberRow, comp fleetComposition, classes map[int64]string, group string) []fleetListGroup {
	squadName := map[[2]int64]string{}
	wingName := map[int64]string{}
	wingOrder := map[int64]int{}
	for i, w := range comp.Wings {
		wingName[w.ID], wingOrder[w.ID] = w.Name, i
		for _, u := range w.Units {
			squadName[[2]int64{w.ID, u.ID}] = u.Name
		}
	}
	key := func(m fleetMemberRow) string {
		switch group {
		case fleetGroupRole:
			if m.Hidden {
				return "Role not shown"
			}
			if r := m.Role(); r != "" {
				return r
			}
			return "No role"
		case fleetGroupSystem:
			switch {
			case m.Hidden:
				return "System not shown"
			case m.System == "":
				return "Unknown system"
			}
			return m.System
		case fleetGroupSquad:
			if m.WingID <= 0 {
				return wingName[m.WingID]
			}
			if s := squadName[[2]int64{m.WingID, m.SquadID}]; s != "" {
				return wingName[m.WingID] + ", " + s
			}
			return wingName[m.WingID]
		case fleetGroupName:
			return "Pilots"
		}
		return fleetClassOf(m, classes)
	}
	byKey := map[string]*fleetListGroup{}
	var order []string
	squadOf := map[string][2]int64{}
	for _, m := range rows {
		k := key(m)
		g := byKey[k]
		if g == nil {
			g = &fleetListGroup{Name: k}
			byKey[k] = g
			order = append(order, k)
			squadOf[k] = [2]int64{int64(wingOrder[m.WingID]), m.SquadID}
		}
		g.Members = append(g.Members, m)
	}
	last := map[string]bool{fleetClassUnseen: true, fleetClassNone: true, "Role not shown": true, "No role": true, "System not shown": true, "Unknown system": true}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if last[a] != last[b] {
			return !last[a]
		}
		switch group {
		case fleetGroupRole:
			ia, ib := slices.Index(opFleetRoles, a), slices.Index(opFleetRoles, b)
			if ia != ib {
				return ia >= 0 && (ib < 0 || ia < ib)
			}
		case fleetGroupSquad:
			sa, sb := squadOf[a], squadOf[b]
			if sa != sb {
				return sa[0] < sb[0] || sa[0] == sb[0] && sa[1] < sb[1]
			}
			return a < b
		}
		if len(byKey[a].Members) != len(byKey[b].Members) {
			return len(byKey[a].Members) > len(byKey[b].Members)
		}
		return a < b
	})
	out := make([]fleetListGroup, 0, len(order))
	for _, k := range order {
		g := byKey[k]
		// Commanders first, then hull by hull, the most flown first as
		// the heading lists them, then by name.
		flown := map[int64]int{}
		for _, m := range g.Members {
			if !m.Hidden {
				flown[m.ShipTypeID]++
			}
		}
		sort.SliceStable(g.Members, func(i, j int) bool {
			a, b := g.Members[i], g.Members[j]
			if a.Commands() != b.Commands() {
				return a.Commands()
			}
			if group != fleetGroupName && (a.Hidden != b.Hidden || a.ShipTypeID != b.ShipTypeID) {
				if a.Hidden != b.Hidden {
					return !a.Hidden
				}
				if flown[a.ShipTypeID] != flown[b.ShipTypeID] {
					return flown[a.ShipTypeID] > flown[b.ShipTypeID]
				}
				return a.Ship < b.Ship
			}
			return a.Name < b.Name
		})
		g.Pilots = len(g.Members)
		hulls := map[int64]*fleetHull{}
		for _, m := range g.Members {
			switch {
			case m.Hidden:
				g.Unseen++
			case m.ShipTypeID != 0:
				h := hulls[m.ShipTypeID]
				if h == nil {
					h = &fleetHull{TypeID: m.ShipTypeID, Name: m.Ship}
					hulls[m.ShipTypeID] = h
				}
				h.Count++
			}
		}
		for _, h := range hulls {
			g.Hulls = append(g.Hulls, *h)
		}
		sortFleetHulls(g.Hulls)
		out = append(out, *g)
	}
	return out
}

// fleetSortedCounts is the keys of counts, the largest count first.
func fleetSortedCounts(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for k := range counts {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// fleetSummaryAt is how many pilots a flat fleet holds before the
// Overview shows it by ship class rather than as its one squad.
const fleetSummaryAt = 20

// fleetClassSummary is the Overview's picture of a flat fleet: a line
// per ship class with its hulls, in place of a structure that would be
// one long squad.
func fleetClassSummary(comp fleetComposition, rows []fleetMemberRow) []fleetListGroup {
	groups := fleetGroupPilots(rows, comp, fleetClasses(comp), fleetGroupClass)
	for i := range groups {
		groups[i].Members = nil
	}
	return groups
}
