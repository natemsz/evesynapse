package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Fitting simulator UI: the editor on the Fittings page.
//
// The browser keeps a small fit document (ship, item lines, charge
// choices) and posts it to /fittings/simulate/ on every change; the
// server runs the dogma engine (the fit package) over local SDE rows
// and returns the whole workbench as an HTML fragment. Everything
// here reads local tables and snapshots only -- no handler in this
// file ever reaches the network, and the tests pin that with the
// counting transport.
//
// Slot families come from the fitted type's slot effect, the same
// IDs the engine keys on (dgmEffects, verified against the live
// dump): 11 loPower, 12 hiPower, 13 medPower, 2663 rigSlot,
// 3772 subSystem. Drones carry no slot effect; they are the types
// with a drone bandwidth draw. Anything else in a fit document is
// cargo: listed, never simulated.
// ---------------------------------------------------------------------------

// fitSlotFamily names, in editor display order.
const (
	fitFamilyHigh      = "high"
	fitFamilyMedium    = "medium"
	fitFamilyLow       = "low"
	fitFamilyRig       = "rig"
	fitFamilySubsystem = "subsystem"
	fitFamilyDrone     = "drone"
	fitFamilyCargo     = "cargo"
)

// fitSlotEffectByFamily maps picker families to their slot effect.
var fitSlotEffectByFamily = map[string]int64{
	fitFamilyHigh:      fit.EffectHiPower,
	fitFamilyMedium:    fit.EffectMedPower,
	fitFamilyLow:       fit.EffectLoPower,
	fitFamilyRig:       fit.EffectRigSlot,
	fitFamilySubsystem: fit.EffectSubsystemSlot,
}

// Dogma attributes the charge picker reads off a weapon: the five
// charge-group slots and the charge size (dgmAttributeTypes,
// verified against the live dump).
var fitChargeGroupAttrs = []int64{604, 605, 606, 609, 610}

const fitAttrChargeSize = 128

// fitDocItem is one line of a fit document. States carries one
// module state per instance ("", online/active/offline/
// overheated; "" resolves to the type default); len(States) should
// match Qty and is normalized by sanitizeFitDoc.
type fitDocItem struct {
	TypeID int64    `json:"typeId"`
	Qty    int      `json:"qty"`
	States []string `json:"states,omitempty"`
}

// fitDoc is one fitting: the editor's working state and the
// stored shape of a local fitting (local_fittings.items_json).
type fitDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	ShipTypeID  int64           `json:"shipTypeId"`
	Items       []fitDocItem    `json:"items"`
	Charges     map[int64]int64 `json:"charges"` // weapon type -> loaded charge type
}

// sanitizeFitDoc normalizes a fit document arriving from the
// browser or storage: sane quantities, duplicate lines folded,
// charges for weapons no longer fitted dropped.
func sanitizeFitDoc(doc *fitDoc) {
	if doc == nil {
		return
	}
	merged := make([]fitDocItem, 0, len(doc.Items))
	index := make(map[int64]int, len(doc.Items))
	for _, it := range doc.Items {
		if it.TypeID <= 0 {
			continue
		}
		if it.Qty < 1 {
			it.Qty = 1
		}
		if it.Qty > 10000 {
			it.Qty = 10000
		}
		if j, ok := index[it.TypeID]; ok {
			merged[j].Qty += it.Qty
			if merged[j].Qty > 10000 {
				merged[j].Qty = 10000
			}
			continue
		}
		index[it.TypeID] = len(merged)
		merged = append(merged, it)
		if len(merged) >= 300 {
			break
		}
	}
	doc.Items = merged
	// Normalize per-instance module states: one entry per
	// instance, unknown values demoted to "" (the default), and
	// all-default arrays dropped so stored docs stay lean.
	for i := range merged {
		it := &merged[i]
		if len(it.States) > it.Qty {
			it.States = it.States[:it.Qty]
		}
		for len(it.States) < it.Qty {
			it.States = append(it.States, "")
		}
		allDefault := true
		for j, s := range it.States {
			switch s {
			case "", fit.StateOnline, fit.StateActive, fit.StateOffline, fit.StateOverheated:
			default:
				it.States[j] = ""
				s = ""
			}
			if s != "" {
				allDefault = false
			}
		}
		if allDefault {
			it.States = nil
		}
	}
	if doc.Charges == nil {
		doc.Charges = make(map[int64]int64)
	}
	fitted := make(map[int64]bool, len(merged))
	for _, it := range merged {
		fitted[it.TypeID] = true
	}
	for weapon := range doc.Charges {
		if !fitted[weapon] || doc.Charges[weapon] <= 0 {
			delete(doc.Charges, weapon)
		}
	}
	if len(doc.Name) > 120 {
		doc.Name = doc.Name[:120]
	}
}

// fitSlotFamilyOf classifies one type into an editor family using
// the engine snapshot (slot effects first, in the engine's own
// order, then the drone bandwidth draw).
func fitSlotFamilyOf(snap *fit.Snapshot, typeID int64) string {
	switch {
	case snap.HasEffect(typeID, fit.EffectRigSlot):
		return fitFamilyRig
	case snap.HasEffect(typeID, fit.EffectSubsystemSlot):
		return fitFamilySubsystem
	case snap.Attrs[typeID][fit.AttrDroneBandwidthUsed] > 0:
		return fitFamilyDrone
	case snap.HasEffect(typeID, fit.EffectHiPower):
		return fitFamilyHigh
	case snap.HasEffect(typeID, fit.EffectMedPower):
		return fitFamilyMedium
	case snap.HasEffect(typeID, fit.EffectLoPower):
		return fitFamilyLow
	}
	return fitFamilyCargo
}

// fitFamilyFitted reports whether the family takes part in the
// simulation (cargo is listed but never fitted).
func fitFamilyFitted(family string) bool {
	return family != fitFamilyCargo
}

// ---------------------------------------------------------------------------
// View model.
// ---------------------------------------------------------------------------

// fitSlotChip is one fitted item line in the slot grid. State
// is the uniform resolved state when every instance shares one
// ("" when mixed or stateless) — for the state visuals.
type fitSlotChip struct {
	TypeID int64
	Name   string
	Qty    int
	State  string
}

// fitSlotGroup is one row of the slot grid.
type fitSlotGroup struct {
	Key    string
	Label  string
	Used   int
	Max    int
	HasMax bool
	Over   bool
	Chips  []fitSlotChip
	Empty  []int // empty slot buttons to render (slot ordinals)
}

// fitVisualSlot is one slot circle on the in-game-style visual fit.
type fitVisualSlot struct {
	TypeID     int64
	Name       string
	GroupKey   string
	GroupLabel string
	Filled     bool
	Index      int     // position within the family's slot row
	X, Y       float64 // center position, percent of the visual box
	CPU        string  // per-module quick-look for the tooltip ("15 tf")
	PG         string  // ("10 MW")
	Meta       string  // ("Tech II")
	Stat       string  // headline effective stat ("Strength 78%")
	ChargeID   int64   // loaded charge type, 0 when the weapon is unloaded
	ChargeName string  // loaded charge name for the badge + tooltip
	// State is the module's resolved state ("" for stateless
	// kinds); ValidStates the states the tooltip may offer;
	// StatesCSV the comma-joined valid list for the data
	// attribute; TipKey ("typeID:ordinal") identifies the
	// instance for state changes.
	State       string
	ValidStates []string
	StatesCSV   string
	TipKey      string
}

// fitVisualView is the ship render with module slots arranged
// around it, in-game style.
type fitVisualView struct {
	ShipID     int64
	ShipName   string
	Slots      []fitVisualSlot
	Separators []fitVisualSep   // ember divider ticks between slot groups
	Labels     []fitVisualLabel // in-ring group captions (HIGH / MID / ...)
	Segs       []fitVisualSeg   // bordered arc segments, one per rendered group
}

// fitVisualSeg is one bordered arc segment behind a slot group: an
// annular sector path (SVG d) in the 0-100 viewBox, precomputed here
// so the template stays declarative.
type fitVisualSeg struct {
	D string
}

// fitVisualSep is one ember divider tick between two rendered slot
// groups, sitting on the slot ring at their shared boundary angle.
// Rot orients the tick's long axis radially (CSS rotate, degrees).
type fitVisualSep struct {
	X, Y float64
	Rot  float64
}

// fitVisualLabel is the in-ring caption for one rendered slot
// group, set in the ember gradient like the app's titles.
type fitVisualLabel struct {
	X, Y float64
	Text string
}

// fitVisualArcs places the five slot families on arcs around the
// ship (screen degrees: 0=east, 90=south, so 270 is straight up):
// highs across the top, mids down the right, lows across the
// bottom, rigs and subsystems sharing the left. The third value
// is the ring radius as a percent of the visual box.
var fitVisualArcs = map[string][3]float64{
	fitFamilyHigh:      {225, 315, 44},
	fitFamilyMedium:    {315, 405, 44},
	fitFamilyLow:       {45, 135, 44},
	fitFamilyRig:       {135, 180, 44},
	fitFamilySubsystem: {180, 225, 44},
}

var fitVisualGroupLabels = map[string]string{
	fitFamilyHigh:      "High slot",
	fitFamilyMedium:    "Mid slot",
	fitFamilyLow:       "Low slot",
	fitFamilyRig:       "Rig",
	fitFamilySubsystem: "Subsystem",
}

// fitVisualGroupCaptions are the short in-ring captions, in circular
// order around the ship (the same order as fitVisualArcs).
var fitVisualGroupCaptions = map[string]string{
	fitFamilyHigh:      "HIGH",
	fitFamilyMedium:    "MID",
	fitFamilyLow:       "LOW",
	fitFamilyRig:       "RIG",
	fitFamilySubsystem: "SUBSYSTEM",
}

// fitVisualGroupOrder is the circular order of slot groups around
// the ship; separators sit at each rendered pair's shared boundary.
var fitVisualGroupOrder = []string{
	fitFamilyHigh,
	fitFamilyMedium,
	fitFamilyLow,
	fitFamilyRig,
	fitFamilySubsystem,
}

// fitVisualMetaName maps the SDE meta group (attribute 1692) to
// the player's vocabulary.
func fitVisualMetaName(group int) string {
	switch group {
	case 2:
		return "Tech II"
	case 3:
		return "Storyline"
	case 4:
		return "Faction"
	case 5:
		return "Officer"
	case 6:
		return "Deadspace"
	default:
		return "Tech I"
	}
}

// fitVisualNum formats a module CPU/PG figure the way the stat
// panel does: whole numbers stay whole.
func fitVisualNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// fitArcPt places a point on the visual's 0-100 box: radius r at
// angle deg (0 = east, positive clockwise in screen coords).
func fitArcPt(r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return 50 + r*math.Cos(rad), 50 + r*math.Sin(rad)
}

// fitSegPath is the SVG path for one bordered group segment: an
// annular sector from a0 to a1 degrees between radii rIn and rOut.
// Angles run clockwise (screen coords), so the outer arc sweeps
// with flag 1 and the returning inner arc with flag 0.
func fitSegPath(a0, a1, rIn, rOut float64) string {
	x0o, y0o := fitArcPt(rOut, a0)
	x1o, y1o := fitArcPt(rOut, a1)
	x1i, y1i := fitArcPt(rIn, a1)
	x0i, y0i := fitArcPt(rIn, a0)
	large := 0
	if a1-a0 > 180 {
		large = 1
	}
	return fmt.Sprintf("M%.2f,%.2f A%.2f,%.2f 0 %d 1 %.2f,%.2f L%.2f,%.2f A%.2f,%.2f 0 %d 0 %.2f,%.2f Z",
		x0o, y0o, rOut, rOut, large, x1o, y1o, x1i, y1i, rIn, rIn, large, x0i, y0i)
}

// fitMetaGroupID is the SDE attribute carrying the inv meta group.
const fitMetaGroupID = 1692

// fitVisualMetaOf reads the static SDE meta group for a type —
// meta level is a property of the type itself, never modified by
// skills or bonuses, so it comes from the raw snapshot, not the
// effective attribute map. Returns "" when unknown.
func fitVisualMetaOf(snap *fit.Snapshot, eff map[int64]float64, typeID int64) string {
	if snap != nil {
		if a := snap.Attrs[typeID]; a != nil {
			if g, ok := a[fitMetaGroupID]; ok && g != 0 {
				return fitVisualMetaName(int(g))
			}
		}
	}
	if eff != nil {
		if g := eff[fitMetaGroupID]; g != 0 {
			return fitVisualMetaName(int(g))
		}
	}
	return ""
}

// fitBuildVisual shapes the visual fit display from the engine
// result and the document's own lines: one circle per slot,
// filled circles carrying their module, positioned on arcs around
// the ship render. Positions are precomputed here so the template
// stays declarative. snap may be nil (tests); then the tooltip
// figures stay empty.
func fitBuildVisual(res *fit.Result, doc *fitDoc, snap *fit.Snapshot, familyOf map[int64]string, nameOf func(int64) string, supportsSubsystems bool) *fitVisualView {
	v := &fitVisualView{ShipID: doc.ShipTypeID, ShipName: nameOf(doc.ShipTypeID)}
	maxOf := map[string]int{
		fitFamilyHigh:   res.HighSlots,
		fitFamilyMedium: res.MediumSlots,
		fitFamilyLow:    res.LowSlots,
		fitFamilyRig:    res.RigSlots,
	}
	// Fitted modules per family, quantities expanded, doc order.
	// Each placed module carries its resolved state and its
	// per-type ordinal so the tooltip can address the instance.
	type fitPlacedModule struct {
		TypeID  int64
		State   string
		Ordinal int
	}
	fitted := map[string][]fitPlacedModule{}
	ordinals := map[int64]int{}
	for _, it := range doc.Items {
		fam := familyOf[it.TypeID]
		if _, ok := fitVisualArcs[fam]; !ok {
			continue
		}
		for i := 0; i < it.Qty; i++ {
			st := ""
			if i < len(it.States) {
				st = it.States[i]
			}
			if snap != nil {
				st = fit.NormalizeModuleState(snap, it.TypeID, st)
			}
			fitted[fam] = append(fitted[fam], fitPlacedModule{TypeID: it.TypeID, State: st, Ordinal: ordinals[it.TypeID]})
			ordinals[it.TypeID]++
		}
	}
	maxOf[fitFamilySubsystem] = 0
	if supportsSubsystems || len(fitted[fitFamilySubsystem]) > 0 {
		maxOf[fitFamilySubsystem] = len(fitted[fitFamilySubsystem])
	}
	rendered := map[string]bool{}
	for _, fam := range []string{fitFamilyHigh, fitFamilyMedium, fitFamilyLow, fitFamilyRig, fitFamilySubsystem} {
		arc := fitVisualArcs[fam]
		max := maxOf[fam]
		if max <= 0 {
			continue
		}
		rendered[fam] = true
		span := arc[1] - arc[0]
		ids := fitted[fam]
		if len(ids) > max {
			ids = ids[:max]
		}
		for i := 0; i < max; i++ {
			ang := (arc[0] + (float64(i)+0.5)*span/float64(max)) * math.Pi / 180
			s := fitVisualSlot{
				GroupKey:   fam,
				GroupLabel: fitVisualGroupLabels[fam],
				Index:      i,
				X:          50 + arc[2]*math.Cos(ang),
				Y:          50 + arc[2]*math.Sin(ang),
			}
			if i < len(ids) {
				pm := ids[i]
				s.Filled = true
				s.TypeID = pm.TypeID
				s.Name = nameOf(pm.TypeID)
				s.State = pm.State
				s.TipKey = fmt.Sprintf("%d:%d", pm.TypeID, pm.Ordinal)
				if snap != nil {
					if vs := fit.ModuleValidStates(snap, pm.TypeID); len(vs) > 0 {
						s.ValidStates = vs
						s.StatesCSV = strings.Join(vs, ",")
					}
				}
				// Tooltip values are post-dogma: the engine's
				// per-module effective attributes (skills + hull
				// bonuses applied), not raw SDE rows. A module in
				// a non-default state reads its own state's
				// numbers (an overheated module shows heated).
				eff := res.ItemAttrs[pm.TypeID]
				if pm.State != "" {
					if sa := res.StateAttrs[fit.StateAttrKey(pm.TypeID, pm.State)]; sa != nil {
						eff = sa
					}
				}
				if eff == nil && snap != nil {
					eff = snap.Attrs[pm.TypeID]
				}
				if eff != nil {
					if cpu := eff[fit.AttrCPU]; cpu > 0 {
						s.CPU = fitVisualNum(cpu) + " tf"
					}
					if pg := eff[fit.AttrPower]; pg > 0 {
						s.PG = fitVisualNum(pg) + " MW"
					}
					s.Meta = fitVisualMetaOf(snap, eff, pm.TypeID)
					// Headline effective stat (post-dogma): web
					// strength and similar speedFactor bonuses
					// show the skill-scaled value, not base.
					if sf := eff[fit.AttrSpeedFactor]; sf != 0 {
						if sf < 0 {
							s.Stat = "Strength " + fitVisualNum(-sf) + "%"
						} else {
							s.Stat = "Boost " + fitVisualNum(sf) + "%"
						}
					}
				}
				// Loaded charge rides the slot as a badge; the
				// tooltip names it too.
				if chID := doc.Charges[pm.TypeID]; chID > 0 {
					s.ChargeID = chID
					s.ChargeName = nameOf(chID)
				}
			}
			v.Slots = append(v.Slots, s)
		}
	}
	// Ember dividers between rendered groups, on the ring at their
	// shared boundary angle, plus an in-ring caption per group so
	// the rings read as distinct in-game-like sections.
	for i, fam := range fitVisualGroupOrder {
		next := fitVisualGroupOrder[(i+1)%len(fitVisualGroupOrder)]
		arc := fitVisualArcs[fam]
		if rendered[fam] {
			mid := (arc[0] + arc[1]) / 2 * math.Pi / 180
			v.Labels = append(v.Labels, fitVisualLabel{
				X:    50 + 30*math.Cos(mid),
				Y:    50 + 30*math.Sin(mid),
				Text: fitVisualGroupCaptions[fam],
			})
		}
		if rendered[fam] && rendered[next] {
			end := arc[1] * math.Pi / 180
			v.Separators = append(v.Separators, fitVisualSep{
				X:   50 + arc[2]*math.Cos(end),
				Y:   50 + arc[2]*math.Sin(end),
				Rot: 90 - arc[1],
			})
		}
		// Bordered arc segment behind the group's slots: the band
		// the slot ring (radius arc[2]) sits inside.
		if rendered[fam] {
			v.Segs = append(v.Segs, fitVisualSeg{
				D: fitSegPath(arc[0], arc[1], arc[2]-4.5, arc[2]+4.5),
			})
		}
	}
	return v
}

// fitChargeOption is one selectable charge for a weapon group.
type fitChargeOption struct {
	ID       int64
	Name     string
	Selected bool
}

// fitChargeSet is the ammunition picker for one weapon type.
type fitChargeSet struct {
	WeaponID   int64
	WeaponName string
	Options    []fitChargeOption
}

// fitBarRow is one resource bar (powergrid, CPU).
type fitBarRow struct {
	Label string
	Used  string
	Max   string
	Left  string
	Pct   float64 // bar fill, 0..100
	Over  bool
}

// fitResistRow is one tank layer's line.
type fitResistRow struct {
	Layer     string
	HP        string
	EM        string
	Thermal   string
	Kinetic   string
	Explosive string
	EHP       string
}

// fitStatsView is the pyfa-vocabulary stat panel, preformatted.
type fitStatsView struct {
	Powergrid     fitBarRow
	CPU           fitBarRow
	Align         string
	Velocity      string
	Signature     string
	Capacitor     string // capacity
	CapPeak       string
	CapDraw       string
	CapState      string
	CapStable     bool
	TurretDPS     string
	MissileDPS    string
	DroneDPS      string
	TotalDPS      string
	TurretVolley  string
	MissileVolley string
	DroneVolley   string
	TotalVolley   string
	NeutDrain     string
	NosDrain      string
	Tank          []fitResistRow
	EHPOmni       string
	ShieldReg     string
	ShieldBoost   string
	ArmorRep      string
	HullRep       string
	TargetRng     string
	ScanRes       string
	Sensor        string
	LockedTgts    string
	Cargo         string
	DroneBW       string
	DroneBay      string
	DronesUp      string
	Hardpoints    string // "Turrets 3/4 · Launchers 0/0"
	HPOver        bool
	Calibration   string
	CalibOver     bool
	SlotLine      string // "High 3/3 · Medium 2/3 · Low 1/2 · Rigs 0/3"
	SlotsOver     bool
}

// fitMissingSkill is one unmet requirement for the chosen pilot.
type fitMissingSkill struct {
	Name string
	Have int
	Need int
}

// fitSimView is the workbench fragment (and its server-rendered
// first paint inside the Fittings page).
// fitCloneOption is one entry in the editor's Clone dropdown:
// the active clone (ID 0) or one of the pilot's jump clones.
type fitCloneOption struct {
	ID       int64
	Name     string // "Active clone" or the clone's name + location
	Implants int    // implant count, shown in the label
}

// fitImplantEntry is one applied implant for the compact list
// under the workbench.
type fitImplantEntry struct {
	TypeID int64
	Name   string
}

type fitSimView struct {
	HasShip    bool
	DataNote   string // ship data still downloading, skills still warming, ...
	ShipID     int64
	ShipName   string
	FitName    string
	PilotID    int64
	PilotLabel string
	StateJSON  string // canonical fit document for the editor script
	// CloneID is the selected clone (0 = active clone); Clones
	// feeds the Clone dropdown; Implants is the compact applied
	// list; ImplantNote covers warming / re-login states.
	CloneID     int64
	Clones      []fitCloneOption
	Implants    []fitImplantEntry
	ImplantNote string
	Groups      []fitSlotGroup
	Visual      *fitVisualView // in-game-style ship + slot rings
	DroneBWNum  float64        // drone bandwidth (for the fittable filter)
	DroneBayNum float64        // drone bay m3 (for the fittable filter)
	// SupportsSubsystems: the hull is a strategic cruiser (SDE
	// group), so the subsystem ring, group, and search family show.
	SupportsSubsystems bool
	// RestrictedJSON maps restricted module type IDs to their
	// requirement label ("Dreadnoughts"), for the client-side
	// add guard and drag highlighting.
	RestrictedJSON string
	ChargeSets     []fitChargeSet
	Stats          *fitStatsView
	Missing        []fitMissingSkill
	Notes          []string // plain-language modeling notes
}

// fitEditorView is the editor chrome around the workbench.
type fitEditorView struct {
	PilotID int64
	Pilots  []assetCharLink // the user's characters; the template adds All V
	// CloneID/Clones feed the Clone dropdown next to the pilot
	// picker (copied from the initial Sim; the dropdown itself
	// lives in the chrome so it survives workbench re-renders).
	CloneID     int64
	Clones      []fitCloneOption
	EVECharID   int64 // active character: the Save-to-EVE target (0 = none)
	EVECharName string
	Description string // fit metadata, editable in the header, autosaved
	Tags        string // comma-separated for the header field; stored as an array
	TagsList    []string
	IsPublic    bool
	Sim         *fitSimView
	LocalID     int64    // saved fit currently open (0 = unsaved)
	Notes       []string // import notes ("couldn't place ..."), shown once
	LocalFits   []localFitEntry
}

// localFitEntry is one saved fit for the your-fits list.
type localFitEntry struct {
	ID          int64
	Name        string
	ShipName    string
	Description string
	Tags        []string
	Updated     string
	IsPublic    bool
	IsDraft     bool
}

// ---------------------------------------------------------------------------
// Simulate: doc + pilot -> snapshot -> engine -> fragment.
// ---------------------------------------------------------------------------

// fitTypeIDs collects every type a fit document touches.
func fitDocTypeIDs(doc *fitDoc) []int64 {
	ids := []int64{doc.ShipTypeID}
	for _, it := range doc.Items {
		ids = append(ids, it.TypeID)
	}
	for _, charge := range doc.Charges {
		ids = append(ids, charge)
	}
	return ids
}

// fitPilotLevels resolves the simulate request's pilot levels:
// pilotID 0 is the All-V view; otherwise the pilot's trained
// skills load from their stored snapshot. The second return is a
// note when the pilot's skill snapshot hasn't landed yet. The
// caller owns pilot labeling and ownership checks.
func (app *Application) fitPilotLevels(ctx context.Context, pilotID int64, snap *fit.Snapshot, doc *fitDoc) (map[int64]int, string) {
	if pilotID == 0 {
		itemIDs := make([]int64, 0, len(doc.Items))
		for _, it := range doc.Items {
			itemIDs = append(itemIDs, it.TypeID)
		}
		return fit.AllVSkillLevels(snap, doc.ShipTypeID, itemIDs), ""
	}
	var skills esi.Skills
	if !app.loadCorpSnapshot(ctx, pilotID, esi.SnapSkills, &skills) {
		return map[int64]int{}, "This pilot's skills are still on their way in — the fit below is calculated with no skills applied."
	}
	return briefingSkillLevels(skills), ""
}

// fitPilotImplants resolves the simulate request's implant set. pilotID
// 0 (All V) has no character and so no implants; cloneID 0 is the
// active clone, otherwise the matching jump clone. Clone labels fall
// back to "Jump clone #<last 4 digits>" when ESI carries no name. Reads
// worker-warmed snapshots only. The second return is the effective
// clone ID (unknown IDs fall back to active); the note covers warming
// and re-login states.
func (app *Application) fitPilotImplants(ctx context.Context, pilotID, cloneID int64) (ids []int64, effectiveClone int64, clones []fitCloneOption, note string) {
	if pilotID == 0 {
		return nil, 0, nil, ""
	}
	var active esi.Implants
	var jc esi.Clones
	activeOK := app.loadCorpSnapshot(ctx, pilotID, esi.SnapImplants, &active)
	clonesOK := app.loadCorpSnapshot(ctx, pilotID, esi.SnapClones, &jc)
	if !activeOK || !clonesOK {
		if st, _, found := app.corpKindState(ctx, pilotID, esi.SnapImplants); found && st == fetchStateRoleMissing {
			return nil, 0, nil, "This character was linked before EveSynapse asked for clone access — sign in again to enable implants."
		}
		return nil, 0, nil, "This pilot's implant data is still on its way in — the fit below is calculated with no implants applied."
	}
	clones = []fitCloneOption{{ID: 0, Name: "Active clone", Implants: len(active)}}
	for _, c := range jc.JumpClones {
		name := strings.TrimSpace(c.Name)
		if name == "" || name == "Jump clone" {
			// EVE doesn't let players name jump clones, so ESI's
			// name field is usually empty or generic. Fall back to
			// the clone ID's last four digits so the dropdown
			// options stay distinguishable. A real custom name is
			// always kept. The implant count is NOT folded in here:
			// both the workbench template and handleFitClonesJSON
			// append " (n)" themselves, so it appears exactly once.
			name = fmt.Sprintf("Jump clone #%04d", c.JumpCloneID%10000)
		}
		loc := app.locationTitle(ctx, c.LocationID, c.LocationType)
		clones = append(clones, fitCloneOption{
			ID:       c.JumpCloneID,
			Name:     name + " — " + loc,
			Implants: len(c.Implants),
		})
	}
	ids = append(ids, active...)
	for _, c := range jc.JumpClones {
		if cloneID != 0 && c.JumpCloneID == cloneID {
			ids = append([]int64{}, c.Implants...)
			effectiveClone = cloneID
			break
		}
	}
	return ids, effectiveClone, clones, ""
}

// buildFitSimView runs the engine for one document and pilot and
// shapes the result for the workbench template. pilotID 0 = All V.
// shipSupportsSubsystems reports whether the hull can fit
// subsystems. The four subsystem slots are a class rule, not a
// hull attribute, so the data-driven signal is the ship's SDE
// group: Strategic Cruiser. No hull list.
func (app *Application) shipSupportsSubsystems(ctx context.Context, snap *fit.Snapshot, shipTypeID int64) bool {
	gid, ok := snap.Groups[shipTypeID]
	if !ok || gid == 0 {
		return false
	}
	names := app.esi.CachedGroupNames(ctx, []int64{gid})
	return names[gid] == "Strategic Cruiser"
}

func (app *Application) buildFitSimView(ctx context.Context, doc *fitDoc, pilotID int64, pilotLabel string, cloneID int64) *fitSimView {
	view := &fitSimView{PilotID: pilotID, FitName: doc.Name}
	state, _ := json.Marshal(doc)
	view.StateJSON = string(state)
	if doc.ShipTypeID <= 0 {
		return view
	}

	// Implants resolve before the snapshot load so their SDE
	// types join the bounded type list.
	implantIDs, effectiveClone, clones, implantNote := app.fitPilotImplants(ctx, pilotID, cloneID)
	view.CloneID = effectiveClone
	view.Clones = clones
	view.ImplantNote = implantNote
	typeIDs := fitDocTypeIDs(doc)
	typeIDs = append(typeIDs, implantIDs...)
	snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs)
	if err != nil {
		logging.Errorf("fittings sim: load snapshot for ship %d: %v", doc.ShipTypeID, err)
		view.DataNote = "Something went wrong reading the ship data — check the server log."
		return view
	}
	if len(snap.Attrs[doc.ShipTypeID]) == 0 {
		view.DataNote = "The ship and module data is still downloading — give it a little while and try again."
		return view
	}
	view.HasShip = true
	view.SupportsSubsystems = app.shipSupportsSubsystems(ctx, snap, doc.ShipTypeID)

	levels, note := app.fitPilotLevels(ctx, pilotID, snap, doc)
	view.PilotLabel = pilotLabel
	if note != "" {
		view.DataNote = note
	}

	// Classify the fit's lines; only fitted families reach the
	// engine (cargo is display-only). Modules expand per state:
	// identical modules in different states simulate as separate
	// engine inputs so each binds only its state's categories.
	familyOf := make(map[int64]string, len(doc.Items))
	engineItems := make([]fit.ItemInput, 0, len(doc.Items))
	for _, it := range doc.Items {
		fam := fitSlotFamilyOf(snap, it.TypeID)
		familyOf[it.TypeID] = fam
		if !fitFamilyFitted(fam) {
			continue
		}
		byState := make(map[string]int, 2)
		var order []string
		for i := 0; i < it.Qty; i++ {
			st := ""
			if i < len(it.States) {
				st = it.States[i]
			}
			if _, ok := byState[st]; !ok {
				order = append(order, st)
			}
			byState[st]++
		}
		for _, st := range order {
			engineItems = append(engineItems, fit.ItemInput{TypeID: it.TypeID, Quantity: byState[st], State: st})
		}
	}
	res := fit.Compute(snap, doc.ShipTypeID, engineItems, levels, doc.Charges, implantIDs)

	// Names for everything the fragment prints.
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok && n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", id)
	}
	view.ShipID = doc.ShipTypeID
	view.ShipName = nameOf(doc.ShipTypeID)
	for _, id := range implantIDs {
		view.Implants = append(view.Implants, fitImplantEntry{TypeID: id, Name: nameOf(id)})
	}

	view.Groups = fitBuildGroups(res, doc, snap, familyOf, nameOf, view.SupportsSubsystems)
	view.Visual = fitBuildVisual(res, doc, snap, familyOf, nameOf, view.SupportsSubsystems)
	view.DroneBWNum = res.DroneBandwidth
	view.DroneBayNum = res.DroneBayCapacity
	view.ChargeSets = app.fitBuildChargeSets(ctx, doc, snap, nameOf)
	view.Stats = fitBuildStats(res)
	view.Missing = fitMissingList(ctx, app, snap, doc, engineItems, levels, pilotID)
	view.Notes = fitPlainNotes(res.Unmodeled)
	view.RestrictedJSON = app.fitRestrictedJSON(ctx, res.Restricted, nameOf)
	for _, r := range res.Restricted {
		view.Notes = append(view.Notes,
			fmt.Sprintf("%s can only be fitted to %s.", nameOf(r.TypeID), app.fitRestrictionLabel(ctx, r)))
	}
	return view
}

// fitRestrictionLabel renders a fit.Restriction's requirement in
// plain language: "Dreadnoughts", "the Rorqual", or "X or Y".
func (app *Application) fitRestrictionLabel(ctx context.Context, r fit.Restriction) string {
	var parts []string
	if len(r.NeedGroup) > 0 {
		gids := make([]int64, 0, len(r.NeedGroup))
		gids = append(gids, r.NeedGroup...)
		names := app.esi.CachedGroupNames(ctx, gids)
		for _, g := range r.NeedGroup {
			if n, ok := names[g]; ok && n != "" {
				parts = append(parts, n+"s")
			}
		}
	}
	for _, t := range r.NeedType {
		names := app.esi.CachedTypeNames(ctx, []int64{t})
		if n, ok := names[t]; ok && n != "" {
			parts = append(parts, "the "+n)
		}
	}
	if len(parts) == 0 {
		return "specific ships"
	}
	return strings.Join(parts, " or ")
}

// fitRestrictedJSON builds the client-side restriction map:
// module type ID -> requirement label.
func (app *Application) fitRestrictedJSON(ctx context.Context, rs []fit.Restriction, nameOf func(int64) string) string {
	if len(rs) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(rs))
	for _, r := range rs {
		m[strconv.FormatInt(r.TypeID, 10)] = app.fitRestrictionLabel(ctx, r)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// engineItemsAll returns engine inputs plus charge types, for
// requirement gathering (ammo skills count too).
func engineItemsAll(engineItems []fit.ItemInput, doc *fitDoc) []fit.ItemInput {
	out := append([]fit.ItemInput{}, engineItems...)
	for _, charge := range doc.Charges {
		out = append(out, fit.ItemInput{TypeID: charge, Quantity: 1})
	}
	return out
}

// fitMissingList names the missing skills for a real pilot (the
// All-V view never misses anything).
func fitMissingList(ctx context.Context, app *Application, snap *fit.Snapshot, doc *fitDoc, engineItems []fit.ItemInput, levels map[int64]int, pilotID int64) []fitMissingSkill {
	if pilotID == 0 {
		return nil
	}
	need := make(map[int64]int64)
	seenType := map[int64]bool{}
	queue := []int64{doc.ShipTypeID}
	for _, it := range engineItemsAll(engineItems, doc) {
		queue = append(queue, it.TypeID)
	}
	for len(queue) > 0 {
		typeID := queue[0]
		queue = queue[1:]
		if seenType[typeID] {
			continue
		}
		seenType[typeID] = true
		for _, req := range snap.Requirements[typeID] {
			if need[req.SkillTypeID] < req.Level {
				need[req.SkillTypeID] = req.Level
			}
			queue = append(queue, req.SkillTypeID)
		}
	}
	var ids []int64
	for skillID, lvl := range need {
		if int64(levels[skillID]) < lvl {
			ids = append(ids, skillID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	names := app.esi.CachedTypeNames(ctx, ids)
	out := make([]fitMissingSkill, 0, len(ids))
	for _, skillID := range ids {
		name := names[skillID]
		if name == "" {
			name = fmt.Sprintf("Type #%d", skillID)
		}
		out = append(out, fitMissingSkill{Name: name, Have: levels[skillID], Need: int(need[skillID])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fitBuildGroups shapes the slot grid from the engine result and
// the document's own lines.
func fitBuildGroups(res *fit.Result, doc *fitDoc, snap *fit.Snapshot, familyOf map[int64]string, nameOf func(int64) string, supportsSubsystems bool) []fitSlotGroup {
	chips := map[string][]fitSlotChip{}
	for _, it := range doc.Items {
		fam := familyOf[it.TypeID]
		// Uniform resolved state across the line's instances;
		// mixed lines carry "" (no single state to show).
		st, uniform := "", true
		for i := 0; i < it.Qty; i++ {
			s := ""
			if i < len(it.States) {
				s = it.States[i]
			}
			if snap != nil {
				s = fit.NormalizeModuleState(snap, it.TypeID, s)
			}
			if i == 0 {
				st = s
			} else if s != st {
				uniform = false
				break
			}
		}
		if !uniform {
			st = ""
		}
		chips[fam] = append(chips[fam], fitSlotChip{TypeID: it.TypeID, Name: nameOf(it.TypeID), Qty: it.Qty, State: st})
	}
	for fam := range chips {
		sort.Slice(chips[fam], func(i, j int) bool { return chips[fam][i].Name < chips[fam][j].Name })
	}
	mk := func(key, label string, used, max int, hasMax bool) fitSlotGroup {
		g := fitSlotGroup{Key: key, Label: label, Used: used, Max: max, HasMax: hasMax, Chips: chips[key]}
		if hasMax {
			g.Over = used > max
			for i := used; i < max; i++ {
				g.Empty = append(g.Empty, i)
			}
		}
		return g
	}
	droneUsed := 0
	for _, c := range chips[fitFamilyDrone] {
		droneUsed += c.Qty
	}
	cargoCount := 0
	for _, c := range chips[fitFamilyCargo] {
		cargoCount += c.Qty
	}
	groups := []fitSlotGroup{
		mk(fitFamilyHigh, "High slots", res.HighSlotsUsed, res.HighSlots, true),
		mk(fitFamilyMedium, "Mid slots", res.MediumSlotsUsed, res.MediumSlots, true),
		mk(fitFamilyLow, "Low slots", res.LowSlotsUsed, res.LowSlots, true),
		mk(fitFamilyRig, "Rigs", res.RigSlotsUsed, res.RigSlots, true),
	}
	// Subsystems only where they belong: strategic-cruiser hulls.
	// Fitted subsystems still show on any hull so an import never
	// hides modules, but the empty group does not.
	if supportsSubsystems || len(chips[fitFamilySubsystem]) > 0 {
		groups = append(groups, mk(fitFamilySubsystem, "Subsystems", len(chips[fitFamilySubsystem]), 0, false))
	}
	groups = append(groups,
		mk(fitFamilyDrone, "Drones", droneUsed, 0, false),
		mk(fitFamilyCargo, "Cargo", cargoCount, 0, false),
	)
	return groups
}

// fitChargeCandidates lists the charge types a weapon can load:
// the types in its charge groups, size-matched when the weapon
// declares a charge size.
func (app *Application) fitChargeCandidates(ctx context.Context, weaponTypeID int64) []db.ListFitChargeTypesRow {
	attrs, err := app.queries.ListSDETypeAttributes(ctx, weaponTypeID)
	if err != nil {
		logging.Errorf("fittings sim: weapon %d attributes: %v", weaponTypeID, err)
		return nil
	}
	var groups []int64
	var size float64
	for _, a := range attrs {
		switch {
		case a.AttributeID == fitAttrChargeSize:
			size = a.Value
		default:
			for _, g := range fitChargeGroupAttrs {
				if a.AttributeID == g && a.Value > 0 {
					groups = append(groups, int64(a.Value))
				}
			}
		}
	}
	if len(groups) == 0 {
		return nil
	}
	rows, err := app.queries.ListFitChargeTypes(ctx, db.ListFitChargeTypesParams{
		GroupIds: groups, ChargeSize: size,
	})
	if err != nil {
		logging.Errorf("fittings sim: charge candidates for %d: %v", weaponTypeID, err)
		return nil
	}
	return rows
}

// fitBuildChargeSets builds the per-weapon ammunition pickers.
func (app *Application) fitBuildChargeSets(ctx context.Context, doc *fitDoc, snap *fit.Snapshot, nameOf func(int64) string) []fitChargeSet {
	var sets []fitChargeSet
	seen := map[int64]bool{}
	for _, it := range doc.Items {
		if seen[it.TypeID] {
			continue
		}
		seen[it.TypeID] = true
		if !snap.HasEffect(it.TypeID, fit.EffectTurretFitted) && !snap.HasEffect(it.TypeID, fit.EffectLauncherFitted) {
			continue
		}
		set := fitChargeSet{WeaponID: it.TypeID, WeaponName: nameOf(it.TypeID)}
		selected := doc.Charges[it.TypeID]
		for _, row := range app.fitChargeCandidates(ctx, it.TypeID) {
			set.Options = append(set.Options, fitChargeOption{
				ID: row.TypeID, Name: row.Name, Selected: row.TypeID == selected,
			})
		}
		// A charge the candidate query no longer lists (stale
		// fit) still shows, selected, rather than vanishing.
		if selected > 0 {
			found := false
			for _, opt := range set.Options {
				if opt.ID == selected {
					found = true
					break
				}
			}
			if !found {
				set.Options = append([]fitChargeOption{{ID: selected, Name: nameOf(selected), Selected: true}}, set.Options...)
			}
		}
		sets = append(sets, set)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].WeaponName < sets[j].WeaponName })
	return sets
}

// fitPlainNotes turns the engine's unmodeled-shape list into a
// few plain-language lines (never the raw engine strings).
func fitPlainNotes(raw []string) []string {
	var notes []string
	add := func(s string) {
		for _, n := range notes {
			if n == s {
				return
			}
		}
		notes = append(notes, s)
	}
	for _, s := range raw {
		switch {
		case strings.Contains(s, "has no charge selected"):
			add("Damage from turrets with no ammunition picked isn't counted yet — pick an ammo type to include it.")
		case strings.Contains(s, "has no missile selected"):
			add("Damage from launchers with no missiles picked isn't counted yet — pick a missile type to include it.")
		default:
			add("A few effects on this fit aren't fully calculated yet, so some bonuses may be missing.")
		}
	}
	if len(notes) > 4 {
		notes = notes[:4]
	}
	return notes
}

// ---------------------------------------------------------------------------
// Stat formatting.
// ---------------------------------------------------------------------------

// fitFmt1 formats with up to one decimal, thousands-grouped.
func fitFmt1(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return groupThousandsFit(s)
}

// fitFmt0 formats rounded, thousands-grouped.
func fitFmt0(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	return esi.FormatInt(int64(math.Round(v)))
}

// groupThousandsFit groups the integer part of a decimal string.
func groupThousandsFit(s string) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	if len(intPart) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	rest := len(intPart) % 3
	if rest > 0 {
		b.WriteString(intPart[:rest])
	}
	for i := rest; i < len(intPart); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	b.WriteString(frac)
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fitSeconds renders a duration the way the fitting window does.
func fitSeconds(secs float64) string {
	if secs <= 0 {
		return "over an hour"
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", int(math.Round(secs)))
	}
	m := int(secs) / 60
	s := int(secs) % 60
	return fmt.Sprintf("%dm %02ds", m, s)
}

// fitResist renders a resonance as a resist percentage.
func fitResist(resonance float64) string {
	pct := (1 - resonance) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return fitFmt1(pct) + "%"
}

// fitEHPText renders one EHP figure (infinite shows as a dash).
func fitEHPText(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	return fitFmt0(v)
}

// fitBar builds one resource bar row.
func fitBar(label string, used, max float64) fitBarRow {
	row := fitBarRow{Label: label, Used: fitFmt1(used), Max: fitFmt1(max)}
	left := max - used
	row.Left = fitFmt1(left)
	row.Over = used > max && max > 0
	if max > 0 {
		row.Pct = used / max * 100
		if row.Pct > 100 {
			row.Pct = 100
		}
		if row.Pct < 0 {
			row.Pct = 0
		}
	}
	return row
}

// fitBuildStats preformats the pyfa-vocabulary stat panel.
func fitBuildStats(res *fit.Result) *fitStatsView {
	st := &fitStatsView{}
	st.Powergrid = fitBar("Powergrid", res.PowergridUsed, res.PowergridMax)
	st.Powergrid.Used += " MW"
	st.Powergrid.Max += " MW"
	st.Powergrid.Left += " MW"
	st.CPU = fitBar("CPU", res.CPUUsed, res.CPUMax)
	st.CPU.Used += " tf"
	st.CPU.Max += " tf"
	st.CPU.Left += " tf"

	st.Capacitor = fitFmt1(res.CapacitorCapacity) + " GJ"
	st.CapPeak = fitFmt1(res.CapacitorPeakRecharge) + " GJ/s"
	st.CapDraw = fitFmt1(res.CapacitorDraw) + " GJ/s"
	if res.CapacitorStable {
		st.CapStable = true
		if res.CapacitorStablePercent >= 99.95 {
			st.CapState = "Stable"
		} else {
			st.CapState = fmt.Sprintf("Stable at %s%%", fitFmt1(res.CapacitorStablePercent))
		}
	} else {
		st.CapState = "Runs out in " + fitSeconds(res.CapacitorDepletionSecs)
	}

	st.TurretDPS = fitFmt1(res.TurretDPS)
	st.MissileDPS = fitFmt1(res.MissileDPS)
	st.DroneDPS = fitFmt1(res.DroneDPS)
	st.TotalDPS = fitFmt1(res.DPS)

	st.TurretVolley = fitFmt1(res.TurretVolley)
	st.MissileVolley = fitFmt1(res.MissileVolley)
	st.DroneVolley = fitFmt1(res.DroneVolley)
	st.TotalVolley = fitFmt1(res.Volley)

	if res.NeutDrainPerSec > 0 {
		st.NeutDrain = fmt.Sprintf("%s GJ/s (%s GJ per cycle)",
			fitFmt1(res.NeutDrainPerSec), fitFmt1(res.NeutDrainPerCycle))
	}
	if res.NosDrainPerSec > 0 {
		st.NosDrain = fmt.Sprintf("%s GJ/s (%s GJ per cycle)",
			fitFmt1(res.NosDrainPerSec), fitFmt1(res.NosDrainPerCycle))
	}

	st.Tank = []fitResistRow{
		{Layer: "Shield", HP: fitFmt0(res.ShieldHP),
			EM: fitResist(resistOf(res, 0, true)), Thermal: fitResist(resistOf(res, 1, true)),
			Kinetic: fitResist(resistOf(res, 2, true)), Explosive: fitResist(resistOf(res, 3, true)),
			EHP: fitEHPText(res.ShieldEHP.Omni)},
		{Layer: "Armor", HP: fitFmt0(res.ArmorHP),
			EM: fitResist(resistOf(res, 0, false)), Thermal: fitResist(resistOf(res, 1, false)),
			Kinetic: fitResist(resistOf(res, 2, false)), Explosive: fitResist(resistOf(res, 3, false)),
			EHP: fitEHPText(res.ArmorEHP.Omni)},
	}
	// Hull resists ride the generic resonance attributes in the
	// engine's ShipAttrs; the layer row uses EHP only for resists.
	st.Tank = append(st.Tank, fitResistRow{Layer: "Hull", HP: fitFmt0(res.HullHP),
		EM: fitResist(res.ShipAttrs[fit.AttrResonanceEM]), Thermal: fitResist(res.ShipAttrs[fit.AttrResonanceThermal]),
		Kinetic: fitResist(res.ShipAttrs[fit.AttrResonanceKinetic]), Explosive: fitResist(res.ShipAttrs[fit.AttrResonanceExplosive]),
		EHP: fitEHPText(res.HullEHP.Omni)})
	st.EHPOmni = fitEHPText(res.EHP.Omni)

	st.ShieldReg = fitFmt1(res.ShieldRegenPeak) + " HP/s"
	st.ShieldBoost = fitFmt1(res.ShieldBoostRate) + " HP/s"
	st.ArmorRep = fitFmt1(res.ArmorRepairRate) + " HP/s"
	st.HullRep = fitFmt1(res.HullRepairRate) + " HP/s"

	st.Velocity = fitFmt0(res.Velocity) + " m/s"
	st.Align = fitFmt1(res.AlignSeconds) + " s"
	st.Signature = fitFmt0(res.SignatureRadius) + " m"

	if res.MaxTargetRange >= 1000 {
		st.TargetRng = fitFmt1(res.MaxTargetRange/1000) + " km"
	} else {
		st.TargetRng = fitFmt0(res.MaxTargetRange) + " m"
	}
	st.ScanRes = fitFmt0(res.ScanResolution) + " mm"
	st.Sensor = fitFmt1(res.SensorStrength)
	st.LockedTgts = fitFmt0(res.MaxLockedTargets)
	st.Cargo = fitFmt1(res.CargoCapacity) + " m³"

	st.DroneBW = fmt.Sprintf("%s / %s Mbit/s", fitFmt1(res.DroneBandwidthUsed), fitFmt1(res.DroneBandwidth))
	st.DroneBay = fmt.Sprintf("%s / %s m³", fitFmt1(res.DroneBayUsed), fitFmt1(res.DroneBayCapacity))
	st.DronesUp = fmt.Sprintf("%d in space of %d carried", res.DronesActive, res.DronesFitted)

	st.Hardpoints = fmt.Sprintf("Turrets %d/%d · Launchers %d/%d",
		res.TurretHardpointsUsed, res.TurretHardpoints, res.LauncherHardpointsUsed, res.LauncherHardpoints)
	st.HPOver = res.TurretHardpointsUsed > res.TurretHardpoints || res.LauncherHardpointsUsed > res.LauncherHardpoints
	st.Calibration = fmt.Sprintf("%s / %s", fitFmt0(res.CalibrationUsed), fitFmt0(res.CalibrationMax))
	st.CalibOver = res.CalibrationUsed > res.CalibrationMax && res.CalibrationMax > 0
	st.SlotLine = fmt.Sprintf("High %d/%d · Mid %d/%d · Low %d/%d · Rigs %d/%d",
		res.HighSlotsUsed, res.HighSlots, res.MediumSlotsUsed, res.MediumSlots,
		res.LowSlotsUsed, res.LowSlots, res.RigSlotsUsed, res.RigSlots)
	st.SlotsOver = res.HighSlotsUsed > res.HighSlots || res.MediumSlotsUsed > res.MediumSlots ||
		res.LowSlotsUsed > res.LowSlots || res.RigSlotsUsed > res.RigSlots
	return st
}

// resistOf reads a layer resonance back out of the computed
// ship attributes (shield when shield=true, armor otherwise);
// kind: 0 EM, 1 thermal, 2 kinetic, 3 explosive.
func resistOf(res *fit.Result, kind int, shield bool) float64 {
	attrs := res.ShipAttrs
	switch {
	case shield && kind == 0:
		return attrs[fit.AttrShieldEM]
	case shield && kind == 1:
		return attrs[fit.AttrShieldThermal]
	case shield && kind == 2:
		return attrs[fit.AttrShieldKinetic]
	case shield && kind == 3:
		return attrs[fit.AttrShieldExplosive]
	case !shield && kind == 0:
		return attrs[fit.AttrArmorEM]
	case !shield && kind == 1:
		return attrs[fit.AttrArmorThermal]
	case !shield && kind == 2:
		return attrs[fit.AttrArmorKinetic]
	default:
		return attrs[fit.AttrArmorExplosive]
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers.
// ---------------------------------------------------------------------------

// fitSimRequest is the simulate/save/export request body: the fit
// document plus (for simulate) the pilot to compute for.
type fitSimRequest struct {
	fitDoc
	Pilot int64 `json:"pilot"`
	Clone int64 `json:"clone"` // jump clone ID; 0 = active clone
}

// readFitRequest decodes a fit JSON body (bounded) and sanitizes
// the document.
func readFitRequest(w http.ResponseWriter, r *http.Request) (fitSimRequest, bool) {
	var req fitSimRequest
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That fitting couldn't be read.", http.StatusBadRequest)
		return req, false
	}
	sanitizeFitDoc(&req.fitDoc)
	return req, true
}

// handleFitSimulate serves POST /fittings/simulate/: run the
// engine for the posted fit and return the workbench fragment.
func (app *Application) handleFitSimulate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := readFitRequest(w, r)
	if !ok {
		return
	}
	userID := app.userID(ctx)

	pilotLabel := "All V"
	if req.Pilot != 0 {
		label, owned := app.fitPilotLabel(ctx, userID, req.Pilot)
		if !owned {
			http.Error(w, "That pilot isn't one of your characters.", http.StatusBadRequest)
			return
		}
		pilotLabel = label
	}
	view := app.buildFitSimView(ctx, &req.fitDoc, req.Pilot, pilotLabel, req.Clone)
	app.renderFragment(w, "fittings.html", "fit-workbench", view)
}

// handleFitClonesJSON serves GET /fittings/clones.json: the
// Clone dropdown options for one pilot (active clone plus jump
// clones). Lets the editor refresh the dropdown when the pilot
// picker changes without a full page load.
func (app *Application) handleFitClonesJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	pilotID, _ := strconv.ParseInt(r.URL.Query().Get("pilot"), 10, 64)
	if _, owned := app.fitPilotLabel(ctx, userID, pilotID); !owned {
		writeFitJSON(w, map[string]any{"ok": false, "error": "That pilot isn't one of your characters."})
		return
	}
	_, effectiveClone, clones, note := app.fitPilotImplants(ctx, pilotID, 0)
	opts := make([]map[string]any, 0, len(clones))
	for _, c := range clones {
		label := c.Name
		if c.Implants > 0 {
			label = fmt.Sprintf("%s (%d)", c.Name, c.Implants)
		}
		opts = append(opts, map[string]any{"id": c.ID, "name": label})
	}
	writeFitJSON(w, map[string]any{"ok": true, "clones": opts, "clone": effectiveClone, "note": note})
}

// fitPilotLabel resolves a pilot character ID to its name when
// it belongs to the signed-in user.
func (app *Application) fitPilotLabel(ctx context.Context, userID, pilotID int64) (string, bool) {
	if userID == 0 || pilotID == 0 {
		return "", false
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("fittings sim: list characters for user %d: %v", userID, err)
		return "", false
	}
	for _, ch := range chars {
		if ch.CharacterID == pilotID {
			return ch.Name, true
		}
	}
	return "", false
}

// handleFitPickerJSON serves GET /fittings/picker.json: the
// editor's item feeds (ships, slot families, drones, charges, or
// the shared marketable pool), all local SDE reads.
func (app *Application) handleFitPickerJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	family := q.Get("family")
	query := q.Get("q")
	meta, _ := strconv.ParseInt(q.Get("meta"), 10, 64)
	pilotID, _ := strconv.ParseInt(q.Get("pilot"), 10, 64)
	usableOnly := q.Get("usable") == "1" && pilotID > 0

	// usableByPilot filters suggestions to what the pilot can actually
	// use: every skill requirement (transitively) met by their levels.
	usableByPilot := func(rows []suggestItem) []suggestItem {
		if !usableOnly || len(rows) == 0 {
			return rows
		}
		var skills esi.Skills
		if !app.loadCorpSnapshot(ctx, pilotID, esi.SnapSkills, &skills) {
			return rows // skills not warmed yet: don't hide everything
		}
		levels := briefingSkillLevels(skills)
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		// Requirement closure over a few rounds (module -> skill -> skill).
		reqs := make(map[int64][]db.SdeRequirement)
		pending := ids
		for round := 0; round < 4 && len(pending) > 0; round++ {
			batch, err := app.queries.ListSDERequirementsByTypes(ctx, pending)
			if err != nil {
				logging.Errorf("fittings picker: requirements: %v", err)
				return rows
			}
			var next []int64
			seen := make(map[int64]bool)
			for _, b := range batch {
				reqs[b.TypeID] = append(reqs[b.TypeID], b)
				if !seen[b.SkillTypeID] {
					seen[b.SkillTypeID] = true
					next = append(next, b.SkillTypeID)
				}
			}
			pending = next
		}
		usable := func(typeID int64) bool {
			seen := make(map[int64]bool)
			queue := []int64{typeID}
			for len(queue) > 0 {
				id := queue[0]
				queue = queue[1:]
				if seen[id] {
					continue
				}
				seen[id] = true
				for _, req := range reqs[id] {
					if int64(levels[req.SkillTypeID]) < req.Level {
						return false
					}
					queue = append(queue, req.SkillTypeID)
				}
			}
			return true
		}
		out := rows[:0]
		for _, row := range rows {
			if usable(row.ID) {
				out = append(out, row)
			}
		}
		return out
	}

	slotKinds := []struct {
		family string
		effect int64
		kind   string
		lim    int64
	}{
		{fitFamilyHigh, fit.EffectHiPower, "high", 8},
		{fitFamilyMedium, fit.EffectMedPower, "medium", 8},
		{fitFamilyLow, fit.EffectLoPower, "low", 8},
		{fitFamilyRig, fit.EffectRigSlot, "rig", 6},
		{fitFamilySubsystem, fit.EffectSubsystemSlot, "subsystem", 6},
	}
	querySlotFamily := func(family, kind string, lim int64) []suggestItem {
		effectID := fitSlotEffectByFamily[family]
		rows, err := app.queries.ListFitSlotTypes(ctx, db.ListFitSlotTypesParams{
			EffectID: effectID, Q: query, Lim: lim, Meta: meta,
		})
		if err != nil {
			logging.Errorf("fittings picker: family %s: %v", family, err)
			return nil
		}
		out := make([]suggestItem, 0, len(rows))
		for _, row := range rows {
			out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: kind})
		}
		return out
	}

	out := []suggestItem{}
	switch {
	case family == "all":
		// Unified search: ships plus every module family, grouped
		// by kind client-side. Charges are deliberately excluded:
		// without a fitted weapon they cannot be placed.
		if rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: query, Lim: 6}); err == nil {
			for _, row := range rows {
				out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "ship"})
			}
		} else {
			logging.Errorf("fittings picker: ships: %v", err)
		}
		for _, sk := range slotKinds {
			out = append(out, querySlotFamily(sk.family, sk.kind, sk.lim)...)
		}
		if rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 6, Meta: meta}); err == nil {
			for _, row := range rows {
				out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "drone"})
			}
		} else {
			logging.Errorf("fittings picker: drones: %v", err)
		}
	case family == "ship":
		rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: query, Lim: 12})
		if err != nil {
			logging.Errorf("fittings picker: ships: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "ship"})
		}
	case family == fitFamilyDrone:
		rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 25, Meta: meta})
		if err != nil {
			logging.Errorf("fittings picker: drones: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "drone"})
		}
	case family == "charge":
		weaponID, _ := strconv.ParseInt(q.Get("weapon"), 10, 64)
		if weaponID > 0 {
			for _, row := range app.fitChargeCandidates(ctx, weaponID) {
				out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Kind: "charge"})
			}
		}
	default:
		if _, isSlot := fitSlotEffectByFamily[family]; isSlot {
			out = append(out, querySlotFamily(family, family, 25)...)
			break
		}
		writeSuggestJSON(w, usableByPilot(app.suggestTypes(ctx, query, suggestPoolMarket, 12)))
		return
	}
	writeSuggestJSON(w, usableByPilot(out))
}

// parseFitTags turns the header's comma-separated tags field into
// a clean array: trimmed, deduped, capped.
func parseFitTags(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] || len(out) >= 20 {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// handleFitSave serves POST /fittings/save/: create or update one
// of the user's local fittings. The editor autosaves through this
// same path: id 0 upserts the user's single draft row (never
// spawning duplicates), id > 0 updates that row, and promote flips
// a draft into a named fit when the Save button names it.
// Description and tags ride inside the fit document; is_public is
// the community-fit flag (drafts are never public).
func (app *Application) handleFitSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in before saving a fit.", http.StatusForbidden)
		return
	}
	var req struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Tags        string `json:"tags"`
		IsPublic    bool   `json:"isPublic"`
		Promote     bool   `json:"promote"`
		Fit         fitDoc `json:"fit"`
	}
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That fitting couldn't be read.", http.StatusBadRequest)
		return
	}
	sanitizeFitDoc(&req.Fit)
	if req.Fit.ShipTypeID <= 0 {
		http.Error(w, "Pick a ship before saving the fit.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(req.Fit.Name)
	}
	if name == "" {
		name = "Unnamed fit"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	description := strings.TrimSpace(req.Description)
	if len(description) > 500 {
		description = description[:500]
	}
	req.Fit.Name = name
	req.Fit.Description = description
	req.Fit.Tags = parseFitTags(req.Tags)
	raw, err := json.Marshal(req.Fit)
	if err != nil {
		http.Error(w, "That fitting couldn't be saved.", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()

	id := req.ID
	isDraft := false
	isPublic := false
	if id > 0 {
		row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: id, UserID: userID})
		if err != nil {
			http.Error(w, "That saved fit couldn't be found.", http.StatusNotFound)
			return
		}
		isDraft = row.IsDraft
		if isDraft && req.Promote && name != "Unnamed fit" {
			isDraft = false // the Save button names the draft: it becomes a real fit
		}
		if !isDraft {
			isPublic = req.IsPublic
		}
		if err := app.queries.UpdateLocalFitting(ctx, db.UpdateLocalFittingParams{
			Name: name, ShipTypeID: req.Fit.ShipTypeID, ItemsJson: string(raw),
			IsPublic: isPublic, IsDraft: isDraft, UpdatedAt: now, ID: id, UserID: userID,
		}); err != nil {
			logging.Errorf("fittings save: update %d: %v", id, err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
	} else if draft, derr := app.queries.GetUserDraftFitting(ctx, userID); derr == nil {
		// Autosave reuses the one draft row; it never spawns
		// duplicates.
		id = draft.ID
		if err := app.queries.UpdateLocalFitting(ctx, db.UpdateLocalFittingParams{
			Name: name, ShipTypeID: req.Fit.ShipTypeID, ItemsJson: string(raw),
			IsPublic: false, IsDraft: true, UpdatedAt: now, ID: id, UserID: userID,
		}); err != nil {
			logging.Errorf("fittings save: update draft %d: %v", id, err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
		isDraft = true
	} else {
		isDraft = !(req.Promote && name != "Unnamed fit")
		if !isDraft {
			isPublic = req.IsPublic
		}
		row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: userID, Name: name, ShipTypeID: req.Fit.ShipTypeID,
			ItemsJson: string(raw), IsPublic: isPublic, IsDraft: isDraft,
			CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			logging.Errorf("fittings save: create: %v", err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
		id = row.ID
	}
	writeFitJSON(w, map[string]any{"id": id, "savedAt": rfc3339(now), "isDraft": isDraft})
}

// writeFitJSON answers a fitting-editor JSON endpoint.
func writeFitJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// Save to EVE: POST the fit to the character's in-game fittings.
// ---------------------------------------------------------------------------

// esiFittingItem is one line of POST /characters/{id}/fittings/.
type esiFittingItem struct {
	Flag     string `json:"flag"`
	Quantity int64  `json:"quantity"`
	TypeID   int64  `json:"type_id"`
}

// esiFittingBody is POST /characters/{id}/fittings/ (201 answers
// {"fitting_id": ...}).
type esiFittingBody struct {
	Description string           `json:"description"`
	Items       []esiFittingItem `json:"items"`
	Name        string           `json:"name"`
	ShipTypeID  int64            `json:"ship_type_id"`
}

// fitESIFlagFamilies maps editor families to their ESI slot-flag
// prefix and slot count. Drones and cargo ride unindexed flags.
var fitESIFlagFamilies = []struct {
	family string
	prefix string
	slots  int
	label  string
}{
	{fitFamilyHigh, "HiSlot", 8, "high-slot"},
	{fitFamilyMedium, "MedSlot", 8, "mid-slot"},
	{fitFamilyLow, "LoSlot", 8, "low-slot"},
	{fitFamilyRig, "RigSlot", 3, "rig"},
	{fitFamilySubsystem, "SubSystemSlot", 4, "subsystem"},
}

// fitESIFittingBody maps the editor document to the ESI fitting
// body: one flag per fitted module instance (HiSlot0-7 and
// friends), charges riding the same flag as their weapon's slot,
// drones to DroneBay and cargo to Cargo. Items beyond the family's
// slot count are an error — ESI has no flag for them, so the
// handler reports it instead of silently dropping them.
func fitESIFittingBody(name string, doc *fitDoc, familyOf map[int64]string) (esiFittingBody, error) {
	body := esiFittingBody{
		Description: "Created by EveSynapse (https://github.com/natemsz/evesynapse)",
		Name:        name,
		ShipTypeID:  doc.ShipTypeID,
	}
	byFamily := map[string][]int64{}
	for _, it := range doc.Items {
		switch familyOf[it.TypeID] {
		case fitFamilyHigh, fitFamilyMedium, fitFamilyLow, fitFamilyRig, fitFamilySubsystem:
			for i := 0; i < it.Qty; i++ {
				byFamily[familyOf[it.TypeID]] = append(byFamily[familyOf[it.TypeID]], it.TypeID)
			}
		}
	}
	// Weapon slots first: charges need the flag of their weapon.
	weaponFlag := map[int64]string{}
	for _, ff := range fitESIFlagFamilies {
		ids := byFamily[ff.family]
		if len(ids) > ff.slots {
			return body, fmt.Errorf("%d %s modules don't fit in %d %s slots — remove %d first",
				len(ids), ff.label, ff.slots, ff.label, len(ids)-ff.slots)
		}
		for i, id := range ids {
			flag := fmt.Sprintf("%s%d", ff.prefix, i)
			body.Items = append(body.Items, esiFittingItem{Flag: flag, Quantity: 1, TypeID: id})
			if _, ok := weaponFlag[id]; !ok {
				weaponFlag[id] = flag
			}
		}
	}
	for _, it := range doc.Items {
		switch familyOf[it.TypeID] {
		case fitFamilyDrone:
			body.Items = append(body.Items, esiFittingItem{Flag: "DroneBay", Quantity: int64(it.Qty), TypeID: it.TypeID})
		case fitFamilyCargo:
			body.Items = append(body.Items, esiFittingItem{Flag: "Cargo", Quantity: int64(it.Qty), TypeID: it.TypeID})
		}
	}
	for weapon, charge := range doc.Charges {
		flag, ok := weaponFlag[weapon]
		if !ok {
			continue // weapon not fitted: its charge can't ride along
		}
		body.Items = append(body.Items, esiFittingItem{Flag: flag, Quantity: 1, TypeID: charge})
	}
	return body, nil
}

// esiSaveToEVEHint turns an ESI failure into a plain sentence for
// the editor; token values never appear.
func esiSaveToEVEHint(err error) string {
	var se *esi.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusBadRequest:
			return "EVE rejected the fitting as sent — or the character's fitting list is full"
		case http.StatusForbidden:
			return "this character hasn't granted fitting write access"
		case http.StatusUnprocessableEntity:
			return "EVE couldn't place one of the modules"
		}
		return fmt.Sprintf("EVE answered status %d", se.Code)
	}
	if errors.Is(err, esi.ErrErrorLimit) {
		return "EVE is rate-limiting requests right now"
	}
	return "the request to EVE failed"
}

// fitSaveToEVERequest is POST /fittings/save-to-eve/: the target
// character plus the fit document.
type fitSaveToEVERequest struct {
	fitDoc
	CharacterID int64 `json:"characterId"`
}

// handleFitSaveToEVE serves POST /fittings/save-to-eve/: map the
// editor document to ESI slot flags and create the fitting in-game
// for one of the signed-in user's characters. A 403 (character
// linked before the write scope existed) is reported as "sign in
// again", never silently swallowed.
func (app *Application) handleFitSaveToEVE(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	var req fitSaveToEVERequest
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeFitJSON(w, map[string]any{"ok": false, "error": "That fitting couldn't be read."})
		return
	}
	sanitizeFitDoc(&req.fitDoc)
	fail := func(msg string, relink bool) {
		writeFitJSON(w, map[string]any{"ok": false, "error": msg, "relink": relink})
	}

	charName, owned := app.fitPilotLabel(ctx, userID, req.CharacterID)
	if !owned {
		fail("That character isn't one of yours.", false)
		return
	}
	if req.ShipTypeID <= 0 {
		fail("Pick a ship before saving to EVE.", false)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Unnamed fit"
	}

	snap, err := fit.LoadSnapshot(ctx, app.queries, fitDocTypeIDs(&req.fitDoc))
	if err != nil {
		logging.Errorf("fittings save-to-eve: snapshot: %v", err)
		fail("Could not read the module data; try again.", false)
		return
	}
	familyOf := map[int64]string{}
	for _, it := range req.Items {
		familyOf[it.TypeID] = fitSlotFamilyOf(snap, it.TypeID)
	}
	esiBody, err := fitESIFittingBody(name, &req.fitDoc, familyOf)
	if err != nil {
		fail(err.Error(), false)
		return
	}

	ch, err := app.queries.GetCharacter(ctx, req.CharacterID)
	if err != nil {
		fail("That character isn't one of yours.", false)
		return
	}
	token, err := app.validAccessToken(ctx, ch)
	if err != nil {
		logging.Errorf("fittings save-to-eve: token for character %d: %v", req.CharacterID, err)
		fail("Could not reach EVE for "+charName+". Sign in again if it keeps failing.", true)
		return
	}
	var created struct {
		FittingID int64 `json:"fitting_id"`
	}
	path := fmt.Sprintf("/characters/%d/fittings/", req.CharacterID)
	if err := app.esi.PostJSONAuthed(ctx, token, path, esiBody, &created); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			fail(charName+" was linked before EveSynapse asked for fitting write access — sign in again to grant it, then save once more.", true)
			return
		}
		logging.Errorf("fittings save-to-eve: ESI POST %s: %v", path, err)
		fail("EVE refused the fitting ("+esiSaveToEVEHint(err)+").", false)
		return
	}

	// Refresh the cached fittings so the new fit shows up in the
	// list without waiting for the next worker cycle.
	if ferr := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapFittings); ferr != nil {
		logging.Errorf("fittings save-to-eve: refetch fittings for character %d: %v", req.CharacterID, ferr)
	}
	writeFitJSON(w, map[string]any{
		"ok": true, "fittingId": created.FittingID, "name": name, "characterId": req.CharacterID,
	})
}

// handleFitDelete serves POST /fittings/delete/ (form field id).
func (app *Application) handleFitDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err == nil {
		if id, perr := strconv.ParseInt(r.FormValue("id"), 10, 64); perr == nil && id > 0 {
			if err := app.queries.DeleteLocalFitting(ctx, db.DeleteLocalFittingParams{ID: id, UserID: userID}); err != nil {
				logging.Errorf("fittings delete: %d: %v", id, err)
			}
		}
	}
	http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
}

// fitMineRow is one entry in the your-fits search results.
type fitMineRow struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ShipName string `json:"shipName"`
	Mine     bool   `json:"mine"`
	IsPublic bool   `json:"isPublic"`
	IsDraft  bool   `json:"isDraft"`
	IsESI    bool   `json:"isESI,omitempty"`
	Author   string `json:"author,omitempty"`
}

// handleFitMineJSON serves GET /fittings/mine.json: the user's
// saved fits matching q, plus other users' public fits when
// community=1 (bounded, capped at 20). Drives the your-fits
// search bar on the fitting screen.
func (app *Application) handleFitMineJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in first.", http.StatusForbidden)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeFitJSON(w, []fitMineRow{})
		return
	}
	rows, err := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: q})
	if err != nil {
		logging.Errorf("fittings mine search: %v", err)
		http.Error(w, "That search couldn't run.", http.StatusInternalServerError)
		return
	}
	out := make([]fitMineRow, 0, len(rows)+20)
	for _, row := range rows {
		out = append(out, fitMineRow{
			ID: row.ID, Name: row.Name, ShipName: row.ShipName,
			Mine: true, IsPublic: row.IsPublic, IsDraft: row.IsDraft,
		})
	}
	// ESI fittings for the active character
	// These are the in-game fittings from the character's ESI snapshot.
	if charID := sessionCharID(app.sessions, ctx); charID != 0 {
		if char, err := app.queries.GetCharacter(ctx, charID); err == nil {
			var fittings esi.Fittings
			if err := app.esi.GetCached(ctx, char, esi.SnapFittings, &fittings); err == nil {
				qLower := strings.ToLower(q)
				for _, f := range fittings {
					if strings.Contains(strings.ToLower(f.Name), qLower) {
						shipName := app.typeNameOrID(ctx, f.ShipTypeID)
						out = append(out, fitMineRow{
							ID:   -f.FittingID, // negative ID marks ESI fit
							Name: f.Name, ShipName: shipName,
							Mine: true, IsESI: true,
						})
					}
				}
			}
		}
	}
	if r.URL.Query().Get("community") == "1" {
		pub, err := app.queries.SearchPublicFittings(ctx, db.SearchPublicFittingsParams{UserID: userID, Q: q})
		if err != nil {
			logging.Errorf("fittings community search: %v", err)
		} else {
			for _, row := range pub {
				author := ""
				if s, ok := row.AuthorName.(string); ok {
					author = s
				}
				out = append(out, fitMineRow{
					ID: row.ID, Name: row.Name, ShipName: row.ShipName,
					Mine: false, IsPublic: true, Author: author,
				})
			}
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	writeFitJSON(w, out)
}

// handleFitFork serves POST /fittings/fork/: copy another user's
// public fit into the caller's own fittings as a draft. The
// origin is noted in the description; the source fit is never
// touched.
func (app *Application) handleFitFork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Error(w, "Sign in first.", http.StatusForbidden)
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil || req.ID <= 0 {
		http.Error(w, "That fit couldn't be read.", http.StatusBadRequest)
		return
	}
	src, err := app.queries.GetPublicFitting(ctx, req.ID)
	if err != nil {
		http.Error(w, "That fit couldn't be found.", http.StatusNotFound)
		return
	}
	if src.UserID == userID {
		writeFitJSON(w, map[string]any{"id": src.ID})
		return
	}
	var doc fitDoc
	if err := json.Unmarshal([]byte(src.ItemsJson), &doc); err != nil {
		http.Error(w, "That fit couldn't be read.", http.StatusBadRequest)
		return
	}
	sanitizeFitDoc(&doc)
	author := ""
	if s, ok := src.AuthorName.(string); ok {
		author = s
	}
	origin := fmt.Sprintf("Based on %q by %s.", src.Name, author)
	if doc.Description != "" {
		doc.Description += "\n\n" + origin
	} else {
		doc.Description = origin
	}
	// Forks are always private drafts; the user publishes
	// deliberately via the Make public toggle.
	doc.Tags = nil
	doc.Name = src.Name
	raw, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "That fit couldn't be copied.", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: userID, Name: doc.Name, ShipTypeID: doc.ShipTypeID,
		ItemsJson: string(raw), IsPublic: false, IsDraft: true,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		logging.Errorf("fittings fork %d: %v", req.ID, err)
		http.Error(w, "That fit couldn't be copied.", http.StatusInternalServerError)
		return
	}
	writeFitJSON(w, map[string]any{"id": row.ID})
}

// sessionFitStash carries an imported fit (plus its notes)
// across the import redirect into the page render.
const sessionFitStash = "fit_stash"

// handleFitImport serves POST /fittings/import/ (form field
// eft): parse the pasted EFT text, stash the fit, and land back
// on the editor.
func (app *Application) handleFitImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
		return
	}
	doc, notes := app.parseEFT(ctx, r.FormValue("eft"))
	stash, _ := json.Marshal(struct {
		Doc   *fitDoc  `json:"doc"`
		Notes []string `json:"notes"`
	}{Doc: doc, Notes: notes})
	app.sessions.Put(ctx, sessionFitStash, string(stash))
	http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
}

// handleFitExport serves POST /fittings/export/: the posted fit
// as EFT text.
func (app *Application) handleFitExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := readFitRequest(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(app.formatEFT(ctx, &req.fitDoc)))
}

// ---------------------------------------------------------------------------
// Editor assembly for the Fittings page.
// ---------------------------------------------------------------------------

// attachFitEditor fills pageData.FitEditor: the editor chrome,
// the workbench's first paint, and the user's saved-fit list.
// esiFits is the active character's saved EVE fittings (possibly
// nil), used by the per-fit "View stats" deep link.
func (app *Application) attachFitEditor(ctx context.Context, r *http.Request, data *pageData, characters []db.Character, active db.Character, esiFits esi.Fittings) {
	userID := app.userID(ctx)
	editor := &fitEditorView{}
	doc := &fitDoc{Charges: map[int64]int64{}}

	q := r.URL.Query()
	localID, _ := strconv.ParseInt(q.Get("local"), 10, 64)
	esiID, _ := strconv.ParseInt(q.Get("esi"), 10, 64)
	switch {
	case localID > 0 && userID > 0:
		if row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: localID, UserID: userID}); err == nil {
			var stored fitDoc
			if json.Unmarshal([]byte(row.ItemsJson), &stored) == nil {
				doc = &stored
				editor.LocalID = row.ID
				editor.IsPublic = row.IsPublic
			}
		}
	case esiID > 0:
		for _, f := range esiFits {
			if f.FittingID == esiID {
				doc = app.fitDocFromESI(ctx, f)
				break
			}
		}
	default:
		if raw := app.sessions.GetString(ctx, sessionFitStash); raw != "" {
			app.sessions.Remove(ctx, sessionFitStash)
			var stash struct {
				Doc   *fitDoc  `json:"doc"`
				Notes []string `json:"notes"`
			}
			if json.Unmarshal([]byte(raw), &stash) == nil && stash.Doc != nil {
				doc = stash.Doc
				editor.Notes = stash.Notes
			}
		}
	}
	sanitizeFitDoc(doc)

	pilotID := int64(0)
	pilotLabel := "All V"
	if active.CharacterID != 0 {
		pilotID = active.CharacterID
		pilotLabel = active.Name
	}
	if want, _ := strconv.ParseInt(q.Get("pilot"), 10, 64); want != 0 {
		for _, ch := range characters {
			if ch.CharacterID == want {
				pilotID, pilotLabel = ch.CharacterID, ch.Name
			}
		}
	}
	editor.PilotID = pilotID
	editor.EVECharID = active.CharacterID
	editor.EVECharName = active.Name
	for _, ch := range characters {
		editor.Pilots = append(editor.Pilots, assetCharLink{
			ID: ch.CharacterID, Name: ch.Name, Active: ch.CharacterID == pilotID,
		})
	}
	cloneID, _ := strconv.ParseInt(q.Get("clone"), 10, 64)
	editor.Sim = app.buildFitSimView(ctx, doc, pilotID, pilotLabel, cloneID)
	editor.CloneID = editor.Sim.CloneID
	editor.Clones = editor.Sim.Clones

	editor.Description = doc.Description
	editor.TagsList = doc.Tags
	editor.Tags = strings.Join(doc.Tags, ", ")
	// The editor does not embed the your-fits list (the search bar
	// above loads fits through /fittings/mine.json), so the whole fits
	// table is not pulled for this page. LocalFits stays on the view
	// for API compatibility; listLocalFitEntries remains for the
	// dedicated list consumers.
	data.FitEditor = editor
}

// listLocalFitEntries builds the your-fits list: the user's saved
// fits, newest first, with ship names and the description/tags
// stored inside each fit document.
//
//lint:ignore U1000 nothing calls it since the editor stopped embedding the list; kept for the dedicated list consumers (see attachFitEditor's note)
func (app *Application) listLocalFitEntries(ctx context.Context, userID int64) []localFitEntry {
	rows, err := app.queries.ListLocalFittings(ctx, userID)
	if err != nil {
		logging.Errorf("fittings: list local fits: %v", err)
		return nil
	}
	shipIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		shipIDs = append(shipIDs, row.ShipTypeID)
	}
	names := app.esi.CachedTypeNames(ctx, shipIDs)
	entries := make([]localFitEntry, 0, len(rows))
	for _, row := range rows {
		var stored fitDoc
		_ = json.Unmarshal([]byte(row.ItemsJson), &stored)
		shipName := fmt.Sprintf("Type #%d", row.ShipTypeID)
		if n, ok := names[row.ShipTypeID]; ok {
			shipName = n
		}
		entries = append(entries, localFitEntry{
			ID:          row.ID,
			Name:        row.Name,
			ShipName:    shipName,
			Description: stored.Description,
			Tags:        stored.Tags,
			Updated:     rfc3339(row.UpdatedAt),
			IsPublic:    row.IsPublic,
			IsDraft:     row.IsDraft,
		})
	}
	return entries
}

// fitDocFromESI turns one saved EVE fitting into an editor
// document. Cargo ammunition that fits exactly one fitted
// weapon group is loaded into that group; the rest stays cargo.
func (app *Application) fitDocFromESI(ctx context.Context, f esi.Fitting) *fitDoc {
	doc := &fitDoc{Name: f.Name, ShipTypeID: f.ShipTypeID, Charges: map[int64]int64{}}
	var cargoIDs []int64
	for _, it := range f.Items {
		qty := int(it.Quantity)
		if qty < 1 {
			qty = 1
		}
		doc.Items = append(doc.Items, fitDocItem{TypeID: it.TypeID, Qty: qty})
		if strings.HasPrefix(it.Flag, "Cargo") {
			cargoIDs = append(cargoIDs, it.TypeID)
		}
	}
	if len(cargoIDs) == 0 || len(doc.Items) == 0 {
		return doc
	}

	snap, err := fit.LoadSnapshot(ctx, app.queries, fitDocTypeIDs(doc))
	if err != nil {
		return doc
	}
	var weapons []int64
	seenWeapon := map[int64]bool{}
	for _, it := range doc.Items {
		if seenWeapon[it.TypeID] {
			continue
		}
		if snap.HasEffect(it.TypeID, fit.EffectTurretFitted) || snap.HasEffect(it.TypeID, fit.EffectLauncherFitted) {
			seenWeapon[it.TypeID] = true
			weapons = append(weapons, it.TypeID)
		}
	}
	for _, cargoID := range cargoIDs {
		var match int64
		matches := 0
		for _, weapon := range weapons {
			for _, row := range app.fitChargeCandidates(ctx, weapon) {
				if row.TypeID == cargoID {
					match = weapon
					matches++
					break
				}
			}
		}
		if matches == 1 {
			doc.Charges[match] = cargoID
		}
	}
	return doc
}

// ---------------------------------------------------------------------------
// EFT import / export.
// ---------------------------------------------------------------------------

var (
	eftHeaderRe = regexp.MustCompile(`^\[(.+),\s*(.+)\]$`)
	eftQtyRe    = regexp.MustCompile(`^(.*?)\s+x(\d+)$`)
)

// parseEFT turns pasted EFT text into a fit document. Item names
// resolve against the local type table; anything unresolved is
// reported as a note, never silently dropped. [empty] slot
// lines are accepted and ignored.
func (app *Application) parseEFT(ctx context.Context, text string) (*fitDoc, []string) {
	doc := &fitDoc{Charges: map[int64]int64{}}
	var notes []string

	lookup := func(name string) (int64, bool) {
		row, err := app.queries.GetSDETypeByName(ctx, name)
		if err != nil {
			return 0, false
		}
		return row.TypeID, true
	}

	lines := strings.Split(text, "\n")
	idx := 0
	for ; idx < len(lines); idx++ {
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			continue
		}
		m := eftHeaderRe.FindStringSubmatch(line)
		if m == nil {
			notes = append(notes, "That doesn't look like an EFT fit — the first line should read [Ship name, Fit name].")
			return doc, notes
		}
		shipName := strings.TrimSpace(m[1])
		doc.Name = strings.TrimSpace(m[2])
		if id, ok := lookup(shipName); ok {
			doc.ShipTypeID = id
		} else {
			notes = append(notes, fmt.Sprintf("Couldn't find a ship called %q.", shipName))
		}
		idx++
		break
	}

	seenUnknown := map[string]bool{}
	for ; idx < len(lines); idx++ {
		line := strings.TrimSpace(lines[idx])
		if line == "" || line == "[empty]" {
			continue
		}
		qty := 1
		body := line
		if m := eftQtyRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
				qty = n
				body = strings.TrimSpace(m[1])
			}
		}
		if id, ok := lookup(body); ok {
			doc.Items = append(doc.Items, fitDocItem{TypeID: id, Qty: qty})
			continue
		}
		// pyfa-style "Module, Charge" line.
		if cut := strings.LastIndex(body, ", "); cut > 0 {
			if modID, ok := lookup(strings.TrimSpace(body[:cut])); ok {
				if chargeID, ok := lookup(strings.TrimSpace(body[cut+2:])); ok {
					doc.Items = append(doc.Items, fitDocItem{TypeID: modID, Qty: qty})
					doc.Charges[modID] = chargeID
					continue
				}
			}
		}
		if !seenUnknown[line] {
			seenUnknown[line] = true
			notes = append(notes, "Couldn't place: "+line)
		}
	}
	sanitizeFitDoc(doc)
	return doc, notes
}

// formatEFT renders a fit document as EFT text: the [Ship,
// Name] header, low/mid/high/rig/subsystem groups in EFT order,
// then drones and cargo with quantities. Loaded ammunition rides
// on its weapon's line ("Module, Charge"), the shape fitting
// tools read back.
func (app *Application) formatEFT(ctx context.Context, doc *fitDoc) string {
	docCopy := *doc
	sanitizeFitDoc(&docCopy)
	doc = &docCopy
	if doc.ShipTypeID <= 0 {
		return ""
	}
	names := app.esi.CachedTypeNames(ctx, fitDocTypeIDs(doc))
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok && n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", id)
	}
	fitName := doc.Name
	if fitName == "" {
		fitName = "Unnamed fit"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s, %s]\n", nameOf(doc.ShipTypeID), fitName)

	// Classification needs the dogma snapshot; without it, keep
	// the document order in one block rather than failing.
	familyOf := map[int64]string{}
	if snap, err := fit.LoadSnapshot(ctx, app.queries, fitDocTypeIDs(doc)); err == nil {
		for _, it := range doc.Items {
			familyOf[it.TypeID] = fitSlotFamilyOf(snap, it.TypeID)
		}
	}
	weaponCharge := func(typeID int64) string {
		if charge, ok := doc.Charges[typeID]; ok && charge > 0 {
			return ", " + nameOf(charge)
		}
		return ""
	}

	var sections []string
	for _, family := range []string{fitFamilyLow, fitFamilyMedium, fitFamilyHigh, fitFamilyRig, fitFamilySubsystem} {
		var lines []string
		for _, it := range doc.Items {
			if familyOf[it.TypeID] != family {
				continue
			}
			for i := 0; i < it.Qty; i++ {
				lines = append(lines, nameOf(it.TypeID)+weaponCharge(it.TypeID))
			}
		}
		if len(lines) > 0 {
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}
	for _, family := range []string{fitFamilyDrone, fitFamilyCargo} {
		var lines []string
		for _, it := range doc.Items {
			if familyOf[it.TypeID] != family {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s x%d", nameOf(it.TypeID), it.Qty))
		}
		if len(lines) > 0 {
			sections = append(sections, strings.Join(lines, "\n"))
		}
	}
	// Anything unclassified (no snapshot) keeps its place.
	var rest []string
	known := map[string]bool{fitFamilyLow: true, fitFamilyMedium: true, fitFamilyHigh: true,
		fitFamilyRig: true, fitFamilySubsystem: true, fitFamilyDrone: true, fitFamilyCargo: true}
	for _, it := range doc.Items {
		if known[familyOf[it.TypeID]] {
			continue
		}
		rest = append(rest, fmt.Sprintf("%s x%d", nameOf(it.TypeID), it.Qty))
	}
	if len(rest) > 0 {
		sections = append(sections, strings.Join(rest, "\n"))
	}
	if len(sections) > 0 {
		b.WriteString(strings.Join(sections, "\n\n"))
		b.WriteString("\n")
	}
	return b.String()
}
