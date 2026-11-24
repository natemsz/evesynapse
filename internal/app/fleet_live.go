package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The live fleet: what the worker read, stored ready to render.
//
// The fleet lane writes one op_composition row per read. The Composition
// tab and its polled fragment read only that row, so a page never calls
// ESI and its cost does not grow with the fleet.
//
// Doctrine matching is by hull. ESI does not show another pilot's fit,
// so nothing here claims a fit is right.
//
// Who sees what follows the game. Only a pilot in the fleet sees the live
// fleet, on a public fleet included, because live numbers and positions
// are worth something to the other side. See fleetSight for what each
// rank sees. Commanders outside the fleet get its setup status only.
// ---------------------------------------------------------------------------

const (
	fleetPilotsPerPage  = 100
	fleetInviteMax      = 100
	fleetTagMaxLen      = 24
	fleetTagsPerPilot   = 5
	fleetLiveStaleAfter = 3 * time.Minute
	fleetPollSeconds    = 20
)

type fleetHull struct {
	TypeID   int64  `json:"id"`
	Name     string `json:"name"`
	Class    string `json:"class,omitempty"`
	Count    int    `json:"n"`
	Doctrine bool   `json:"doctrine,omitempty"`
}

type fleetRoleCount struct {
	Role  string `json:"role"`
	Count int    `json:"n"`
}

type fleetWing struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Squads int    `json:"squads"`
	Pilots int    `json:"n"`
	// Units are the wing's squads that have pilots, in EVE's order, so
	// the structure keeps their names.
	Units []fleetSquad `json:"units,omitempty"`
}

// fleetSquad is one squad of a wing as last read.
type fleetSquad struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// fleetUnitNames are the names a fleet's boss gave its wings and
// squads, from one read of the fleet's wings.
type fleetUnitNames struct {
	Wings  map[int64]string
	Squads map[int64]string
}

type fleetSystem struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"n"`
}

// fleetMemberRow is one pilot as last read.
type fleetMemberRow struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	ShipTypeID int64    `json:"ship_id"`
	Ship       string   `json:"ship"`
	SystemID   int64    `json:"system_id"`
	System     string   `json:"system"`
	WingID     int64    `json:"wing_id"`
	SquadID    int64    `json:"squad_id"`
	Position   string   `json:"position"`
	Suggested  string   `json:"suggested"`
	Assigned   string   `json:"assigned,omitempty"`
	OnDoctrine bool     `json:"on_doctrine,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	// Hidden: this viewer's rank does not show this pilot's ship and
	// system. Set on the copy made for a page, never stored.
	Hidden bool `json:"-"`
	// Mine: one of the viewer's own characters. ShowSystem: the
	// structure names this pilot's system because the unit is spread
	// over several. Both are set on a page's copy only.
	Mine       bool `json:"-"`
	ShowSystem bool `json:"-"`
	// OffDoctrine: the fleet flies a doctrine and this pilot's hull is
	// not in it.
	OffDoctrine bool `json:"-"`
	// Plain: the name is not a link (the public example fleet).
	Plain bool `json:"-"`
}

// RoleClass is the colour class of the pilot's part in the fleet.
func (m fleetMemberRow) RoleClass() string { return fleetRoleClass(m.Role()) }

// Commands says the pilot leads a unit: the fleet, a wing or a squad.
func (m fleetMemberRow) Commands() bool {
	return m.Position == "fleet_commander" || m.Position == "wing_commander" || m.Position == "squad_commander"
}

// Role is the commander's assignment, or else what the hull suggests.
func (m fleetMemberRow) Role() string {
	if m.Assigned != "" {
		return m.Assigned
	}
	return m.Suggested
}

// PositionText is the pilot's place in the command chain, in words.
func (m fleetMemberRow) PositionText() string {
	switch m.Position {
	case "fleet_commander":
		return "Fleet commander"
	case "wing_commander":
		return "Wing commander"
	case "squad_commander":
		return "Squad commander"
	case "squad_member":
		return "Squad member"
	}
	return ""
}

// TagText is the pilot's tags as the form takes them.
func (m fleetMemberRow) TagText() string { return strings.Join(m.Tags, ", ") }

// fleetComposition is the stored payload of op_composition.
type fleetComposition struct {
	FleetID       int64            `json:"fleet_id"`
	BossID        int64            `json:"boss_id"`
	Boss          string           `json:"boss"`
	Pilots        int              `json:"pilots"`
	HasDoctrine   bool             `json:"has_doctrine,omitempty"`
	OnDoctrine    int              `json:"on_doctrine,omitempty"`
	OffDoctrine   int              `json:"off_doctrine,omitempty"`
	DoctrineRoles []string         `json:"doctrine_roles,omitempty"`
	MissingHulls  []string         `json:"missing_hulls,omitempty"`
	MissingRoles  []string         `json:"missing_roles,omitempty"`
	Hulls         []fleetHull      `json:"hulls"`
	Roles         []fleetRoleCount `json:"roles"`
	Wings         []fleetWing      `json:"wings,omitempty"`
	Systems       []fleetSystem    `json:"systems,omitempty"`
	Members       []fleetMemberRow `json:"members,omitempty"`
}

// recount refreshes the figures that depend on who holds which role.
func (c *fleetComposition) recount() {
	counts := map[string]int{}
	for _, m := range c.Members {
		counts[m.Role()]++
	}
	c.Roles = make([]fleetRoleCount, 0, len(opFleetRoles))
	for _, role := range opFleetRoles {
		if n := counts[role]; n > 0 {
			c.Roles = append(c.Roles, fleetRoleCount{Role: role, Count: n})
		}
	}
	c.MissingRoles = nil
	for _, role := range c.DoctrineRoles {
		if counts[role] == 0 {
			c.MissingRoles = append(c.MissingRoles, role)
		}
	}
}

// assign lays commanders' roles and tags over the members.
func (c *fleetComposition) assign(tags []db.ListOpMemberTagsRow) {
	roles := map[int64]string{}
	notes := map[int64][]string{}
	for _, t := range tags {
		if t.Kind == fleetTagRole {
			roles[t.CharacterID] = t.Tag
		} else {
			notes[t.CharacterID] = append(notes[t.CharacterID], t.Tag)
		}
	}
	for i := range c.Members {
		c.Members[i].Assigned = roles[c.Members[i].ID]
		c.Members[i].Tags = notes[c.Members[i].ID]
	}
	c.recount()
}

// Kinds of op_member_tags row.
const (
	fleetTagRole = "role"
	fleetTagNote = "tag"
)

// fleetRoleForHull is the part a hull plays, in the fleet's role names,
// from the same ship tables the scan pages use.
func fleetRoleForHull(typeID int64, group string) string {
	switch group {
	case "Interceptor":
		return "Tackle"
	case "Covert Ops":
		return "Scout"
	}
	role := "DPS"
	if r, ok := shipRoleTypes[typeID]; ok {
		role = r
	} else if r, ok := shipRoleGroups[group]; ok {
		role = r
	}
	switch role {
	case "Logistics", "Command", "DPS":
		return role
	case "Interdiction":
		return "Tackle"
	case "Recon & EWAR":
		return "EWAR"
	case "Bombers":
		return "DPS"
	}
	return "Other"
}

// fleetDoctrineFits is what the fleet is flying against: the copy taken
// at its start, or the current doctrine before then. A fleet with no
// doctrine (kitchen sink) has none.
func (app *Application) fleetDoctrineFits(ctx context.Context, op db.Op) []doctrineSnapshotFit {
	if op.DoctrineMode == doctrineNone {
		return nil
	}
	if op.DoctrineSnapshotAt.Valid && op.DoctrineSnapshot.Valid {
		var snapshot doctrineSnapshot
		if err := json.Unmarshal([]byte(op.DoctrineSnapshot.String), &snapshot); err != nil {
			logging.Errorf("fleets: doctrine snapshot of %d: %v", op.ID, err)
			return nil
		}
		return snapshot.Fits
	}
	if op.DoctrineID == 0 {
		return nil
	}
	d, err := app.queries.GetDoctrine(ctx, op.DoctrineID)
	if err != nil || doctrineOwner(d) != opOwner(op) {
		return nil
	}
	fits, err := app.queries.ListDoctrineFits(ctx, []int64{d.ID})
	if err != nil {
		logging.Errorf("fleets: doctrine fits of %d: %v", op.ID, err)
		return nil
	}
	out := make([]doctrineSnapshotFit, 0, len(fits))
	for _, fit := range fits {
		out = append(out, doctrineSnapshotFit{ID: fit.ID, Name: fit.Name, ShipTypeID: fit.ShipTypeID, Role: fit.FleetRole})
	}
	return out
}

// fleetWingNames reads the fleet's wing names. They are a nicety: on any
// failure the page numbers the wings instead.
func (app *Application) fleetWingNames(ctx context.Context, token string, fleetID int64) fleetUnitNames {
	var wings []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Squads []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"squads"`
	}
	out := fleetUnitNames{Wings: map[int64]string{}, Squads: map[int64]string{}}
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/fleets/%d/wings/", fleetID), &wings); err != nil {
		logging.Debugf("fleets: wings of %d: %v", fleetID, err)
		return out
	}
	for _, w := range wings {
		out.Wings[w.ID] = w.Name
		for _, sq := range w.Squads {
			out.Squads[sq.ID] = sq.Name
		}
	}
	return out
}

// composeFleet turns one read into the stored composition.
func (app *Application) composeFleet(ctx context.Context, op db.Op, read fleetRead, units fleetUnitNames, tags []db.ListOpMemberTagsRow) *fleetComposition {
	members := make([]esi.FleetMember, 0, len(read.Members))
	seen := make(map[int64]bool, len(read.Members))
	var hullIDs, unknown []int64
	hullSeen := map[int64]bool{}
	for _, m := range read.Members {
		if m.CharacterID == 0 || seen[m.CharacterID] {
			continue
		}
		seen[m.CharacterID] = true
		members = append(members, m)
		if m.ShipTypeID != 0 && !hullSeen[m.ShipTypeID] {
			hullSeen[m.ShipTypeID] = true
			hullIDs = append(hullIDs, m.ShipTypeID)
		}
		if _, ok := app.esi.CachedCharacterName(m.CharacterID); !ok {
			unknown = append(unknown, m.CharacterID)
		}
	}
	if len(unknown) > 0 {
		_, _ = app.resolveUniverseNamesLimited(ctx, &fetchBudget{left: 2}, unknown)
	}
	groups := map[int64]string{}
	if len(hullIDs) > 0 {
		rows, err := app.queries.ListTypeGroupNames(ctx, hullIDs)
		if err != nil {
			logging.Errorf("fleets: ship classes of %d: %v", op.ID, err)
		}
		for _, row := range rows {
			groups[row.TypeID] = row.Name
		}
	}

	// What the doctrine asks of each hull. A hull flown in two parts
	// names neither, and takes the part its class suggests.
	fits := app.fleetDoctrineFits(ctx, op)
	doctrineRole := map[int64]string{}
	inDoctrine := map[int64]bool{}
	wanted := map[string]bool{}
	for _, fit := range fits {
		if inDoctrine[fit.ShipTypeID] && doctrineRole[fit.ShipTypeID] != fit.Role {
			doctrineRole[fit.ShipTypeID] = ""
		} else if !inDoctrine[fit.ShipTypeID] {
			doctrineRole[fit.ShipTypeID] = fit.Role
		}
		inDoctrine[fit.ShipTypeID] = true
		if slices.Contains(opFleetRoles, fit.Role) {
			wanted[fit.Role] = true
		}
	}

	names := app.newKillNames(ctx)
	comp := &fleetComposition{
		FleetID: read.FleetID, BossID: read.BossID, Boss: names.character(read.BossID),
		Pilots: len(members), HasDoctrine: len(fits) > 0,
	}
	for _, role := range opFleetRoles {
		if wanted[role] {
			comp.DoctrineRoles = append(comp.DoctrineRoles, role)
		}
	}
	hulls := map[int64]*fleetHull{}
	systems := map[int64]*fleetSystem{}
	wings := map[int64]*fleetWing{}
	squads := map[int64]map[int64]bool{}
	for _, m := range members {
		ship := ""
		if m.ShipTypeID != 0 {
			ship = names.typeName(m.ShipTypeID)
		}
		system := names.system(m.SolarSystemID).Name
		suggested := doctrineRole[m.ShipTypeID]
		if !slices.Contains(opFleetRoles, suggested) {
			suggested = fleetRoleForHull(m.ShipTypeID, groups[m.ShipTypeID])
		}
		on := inDoctrine[m.ShipTypeID]
		if len(fits) > 0 {
			if on {
				comp.OnDoctrine++
			} else {
				comp.OffDoctrine++
			}
		}
		comp.Members = append(comp.Members, fleetMemberRow{
			ID: m.CharacterID, Name: names.character(m.CharacterID),
			ShipTypeID: m.ShipTypeID, Ship: ship,
			SystemID: m.SolarSystemID, System: system,
			WingID: m.WingID, SquadID: m.SquadID, Position: m.Role,
			Suggested: suggested, OnDoctrine: on,
		})
		if m.ShipTypeID != 0 {
			h := hulls[m.ShipTypeID]
			if h == nil {
				h = &fleetHull{TypeID: m.ShipTypeID, Name: ship, Class: groups[m.ShipTypeID], Doctrine: on}
				hulls[m.ShipTypeID] = h
			}
			h.Count++
		}
		if m.SolarSystemID != 0 {
			s := systems[m.SolarSystemID]
			if s == nil {
				s = &fleetSystem{ID: m.SolarSystemID, Name: system}
				systems[m.SolarSystemID] = s
			}
			s.Count++
		}
		w := wings[m.WingID]
		if w == nil {
			w = &fleetWing{ID: m.WingID, Name: units.Wings[m.WingID]}
			wings[m.WingID] = w
			squads[m.WingID] = map[int64]bool{}
		}
		w.Pilots++
		if m.SquadID > 0 {
			squads[m.WingID][m.SquadID] = true
		}
	}
	for _, h := range hulls {
		comp.Hulls = append(comp.Hulls, *h)
	}
	sort.Slice(comp.Hulls, func(i, j int) bool {
		if comp.Hulls[i].Count != comp.Hulls[j].Count {
			return comp.Hulls[i].Count > comp.Hulls[j].Count
		}
		return comp.Hulls[i].Name < comp.Hulls[j].Name
	})
	for _, s := range systems {
		comp.Systems = append(comp.Systems, *s)
	}
	sort.Slice(comp.Systems, func(i, j int) bool {
		if comp.Systems[i].Count != comp.Systems[j].Count {
			return comp.Systems[i].Count > comp.Systems[j].Count
		}
		return comp.Systems[i].Name < comp.Systems[j].Name
	})
	for id, w := range wings {
		w.Squads = len(squads[id])
		for squad := range squads[id] {
			w.Units = append(w.Units, fleetSquad{ID: squad, Name: units.Squads[squad]})
		}
		sort.Slice(w.Units, func(i, j int) bool { return w.Units[i].ID < w.Units[j].ID })
		for i := range w.Units {
			if w.Units[i].Name == "" {
				w.Units[i].Name = "Squad " + strconv.Itoa(i+1)
			}
		}
		comp.Wings = append(comp.Wings, *w)
	}
	sort.Slice(comp.Wings, func(i, j int) bool { return comp.Wings[i].ID < comp.Wings[j].ID })
	number := 0
	for i := range comp.Wings {
		switch {
		case comp.Wings[i].ID <= 0:
			comp.Wings[i].Name = "Fleet command"
		default:
			number++
			if comp.Wings[i].Name == "" {
				comp.Wings[i].Name = "Wing " + strconv.Itoa(number)
			}
		}
	}
	missing := map[string]bool{}
	for _, fit := range fits {
		if !hullSeen[fit.ShipTypeID] && fit.ShipTypeID != 0 {
			missing[names.typeName(fit.ShipTypeID)] = true
		}
	}
	for name := range missing {
		comp.MissingHulls = append(comp.MissingHulls, name)
	}
	sort.Strings(comp.MissingHulls)
	sort.Slice(comp.Members, func(i, j int) bool { return comp.Members[i].Name < comp.Members[j].Name })
	comp.assign(tags)
	return comp
}

// storeFleetRead records one read: attendance for everyone in the fleet,
// and the composition the page renders.
func (app *Application) storeFleetRead(ctx context.Context, op db.Op, read fleetRead, token string, now time.Time) (int, error) {
	tags, err := app.queries.ListOpMemberTags(ctx, op.ID)
	if err != nil {
		return 0, err
	}
	comp := app.composeFleet(ctx, op, read, app.fleetWingNames(ctx, token, read.FleetID), tags)
	if err := app.saveFleetRead(ctx, op, comp, now); err != nil {
		return 0, err
	}
	return len(comp.Members), nil
}

// saveFleetRead stores a composed read: who was seen, and what the page
// renders.
func (app *Application) saveFleetRead(ctx context.Context, op db.Op, comp *fleetComposition, now time.Time) error {
	payload, err := json.Marshal(comp)
	if err != nil {
		return err
	}
	params := db.UpsertFleetAttendanceParams{OpID: op.ID, SeenAt: now.UTC()}
	for _, m := range comp.Members {
		params.CharacterIds = append(params.CharacterIds, m.ID)
		params.ShipTypeIds = append(params.ShipTypeIds, m.ShipTypeID)
		params.WingIds = append(params.WingIds, m.WingID)
		params.SquadIds = append(params.SquadIds, m.SquadID)
		params.Positions = append(params.Positions, m.Position)
		params.SystemIds = append(params.SystemIds, m.SystemID)
		params.SuggestedRoles = append(params.SuggestedRoles, m.Suggested)
	}
	if _, err := app.queries.UpsertFleetAttendance(ctx, params); err != nil {
		return err
	}
	return app.queries.UpsertOpComposition(ctx, db.UpsertOpCompositionParams{
		OpID: op.ID, Payload: string(payload), ComputedAt: now.UTC(),
	})
}

// ---------------------------------------------------------------------------
// The Composition tab.
// ---------------------------------------------------------------------------

// fleetRoleSegment is one part of the role bar, in the bar's 0 to 100
// geometry.
type fleetRoleSegment struct {
	Role  string
	Class string
	Count int
	X, W  float64
}

// fleetLiveView is the Composition tab and its polled fragment.
type fleetLiveView struct {
	ID          int64
	Title       string
	Revision    int64
	Has         bool // the viewer is in the fleet as last read
	Full        bool // fleet commander or boss: every ship and system, and the totals
	CanManage   bool // runs this fleet on EveSynapse: sees how the read is going
	CanEdit     bool // runs this fleet and is in it: sets roles and tags
	Reading     bool // a read of the fleet exists, whether or not the viewer is in it
	Stale       bool
	Polling     bool
	AsOf        string
	AsOfRaw     string
	Status      string
	Notes       []string
	PollSeconds int
	Comp        *fleetComposition
	RoleBar     []fleetRoleSegment
	RoleSummary string
	Members     []fleetMemberRow
	Page, Pages int
	PrevURL     string
	NextURL     string
	Invite      string
	InviteShown int
	InviteCount int
	InviteCut   bool
	Roles       []string
	LiveURL     string
	// TryURL is set on the demo's live fleet: the commander tools answer
	// from there and store nothing.
	TryURL      string
	ViewerChars map[int64]bool
	// Running says how long the fleet has been going ("38 min").
	Running string
	// Gaps is one sentence on what the doctrine asks for that nobody
	// in the fleet is flying, for those who see the whole fleet.
	Gaps string
	// Structure is the fleet as EVE's fleet window lays it out: fleet
	// command, then each wing and its squads, with ships shown as the
	// viewer's rank allows.
	Structure []fleetUnitView
	// The pilots panel (fleet_pilot_list.go): the view shown (list,
	// structure or table) and the links to each, whether the fleet is
	// flat, and for the list its groupings, class and hull chips, groups
	// and how many pilots the chips keep. ListQuery carries the reader's
	// choice through a saved edit.
	Mode      string
	Modes     []fleetChip
	Flat      bool
	Groupings []fleetChip
	Classes   []fleetChip
	Ships     []fleetChip
	List      []fleetListGroup
	ListShown int
	ListQuery string
	// Summary is the Overview's line per ship class for a flat fleet.
	Summary  []fleetListGroup
	listKept []fleetMemberRow
	listQ    fleetListQuery
}

// fleetUnitView is fleet command, one wing or one squad, as one viewer
// may see it.
type fleetUnitView struct {
	Name      string
	Pilots    int
	Commander *fleetMemberRow
	Members   []fleetMemberRow // everyone under the commander
	Hulls     []fleetHull      // the ships the viewer can see, most flown first
	Unseen    int              // pilots whose ship this viewer's rank does not show
	System    string           // the one system all of them are in, when the viewer sees them all
	Squads    []fleetUnitView
	Command   bool // fleet command, above the wings
}

// fleetStructure lays the members out by wing and squad. Every member
// sees the names and positions, as in EVE; ships and systems follow
// sight. It reads only the stored composition, one fleet of at most
// EVE's 256 pilots.
func fleetStructure(comp fleetComposition, sight fleetSight, edit bool) []fleetUnitView {
	bySquad := map[int64][]fleetMemberRow{}
	wingCommand := map[int64][]fleetMemberRow{}
	var command []fleetMemberRow
	for _, m := range comp.Members {
		r := sight.row(m, edit)
		r.Mine = sight.own[m.ID]
		r.OffDoctrine = comp.HasDoctrine && !r.Hidden && !m.OnDoctrine
		switch {
		case m.WingID <= 0:
			command = append(command, r)
		case m.SquadID <= 0:
			wingCommand[m.WingID] = append(wingCommand[m.WingID], r)
		default:
			bySquad[m.SquadID] = append(bySquad[m.SquadID], r)
		}
	}
	var out []fleetUnitView
	if len(command) > 0 {
		lead := fleetUnit("Fleet command", command, "fleet_commander")
		lead.Command = true
		out = append(out, lead)
	}
	for _, w := range comp.Wings {
		if w.ID <= 0 {
			continue
		}
		wing := fleetUnit(w.Name, wingCommand[w.ID], "wing_commander")
		wing.Pilots = w.Pilots
		seen := map[int64]bool{}
		for _, sq := range w.Units {
			seen[sq.ID] = true
			if rows := bySquad[sq.ID]; len(rows) > 0 {
				wing.Squads = append(wing.Squads, fleetUnit(sq.Name, rows, "squad_commander"))
			}
		}
		// A read stored before squads carried names still lays out.
		var rest []int64
		for _, m := range comp.Members {
			if m.WingID == w.ID && m.SquadID > 0 && !seen[m.SquadID] {
				seen[m.SquadID] = true
				rest = append(rest, m.SquadID)
			}
		}
		slices.Sort(rest)
		for _, id := range rest {
			wing.Squads = append(wing.Squads, fleetUnit("Squad "+strconv.Itoa(len(wing.Squads)+1), bySquad[id], "squad_commander"))
		}
		// The wing's own figures cover its squads too.
		for _, sq := range wing.Squads {
			wing.Unseen += sq.Unseen
			wing.Hulls = mergeFleetHulls(wing.Hulls, sq.Hulls)
		}
		wing.System = ""
		out = append(out, wing)
	}
	return out
}

// fleetGapText says what the doctrine asks for that the fleet lacks:
// "Nobody is flying EWAR or Command. No Claymore or Falcon in the
// fleet." Empty when nothing is missing.
func fleetGapText(roles, hulls []string) string {
	or := func(words []string) string {
		if len(words) < 2 {
			return strings.Join(words, "")
		}
		return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
	}
	var parts []string
	if len(roles) > 0 {
		parts = append(parts, "Nobody is flying "+or(roles)+".")
	}
	if len(hulls) > 0 {
		parts = append(parts, "No "+or(hulls)+" in the fleet.")
	}
	return strings.Join(parts, " ")
}

// fleetRunningText says how long a fleet has been going: "12 min",
// "1h 05m". Before its start it says nothing.
func fleetRunningText(d time.Duration) string {
	if d < time.Minute {
		if d < 0 {
			return ""
		}
		return "under a minute"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return fmt.Sprintf("%dh %02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// fleetUnit is one unit's view: its commander apart, the hulls the
// viewer can see counted, and its system when every pilot shows one.
func fleetUnit(name string, rows []fleetMemberRow, commander string) fleetUnitView {
	u := fleetUnitView{Name: name, Pilots: len(rows)}
	counts := map[int64]*fleetHull{}
	systems := map[string]bool{}
	for _, r := range rows {
		if r.Hidden {
			u.Unseen++
			continue
		}
		systems[r.System] = true
		if r.ShipTypeID == 0 {
			continue
		}
		h := counts[r.ShipTypeID]
		if h == nil {
			h = &fleetHull{TypeID: r.ShipTypeID, Name: r.Ship}
			counts[r.ShipTypeID] = h
		}
		h.Count++
	}
	if u.Unseen == 0 && len(systems) == 1 {
		for s := range systems {
			u.System = s
		}
	}
	for _, h := range counts {
		u.Hulls = append(u.Hulls, *h)
	}
	sortFleetHulls(u.Hulls)
	for _, r := range rows {
		r.ShowSystem = u.System == "" && !r.Hidden && r.System != ""
		if r.Position == commander && u.Commander == nil {
			lead := r
			u.Commander = &lead
			continue
		}
		u.Members = append(u.Members, r)
	}
	return u
}

func mergeFleetHulls(into, add []fleetHull) []fleetHull {
	for _, h := range add {
		found := false
		for i := range into {
			if into[i].TypeID == h.TypeID {
				into[i].Count += h.Count
				found = true
				break
			}
		}
		if !found {
			into = append(into, h)
		}
	}
	sortFleetHulls(into)
	return into
}

func sortFleetHulls(h []fleetHull) {
	sort.Slice(h, func(i, j int) bool {
		if h[i].Count != h[j].Count {
			return h[i].Count > h[j].Count
		}
		return h[i].Name < h[j].Name
	})
}

func fleetCompositionPath(id int64) string { return opURL(id) + "composition" }

// fleetTabs is the second tab row on one fleet's pages. Each tab is
// offered only to those who can open it.
func fleetTabs(id int64, overview, composition, srp, report bool, active string) []navLink {
	if !composition && !srp && !report {
		return nil
	}
	var tabs []navLink
	if overview {
		tabs = append(tabs, navLink{Href: opURL(id), Label: "Overview", Active: active == "overview"})
	}
	if composition {
		tabs = append(tabs, navLink{Href: fleetCompositionPath(id), Label: "Composition", Active: active == "composition"})
	}
	if srp {
		tabs = append(tabs, navLink{Href: opURL(id) + "srp", Label: "Ship Replacement", Active: active == "srp"})
	}
	if report {
		tabs = append(tabs, navLink{Href: fleetReportPath(id), Label: "Report", Active: active == "report"})
	}
	if len(tabs) < 2 {
		return nil
	}
	return tabs
}

// fleetSight is what a viewer's characters can see of a fleet in game.
// Every member sees who is in the fleet and where they sit in the command
// chain. Ships and systems follow EVE's Fleet Composition window: a squad
// commander sees their squad, a wing commander their wing, and the fleet
// commander and boss the whole fleet. A pilot always sees their own.
type fleetSight struct {
	In     bool
	All    bool
	own    map[int64]bool
	wings  map[int64]bool
	squads map[int64]bool
}

// sightFor reads the viewer's places from the stored fleet. An account
// with several characters in the fleet sees what each of them sees.
func (c *fleetComposition) sightFor(ids []int64) fleetSight {
	s := fleetSight{own: map[int64]bool{}, wings: map[int64]bool{}, squads: map[int64]bool{}}
	mine := make(map[int64]bool, len(ids))
	for _, id := range ids {
		mine[id] = true
	}
	if c.BossID != 0 && mine[c.BossID] {
		s.In, s.All = true, true
		s.own[c.BossID] = true
	}
	for _, m := range c.Members {
		if !mine[m.ID] {
			continue
		}
		s.In = true
		s.own[m.ID] = true
		switch m.Position {
		case "fleet_commander":
			s.All = true
		case "wing_commander":
			if m.WingID > 0 {
				s.wings[m.WingID] = true
			}
		case "squad_commander":
			if m.SquadID > 0 {
				s.squads[m.SquadID] = true
			}
		}
	}
	return s
}

func (s fleetSight) seesShip(m fleetMemberRow) bool {
	return s.All || s.own[m.ID] || (m.WingID > 0 && s.wings[m.WingID]) || (m.SquadID > 0 && s.squads[m.SquadID])
}

// row is a copy of one pilot holding only what this viewer may see.
// Roles and tags are commanders' notes, so they stay with those who
// can edit them. The suggested role goes with the ship it comes from.
func (s fleetSight) row(m fleetMemberRow, edit bool) fleetMemberRow {
	if !s.seesShip(m) {
		m.ShipTypeID, m.Ship, m.SystemID, m.System, m.Suggested, m.OnDoctrine = 0, "", 0, "", "", false
		m.Hidden = true
	}
	if !edit {
		m.Assigned, m.Tags = "", nil
	}
	return m
}

// fleetLiveOpen: the fleet is inside the window it is read in.
func fleetLiveOpen(op db.Op, now time.Time) bool {
	return !op.CancelledAt.Valid && now.Before(opEnd(op).Add(opCaptureTail)) && !now.Before(op.StartsAt.Add(-opCaptureLead))
}

// seesFleetLive: the Composition tab is for pilots in the fleet, and for
// those running it until it closes, who see its setup status and not
// its pilots. A share link alone never opens it.
func seesFleetLive(op db.Op, a fleetAccess, sight fleetSight, now time.Time) bool {
	return sight.In || (a.Manage && !op.CancelledAt.Valid && now.Before(opEnd(op).Add(opCaptureTail)))
}

// fleetLiveRead is the stored read of a running fleet and what one viewer
// can see of it. It is empty outside the window the fleet is read in, and
// for a read left from before the fleet was moved to a later time.
type fleetLiveRead struct {
	row   *db.OpComposition
	comp  fleetComposition
	sight fleetSight
}

func (app *Application) fleetLiveReadFor(ctx context.Context, op db.Op, v fleetViewer, now time.Time) (fleetLiveRead, error) {
	ids := v.characterIDs()
	if len(ids) == 0 || !fleetLiveOpen(op, now) {
		return fleetLiveRead{}, nil
	}
	row, err := app.storedComposition(ctx, op.ID)
	if err != nil || row == nil || row.ComputedAt.Before(op.StartsAt.Add(-opCaptureLead)) {
		return fleetLiveRead{}, err
	}
	live := fleetLiveRead{row: row}
	if err := json.Unmarshal([]byte(row.Payload), &live.comp); err != nil {
		return fleetLiveRead{}, err
	}
	live.sight = live.comp.sightFor(ids)
	return live, nil
}

// fleetCaptureStatus is captureSummary for one viewer. While the fleet
// runs, the count and the boss's name are for those who see the whole
// fleet.
func (app *Application) fleetCaptureStatus(ctx context.Context, op db.Op, now time.Time, recorded int, sight fleetSight) (string, []string) {
	if !sight.All && op.CaptureStatus == captureRunning && fleetLiveOpen(op, now) {
		return "Attendance is being read from the fleet in EVE.", nil
	}
	return app.captureSummary(ctx, op, now, recorded)
}

// liveFleetFromURL loads a fleet for its live pages. Being in the fleet
// in game opens them even for a pilot who could not otherwise see the
// fleet. Anyone else is answered here: a polled fragment that may no
// longer show gets a 403, so the page can reload.
func (app *Application) liveFleetFromURL(w http.ResponseWriter, r *http.Request, fragment bool) (db.Op, fleetViewer, fleetAccess, fleetLiveRead, bool) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: load fleet %d: %v", id, err)
		}
		app.renderFleetHidden(w, r, v)
		return db.Op{}, v, fleetAccess{}, fleetLiveRead{}, false
	}
	a := v.access(op, app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID])
	now := time.Now()
	live, err := app.fleetLiveReadFor(ctx, op, v, now)
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
		if fragment {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return db.Op{}, v, fleetAccess{}, fleetLiveRead{}, false
		}
	}
	if !seesFleetLive(op, a, live.sight, now) {
		switch {
		case !a.View:
			app.renderFleetHidden(w, r, v)
		case fragment:
			http.Error(w, "Forbidden", http.StatusForbidden)
		default:
			http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
		}
		return db.Op{}, v, fleetAccess{}, fleetLiveRead{}, false
	}
	return op, v, a, live, true
}

// redirectIntoLiveFleet sends a pilot who is in the fleet but cannot see
// its page to the part they can see.
func (app *Application) redirectIntoLiveFleet(w http.ResponseWriter, r *http.Request, op db.Op, v fleetViewer) bool {
	if r.Method != http.MethodGet {
		return false
	}
	live, err := app.fleetLiveReadFor(r.Context(), op, v, time.Now())
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
		return false
	}
	if !live.sight.In {
		return false
	}
	http.Redirect(w, r, fleetCompositionPath(op.ID), http.StatusSeeOther)
	return true
}

func fleetRoleClass(role string) string {
	switch role {
	case "DPS":
		return "fl-r-dps"
	case "Logistics":
		return "fl-r-logi"
	case "Tackle":
		return "fl-r-tackle"
	case "Scout":
		return "fl-r-scout"
	case "EWAR":
		return "fl-r-ewar"
	case "Command":
		return "fl-r-command"
	}
	return "fl-r-other"
}

func fleetRoleBar(roles []fleetRoleCount, pilots int) []fleetRoleSegment {
	if pilots <= 0 {
		return nil
	}
	out := make([]fleetRoleSegment, 0, len(roles))
	x := 0.0
	for _, r := range roles {
		w := float64(r.Count) * 100 / float64(pilots)
		out = append(out, fleetRoleSegment{Role: r.Role, Class: fleetRoleClass(r.Role), Count: r.Count, X: x, W: w})
		x += w
	}
	return out
}

// fleetLiveFor builds the view from the stored read. It copies only what
// the viewer's rank shows, so the template cannot show more by mistake.
func (app *Application) fleetLiveFor(ctx context.Context, op db.Op, v fleetViewer, a fleetAccess, live fleetLiveRead, q fleetListQuery, now time.Time) *fleetLiveView {
	view := &fleetLiveView{
		ID: op.ID, Title: op.Title, Full: live.sight.All, CanManage: a.Manage,
		CanEdit: a.Manage && live.sight.In, Reading: live.row != nil, Roles: opFleetRoles,
		Polling: fleetLiveOpen(op, now),
		LiveURL: fleetCompositionPath(op.ID) + "/live", PollSeconds: fleetPollSeconds,
		Page: 1, Pages: 1,
		ViewerChars: make(map[int64]bool, len(v.Characters)),
	}
	for _, c := range v.Characters {
		view.ViewerChars[c.CharacterID] = true
	}
	view.LiveURL += q.query()
	if live.row != nil {
		view.Revision = live.row.Revision
	}
	// A refresh would take a demo visitor's changes off the page.
	if isDemoFleet(op) && app.sessionIsDemo(ctx) {
		view.Polling, view.TryURL = false, fleetCompositionPath(op.ID)+"/try"+q.query()
	}
	if a.Manage {
		view.Status, view.Notes = app.fleetCaptureStatus(ctx, op, now, live.comp.Pilots, live.sight)
		// The list leaves out who is in the fleet, so it would name them
		// to a commander who is not.
		if live.row == nil || live.sight.In {
			app.fillFleetInvite(ctx, view, op, live.comp.Members)
		}
	}
	if !live.sight.In {
		return view
	}
	comp := live.comp
	view.Has = true
	view.AsOf = live.row.ComputedAt.UTC().Format("15:04") + " EVE"
	view.AsOfRaw = live.row.ComputedAt.UTC().Format(time.RFC3339)
	view.Stale = now.Sub(live.row.ComputedAt) > fleetLiveStaleAfter
	view.Running = fleetRunningText(now.Sub(op.StartsAt))
	view.Structure = fleetStructure(comp, live.sight, view.CanEdit)
	if live.sight.All {
		view.Comp = &comp
		view.RoleBar = fleetRoleBar(comp.Roles, comp.Pilots)
		parts := make([]string, 0, len(comp.Roles))
		for _, role := range comp.Roles {
			parts = append(parts, strconv.Itoa(role.Count)+" "+role.Role)
		}
		view.RoleSummary = "Pilots by role: " + strings.Join(parts, ", ")
		view.Gaps = fleetGapText(comp.MissingRoles, comp.MissingHulls)
	} else {
		view.Comp = &fleetComposition{Pilots: comp.Pilots, Wings: comp.Wings}
	}
	rows := make([]fleetMemberRow, 0, len(comp.Members))
	for _, m := range comp.Members {
		rows = append(rows, live.sight.row(m, view.CanEdit))
	}
	base := fleetCompositionPath(op.ID)
	fillFleetList(view, comp, rows, q, base)
	if view.Flat && len(rows) > fleetSummaryAt {
		view.Summary = fleetClassSummary(comp, rows)
	}
	// The table pages through the pilots the chips keep.
	kept := view.listKept
	view.Pages = max(1, (len(kept)+fleetPilotsPerPage-1)/fleetPilotsPerPage)
	view.Page = min(max(q.Page, 1), view.Pages)
	from := (view.Page - 1) * fleetPilotsPerPage
	view.Members = kept[from:min(from+fleetPilotsPerPage, len(kept))]
	pageQ := view.listQ
	if view.Page > 1 {
		pageQ.Page = view.Page - 1
		view.PrevURL = base + pageQ.query()
	}
	if view.Page < view.Pages {
		pageQ.Page = view.Page + 1
		view.NextURL = base + pageQ.query()
	}
	return view
}

// fillFleetInvite lists who said they are coming and is not in the
// fleet yet, for the FC to invite in game.
func (app *Application) fillFleetInvite(ctx context.Context, view *fleetLiveView, op db.Op, members []fleetMemberRow) {
	if op.CancelledAt.Valid || opEnd(op).Before(time.Now()) {
		return
	}
	signups, err := app.queries.ListOpSignups(ctx, op.ID)
	if err != nil {
		logging.Errorf("fleets: sign-ups for %d: %v", op.ID, err)
		return
	}
	inFleet := make(map[int64]bool, len(members))
	for _, m := range members {
		inFleet[m.ID] = true
	}
	waiting := make([]int64, 0, len(signups))
	for _, s := range signups {
		if s.Response == opYes && !inFleet[s.CharacterID] {
			waiting = append(waiting, s.CharacterID)
		}
	}
	app.primeVerifiedNames(ctx, waiting)
	var names []string
	for _, s := range signups {
		if s.Response != opYes || inFleet[s.CharacterID] {
			continue
		}
		view.InviteCount++
		if len(names) < fleetInviteMax {
			names = append(names, app.displayCharacter(ctx, s.CharacterID))
		}
	}
	sort.Strings(names)
	view.InviteShown = len(names)
	view.InviteCut = view.InviteCount > len(names)
	view.Invite = strings.Join(names, "\n")
}

func (app *Application) storedComposition(ctx context.Context, opID int64) (*db.OpComposition, error) {
	row, err := app.queries.GetOpComposition(ctx, opID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// handleFleetComposition is the Composition tab.
func (app *Application) handleFleetComposition(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, a, live, ok := app.liveFleetFromURL(w, r, false)
	if !ok {
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	data := pageData{ShellData: app.PublicShell(ctx), FleetLive: app.fleetLiveFor(ctx, op, v, a, live, fleetListQueryOf(r), time.Now())}
	// A pilot let in only by being in the fleet has no Overview to go to.
	_, _, _, srp := app.seesFleetSRP(ctx, op, v)
	data.SectionSubTabs = fleetTabs(op.ID, a.View, true, srp, app.fleetReportTab(ctx, op, v, time.Now()), "composition")
	data.Scripts = []string{"/static/fleets.js"}
	app.render(ctx, w, http.StatusOK, "fleet_composition.html", data)
}

// handleFleetLive is the polled fragment: stored rows only, and an
// empty answer when nothing changed since the revision the page holds.
func (app *Application) handleFleetLive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, a, live, ok := app.liveFleetFromURL(w, r, true)
	if !ok {
		return
	}
	held, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	var current int64
	if live.row != nil {
		current = live.row.Revision
	}
	if current == held {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	app.renderFragment(w, "fleet_composition.html", "fleet-live", app.fleetLiveFor(ctx, op, v, a, live, fleetListQueryOf(r), time.Now()))
}

// parseFleetTags reads the tag box: comma separated, short, no repeats.
func parseFleetTags(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		tag := strings.Join(strings.Fields(strings.Map(func(c rune) rune {
			if unicode.IsControl(c) {
				return ' '
			}
			return c
		}, part)), " ")
		if utf8.RuneCountInString(tag) > fleetTagMaxLen {
			tag = strings.TrimSpace(string([]rune(tag)[:fleetTagMaxLen]))
		}
		if tag == "" || slices.ContainsFunc(out, func(have string) bool { return strings.EqualFold(have, tag) }) {
			continue
		}
		out = append(out, tag)
		if len(out) == fleetTagsPerPilot {
			break
		}
	}
	return out
}

// handleFleetPilot saves a commander's role and tags for one pilot in
// the fleet, and updates the stored totals so the page shows it at once.
func (app *Application) handleFleetPilot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, a, live, ok := app.liveFleetFromURL(w, r, false)
	if !ok {
		return
	}
	// Back to the view the edit was made from.
	list, _ := url.ParseQuery(r.FormValue("list"))
	back := fleetCompositionPath(op.ID) + fleetListQueryFrom(list).query()
	defer func() { http.Redirect(w, r, back, http.StatusSeeOther) }()
	if !a.Manage || !live.sight.In {
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	role := r.FormValue("role")
	if role != "" && !slices.Contains(opFleetRoles, role) {
		return
	}
	row, comp := live.row, live.comp
	at := slices.IndexFunc(comp.Members, func(m fleetMemberRow) bool { return m.ID == characterID })
	if at < 0 {
		app.flash(ctx, "That pilot is not in the fleet as last read.")
		return
	}
	if err := app.saveFleetPilot(ctx, op.ID, characterID, role, parseFleetTags(r.FormValue("tags")), firstCharacter(v)); err != nil {
		logging.Errorf("fleets: save pilot %d on %d: %v", characterID, op.ID, err)
		app.flash(ctx, "That could not be saved. Try again.")
		return
	}
	tags, err := app.queries.ListOpMemberTags(ctx, op.ID)
	if err != nil {
		logging.Errorf("fleets: tags of %d: %v", op.ID, err)
		return
	}
	comp.assign(tags)
	payload, err := json.Marshal(comp)
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
		return
	}
	if _, err := app.queries.UpdateOpCompositionAt(ctx, db.UpdateOpCompositionAtParams{
		OpID: op.ID, Payload: string(payload), Revision: row.Revision,
	}); err != nil {
		logging.Errorf("fleets: update composition of %d: %v", op.ID, err)
	}
	app.flash(ctx, "Saved "+comp.Members[at].Name+".")
}

// fleetTryLimit is the most a demo visitor's page can send.
const fleetTryLimit = 64 << 10

// handleFleetTry answers the demo's commander tools. The page sends every
// change the visitor has made, and this lays them over the stored fleet
// and renders the result. Nothing is written, so a change lasts only as
// long as the page and no other visitor ever sees it.
func (app *Application) handleFleetTry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !app.sessionIsDemo(ctx) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, fleetTryLimit)
	op, v, a, live, ok := app.liveFleetFromURL(w, r, true)
	if !ok {
		return
	}
	if !isDemoFleet(op) || !a.Manage || !live.sight.In {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var edits map[string]struct {
		Role string `json:"role"`
		Tags string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("edits")), &edits); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	stored, err := app.queries.ListOpMemberTags(ctx, op.ID)
	if err != nil {
		logging.Errorf("fleets: tags of %d: %v", op.ID, err)
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	changed := make(map[int64]bool, len(edits))
	var tags []db.ListOpMemberTagsRow
	for _, m := range live.comp.Members {
		edit, ok := edits[strconv.FormatInt(m.ID, 10)]
		if !ok || (edit.Role != "" && !slices.Contains(opFleetRoles, edit.Role)) {
			continue
		}
		changed[m.ID] = true
		if edit.Role != "" {
			tags = append(tags, db.ListOpMemberTagsRow{CharacterID: m.ID, Kind: fleetTagRole, Tag: edit.Role})
		}
		for _, tag := range parseFleetTags(edit.Tags) {
			tags = append(tags, db.ListOpMemberTagsRow{CharacterID: m.ID, Kind: fleetTagNote, Tag: tag})
		}
	}
	for _, t := range stored {
		if !changed[t.CharacterID] {
			tags = append(tags, t)
		}
	}
	live.comp.assign(tags)
	q := fleetListQueryOf(r)
	if page, _ := strconv.Atoi(r.FormValue("page")); page > 0 {
		q.Page = page
	}
	app.renderFragment(w, "fleet_composition.html", "fleet-live", app.fleetLiveFor(ctx, op, v, a, live, q, time.Now()))
}

// saveFleetPilot replaces one pilot's role and tags together.
func (app *Application) saveFleetPilot(ctx context.Context, opID, characterID int64, role string, tags []string, by int64) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if err := q.ClearOpMemberTags(ctx, db.ClearOpMemberTagsParams{OpID: opID, CharacterID: characterID}); err != nil {
		return err
	}
	now := time.Now().UTC()
	insert := func(kind, tag string) error {
		return q.InsertOpMemberTag(ctx, db.InsertOpMemberTagParams{
			OpID: opID, CharacterID: characterID, Kind: kind, Tag: tag, SetBy: by, SetAt: now,
		})
	}
	if role != "" {
		if err := insert(fleetTagRole, role); err != nil {
			return err
		}
	}
	for _, tag := range tags {
		if err := insert(fleetTagNote, tag); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
