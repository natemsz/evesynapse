package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
func (app *Application) fleetWingNames(ctx context.Context, token string, fleetID int64) map[int64]string {
	var wings []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/fleets/%d/wings/", fleetID), &wings); err != nil {
		logging.Debugf("fleets: wings of %d: %v", fleetID, err)
		return nil
	}
	out := make(map[int64]string, len(wings))
	for _, w := range wings {
		out[w.ID] = w.Name
	}
	return out
}

// composeFleet turns one read into the stored composition.
func (app *Application) composeFleet(ctx context.Context, op db.Op, read fleetRead, wingNames map[int64]string, tags []db.ListOpMemberTagsRow) *fleetComposition {
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
			w = &fleetWing{ID: m.WingID, Name: wingNames[m.WingID]}
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
	payload, err := json.Marshal(comp)
	if err != nil {
		return 0, err
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
	seen, err := app.queries.UpsertFleetAttendance(ctx, params)
	if err != nil {
		return 0, err
	}
	if err := app.queries.UpsertOpComposition(ctx, db.UpsertOpCompositionParams{
		OpID: op.ID, Payload: string(payload), ComputedAt: now.UTC(),
	}); err != nil {
		return int(seen), err
	}
	return int(seen), nil
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
	ViewerChars map[int64]bool
}

func fleetCompositionPath(id int64) string { return opURL(id) + "composition" }

// fleetTabs is the second tab row on one fleet's pages. Composition is
// offered only to those who can open it.
func fleetTabs(id int64, composition bool, active string) []navLink {
	if !composition {
		return nil
	}
	return []navLink{
		{Href: opURL(id), Label: "Overview", Active: active == "overview"},
		{Href: fleetCompositionPath(id), Label: "Composition", Active: active == "composition"},
	}
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
		return "Attendance is being recorded from this fleet.", nil
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
func (app *Application) fleetLiveFor(ctx context.Context, op db.Op, v fleetViewer, a fleetAccess, live fleetLiveRead, page int, now time.Time) *fleetLiveView {
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
	if page > 1 {
		view.LiveURL += "?page=" + strconv.Itoa(page)
	}
	if live.row != nil {
		view.Revision = live.row.Revision
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
	if live.sight.All {
		view.Comp = &comp
		view.RoleBar = fleetRoleBar(comp.Roles, comp.Pilots)
		parts := make([]string, 0, len(comp.Roles))
		for _, role := range comp.Roles {
			parts = append(parts, strconv.Itoa(role.Count)+" "+role.Role)
		}
		view.RoleSummary = "Pilots by role: " + strings.Join(parts, ", ")
	} else {
		view.Comp = &fleetComposition{Pilots: comp.Pilots, Wings: comp.Wings}
	}
	view.Pages = max(1, (len(comp.Members)+fleetPilotsPerPage-1)/fleetPilotsPerPage)
	view.Page = min(max(page, 1), view.Pages)
	from := (view.Page - 1) * fleetPilotsPerPage
	shown := comp.Members[from:min(from+fleetPilotsPerPage, len(comp.Members))]
	view.Members = make([]fleetMemberRow, 0, len(shown))
	for _, m := range shown {
		view.Members = append(view.Members, live.sight.row(m, view.CanEdit))
	}
	if view.Page > 1 {
		view.PrevURL = fleetCompositionPath(op.ID) + "?page=" + strconv.Itoa(view.Page-1)
	}
	if view.Page < view.Pages {
		view.NextURL = fleetCompositionPath(op.ID) + "?page=" + strconv.Itoa(view.Page+1)
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
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	w.Header().Set("X-Robots-Tag", "noindex")
	data := pageData{ShellData: app.PublicShell(ctx), FleetLive: app.fleetLiveFor(ctx, op, v, a, live, page, time.Now())}
	// A pilot let in only by being in the fleet has no Overview to go to.
	data.SectionSubTabs = fleetTabs(op.ID, a.View, "composition")
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
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	app.renderFragment(w, "fleet_composition.html", "fleet-live", app.fleetLiveFor(ctx, op, v, a, live, page, time.Now()))
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
	back := fleetCompositionPath(op.ID)
	if p, _ := strconv.Atoi(r.FormValue("page")); p > 1 {
		back += "?page=" + strconv.Itoa(p)
	}
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
