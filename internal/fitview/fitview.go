package fitview

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
	FitFamilyHigh      = "high"
	FitFamilyMedium    = "medium"
	FitFamilyLow       = "low"
	FitFamilyRig       = "rig"
	FitFamilySubsystem = "subsystem"
	FitFamilyDrone     = "drone"
	FitFamilyCargo     = "cargo"
)

// FitSlotEffectByFamily maps picker families to their slot effect.
var FitSlotEffectByFamily = map[string]int64{
	FitFamilyHigh:      fit.EffectHiPower,
	FitFamilyMedium:    fit.EffectMedPower,
	FitFamilyLow:       fit.EffectLoPower,
	FitFamilyRig:       fit.EffectRigSlot,
	FitFamilySubsystem: fit.EffectSubsystemSlot,
}

// Dogma attributes the charge picker reads off a weapon: the five
// charge-group slots and the charge size (dgmAttributeTypes,
// verified against the live dump).
var FitChargeGroupAttrs = []int64{604, 605, 606, 609, 610}

const FitAttrChargeSize = 128

// FitDocItem is one line of a fit document. States carries one
// module state per instance ("", online/active/offline/
// overheated; "" resolves to the type default); len(States) should
// match Qty and is normalized by SanitizeFitDoc.
type FitDocItem struct {
	TypeID int64    `json:"typeId"`
	Qty    int      `json:"qty"`
	States []string `json:"states,omitempty"`
}

// FitDoc is one fitting: the editor's working state and the
// stored shape of a local fitting (local_fittings.items_json).
type FitDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	ShipTypeID  int64           `json:"shipTypeId"`
	Items       []FitDocItem    `json:"items"`
	Charges     map[int64]int64 `json:"charges"` // weapon type -> loaded charge type
	// Implants overrides the pilot's clone slot by slot: slot (1-10)
	// -> implant type, 0 = slot left empty. Slots not listed keep
	// whatever the selected clone has plugged in.
	Implants map[int]int64 `json:"implants,omitempty"`
}

// FitImplantSlots is how many implant slots a clone has.
const FitImplantSlots = 10

// FitAttrImplantness is the dogma attribute naming an implant's slot.
const FitAttrImplantness = 331

// Bounds that keep a hostile or corrupt fit document from
// turning into unbounded engine or rendering work: at most 8
// fitted instances of one type (the widest rack in the game),
// at most 32 fitted instances across a whole fit (8/8/8 racks
// plus 3 rigs plus 5 subsystems is the largest real total),
// and at most 64 per-instance state entries on one line (no
// real line carries more; consumers read missing entries as
// the default state).
const (
	maxFittedPerLine   = 8
	maxFittedPerShip   = 32
	maxFitStateEntries = 64
)

// SanitizeFitDoc normalizes a fit document arriving from the
// browser or storage: sane quantities, duplicate lines folded,
// charges for weapons no longer fitted dropped. When the
// caller passes the engine snapshot, fitted-slot lines are
// also capped at what a hull can actually carry (see the
// bounds above); drones and cargo keep bulk quantities — a
// bay or a hold legitimately stacks high.
func SanitizeFitDoc(doc *FitDoc, snap ...*fit.Snapshot) {
	if doc == nil {
		return
	}
	merged := make([]FitDocItem, 0, len(doc.Items))
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
	if len(snap) > 0 && snap[0] != nil {
		fittedTotal := 0
		kept := merged[:0]
		for _, it := range merged {
			switch FitSlotFamilyOf(snap[0], it.TypeID) {
			case FitFamilyHigh, FitFamilyMedium, FitFamilyLow, FitFamilyRig, FitFamilySubsystem:
				if it.Qty > maxFittedPerLine {
					it.Qty = maxFittedPerLine
				}
				if fittedTotal+it.Qty > maxFittedPerShip {
					it.Qty = maxFittedPerShip - fittedTotal
				}
				if it.Qty <= 0 {
					continue // past the ship total: dropped
				}
				fittedTotal += it.Qty
			}
			kept = append(kept, it)
		}
		merged = kept
		doc.Items = merged
	}
	for slot, typeID := range doc.Implants {
		if slot < 1 || slot > FitImplantSlots || typeID < 0 {
			delete(doc.Implants, slot)
		}
	}
	if len(doc.Implants) == 0 {
		doc.Implants = nil
	}
	// Normalize per-instance module states: one entry per
	// instance, unknown values demoted to "" (the default), and
	// all-default arrays dropped so stored docs stay lean.
	for i := range merged {
		it := &merged[i]
		if len(it.States) > it.Qty {
			it.States = it.States[:it.Qty]
		}
		for len(it.States) < it.Qty && len(it.States) < maxFitStateEntries {
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

// FitSlotFamilyOf classifies one type into an editor family using
// the engine snapshot (slot effects first, in the engine's own
// order, then the drone bandwidth draw).
func FitSlotFamilyOf(snap *fit.Snapshot, typeID int64) string {
	switch {
	case snap.HasEffect(typeID, fit.EffectRigSlot):
		return FitFamilyRig
	case snap.HasEffect(typeID, fit.EffectSubsystemSlot):
		return FitFamilySubsystem
	case snap.Attrs[typeID][fit.AttrDroneBandwidthUsed] > 0:
		return FitFamilyDrone
	case snap.HasEffect(typeID, fit.EffectHiPower):
		return FitFamilyHigh
	case snap.HasEffect(typeID, fit.EffectMedPower):
		return FitFamilyMedium
	case snap.HasEffect(typeID, fit.EffectLoPower):
		return FitFamilyLow
	}
	return FitFamilyCargo
}

// FitFamilyFitted reports whether the family takes part in the
// simulation (cargo is listed but never fitted).
func FitFamilyFitted(family string) bool {
	return family != FitFamilyCargo
}

// FitSlotChip is one fitted item line in the slot grid. State
// is the uniform resolved state when every instance shares one
// ("" when mixed or stateless) — for the state visuals.
// StatesCSV is the comma-joined list of states the line's
// modules may take (slot families only; "" for stateless kinds),
// for the phone racks' tap-to-cycle.
type FitSlotChip struct {
	TypeID    int64
	Name      string
	Qty       int
	State     string
	Sub       string // the first stats from the tooltip, one line under the name
	TipLines  string // serialized FitTipLine rows for the tooltip
	StatesCSV string
}

// FitSlotGroup is one row of the slot grid.
type FitSlotGroup struct {
	Key    string
	Label  string
	Used   int
	Max    int
	HasMax bool
	Over   bool
	Chips  []FitSlotChip
	Empty  []int // empty slot buttons to render (slot ordinals)
	// ReadOnly mirrors the workbench's, so the shared group template
	// (fit_workbench.html "fit-group") can drop editing controls.
	ReadOnly bool
}

// FitBayGroup reports whether a group holds a bay's contents (drones,
// cargo) rather than fitted slots: the workbench shows bays under the
// ring on a wide screen, slots beside it.
func FitBayGroup(key string) bool { return key == "drone" || key == "cargo" }

// ImplantsUsed counts the filled implant slots.
func (v *FitSimView) ImplantsUsed() int {
	n := 0
	for _, s := range v.Implants {
		if s.TypeID > 0 {
			n++
		}
	}
	return n
}

// SlotGroups are the fitted-slot groups, in order.
func (v *FitSimView) SlotGroups() []FitSlotGroup { return v.GroupsWhere(false) }

// BayGroups are the drone and cargo groups, in order.
func (v *FitSimView) BayGroups() []FitSlotGroup { return v.GroupsWhere(true) }

func (v *FitSimView) GroupsWhere(bays bool) []FitSlotGroup {
	var out []FitSlotGroup
	for _, g := range v.Groups {
		if FitBayGroup(g.Key) == bays {
			g.ReadOnly = v.ReadOnly
			out = append(out, g)
		}
	}
	return out
}

// FitVisualSlot is one slot circle on the in-game-style visual fit.
type FitVisualSlot struct {
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
	TipLines   string  // serialized FitTipLine rows for the rich tooltip
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

// FitVisualView is the ship render with module slots arranged
// around it, in-game style.
type FitVisualView struct {
	ShipID     int64
	ShipName   string
	Slots      []FitVisualSlot
	Separators []FitVisualSep   // ember divider ticks between slot groups
	Labels     []FitVisualLabel // in-ring group captions (HIGH / MID / ...)
	Segs       []FitVisualSeg   // bordered arc segments, one per rendered group
}

// FitVisualSeg is one bordered arc segment behind a slot group: an
// annular sector path (SVG d) in the 0-100 viewBox, precomputed here
// so the template stays declarative.
type FitVisualSeg struct {
	D string
}

// FitVisualSep is one ember divider tick between two rendered slot
// groups, sitting on the slot ring at their shared boundary angle.
// Rot orients the tick's long axis radially (CSS rotate, degrees).
type FitVisualSep struct {
	X, Y float64
	Rot  float64
}

// FitVisualLabel is the in-ring caption for one rendered slot
// group, set in the ember gradient like the app's titles.
type FitVisualLabel struct {
	X, Y float64
	Text string
}

// FitVisualArcs places the five slot families on arcs around the
// ship (screen degrees: 0=east, 90=south, so 270 is straight up):
// highs across the top, mids down the right, lows across the
// bottom, rigs and subsystems sharing the left. The third value
// is the ring radius as a percent of the visual box.
var FitVisualArcs = map[string][3]float64{
	FitFamilyHigh:      {225, 315, 44},
	FitFamilyMedium:    {315, 405, 44},
	FitFamilyLow:       {45, 135, 44},
	FitFamilyRig:       {135, 180, 44},
	FitFamilySubsystem: {180, 225, 44},
}

var FitVisualGroupLabels = map[string]string{
	FitFamilyHigh:      "High slot",
	FitFamilyMedium:    "Mid slot",
	FitFamilyLow:       "Low slot",
	FitFamilyRig:       "Rig",
	FitFamilySubsystem: "Subsystem",
}

// FitVisualGroupCaptions are the short in-ring captions, in circular
// order around the ship (the same order as FitVisualArcs).
var FitVisualGroupCaptions = map[string]string{
	FitFamilyHigh:      "HIGH",
	FitFamilyMedium:    "MID",
	FitFamilyLow:       "LOW",
	FitFamilyRig:       "RIG",
	FitFamilySubsystem: "SUBSYSTEM",
}

// FitVisualGroupOrder is the circular order of slot groups around
// the ship; separators sit at each rendered pair's shared boundary.
var FitVisualGroupOrder = []string{
	FitFamilyHigh,
	FitFamilyMedium,
	FitFamilyLow,
	FitFamilyRig,
	FitFamilySubsystem,
}

// FitVisualMetaName maps the SDE meta group (attribute 1692) to
// the player's vocabulary.
func FitVisualMetaName(group int) string {
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

// FitVisualNum formats a module CPU/PG figure the way the stat
// panel does: whole numbers stay whole.
func FitVisualNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// FitArcPt places a point on the visual's 0-100 box: radius r at
// angle deg (0 = east, positive clockwise in screen coords).
func FitArcPt(r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return 50 + r*math.Cos(rad), 50 + r*math.Sin(rad)
}

// FitSegPath is the SVG path for one bordered group segment: an
// annular sector from a0 to a1 degrees between radii rIn and rOut.
// Angles run clockwise (screen coords), so the outer arc sweeps
// with flag 1 and the returning inner arc with flag 0.
func FitSegPath(a0, a1, rIn, rOut float64) string {
	x0o, y0o := FitArcPt(rOut, a0)
	x1o, y1o := FitArcPt(rOut, a1)
	x1i, y1i := FitArcPt(rIn, a1)
	x0i, y0i := FitArcPt(rIn, a0)
	large := 0
	if a1-a0 > 180 {
		large = 1
	}
	return fmt.Sprintf("M%.2f,%.2f A%.2f,%.2f 0 %d 1 %.2f,%.2f L%.2f,%.2f A%.2f,%.2f 0 %d 0 %.2f,%.2f Z",
		x0o, y0o, rOut, rOut, large, x1o, y1o, x1i, y1i, rIn, rIn, large, x0i, y0i)
}

// FitMetaGroupID is the SDE attribute carrying the inv meta group.
const FitMetaGroupID = 1692

// FitVisualMetaOf reads the static SDE meta group for a type —
// meta level is a property of the type itself, never modified by
// skills or bonuses, so it comes from the raw snapshot, not the
// effective attribute map. Returns "" when unknown.
func FitVisualMetaOf(snap *fit.Snapshot, eff map[int64]float64, typeID int64) string {
	if snap != nil {
		if a := snap.Attrs[typeID]; a != nil {
			if g, ok := a[FitMetaGroupID]; ok && g != 0 {
				return FitVisualMetaName(int(g))
			}
		}
	}
	if eff != nil {
		if g := eff[FitMetaGroupID]; g != 0 {
			return FitVisualMetaName(int(g))
		}
	}
	return ""
}

// FitTipLine is one label/value row of a module tooltip.
// Class is "" (effective matches base), "good" (effective is
// an improvement on the module's base value) or "bad" (it is
// worse); the client colors the value by it.
type FitTipLine struct {
	Label string
	Value string
	Class string
}

// FitChipSub is a module row's one-line summary: its first two
// tooltip stats ("Optimal 2.4 km · Falloff 8.4 km").
func FitChipSub(lines []FitTipLine) string {
	var parts []string
	for _, l := range lines {
		if len(parts) == 2 {
			break
		}
		if l.Label != "" && l.Value != "" {
			parts = append(parts, l.Label+" "+l.Value)
		}
	}
	return strings.Join(parts, " · ")
}

// FitTipLinesString serializes tooltip rows for the data
// attribute the client renders: "Label|Value|Class" rows
// joined by ";". Labels and values are composed here from
// formatted numbers and units, so they never contain either
// separator.
func FitTipLinesString(lines []FitTipLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Label + "|" + l.Value + "|" + l.Class
	}
	return strings.Join(parts, ";")
}

// FitTipClass colors an effective value against its base:
// higherBetter says which direction counts as an improvement
// (more range is good; less CPU is good). A relative shift
// under half a percent reads as unchanged.
func FitTipClass(eff, base float64, higherBetter bool) string {
	if base == 0 || eff == 0 {
		return ""
	}
	d := (eff - base) / math.Abs(base)
	if math.Abs(d) < 0.005 {
		return ""
	}
	if (d > 0) == higherBetter {
		return "good"
	}
	return "bad"
}

// FitEffAttrs resolves a fitted type's effective attributes
// for a state: the engine's per-state fold when the module
// is in a non-default state, else its resting fold, else the
// raw SDE row when the engine has nothing (snap may be nil).
func FitEffAttrs(res *fit.Result, snap *fit.Snapshot, typeID int64, state string) map[int64]float64 {
	eff := res.ItemAttrs[typeID]
	if state != "" {
		if sa := res.StateAttrs[fit.StateAttrKey(typeID, state)]; sa != nil {
			eff = sa
		}
	}
	if eff == nil && snap != nil {
		eff = snap.Attrs[typeID]
	}
	return eff
}

// FitTipCycle is a module's cycle time in seconds from an
// attribute set (duration, falling back to speed — the same
// rule the engine's cycleSeconds applies).
func FitTipCycle(attrs map[int64]float64) float64 {
	if attrs == nil {
		return 0
	}
	if d := attrs[fit.AttrDuration]; d > 0 {
		return d / 1000
	}
	if s := attrs[fit.AttrSpeed]; s > 0 {
		return s / 1000
	}
	return 0
}

// FitTipDamage sums a charge's four damage attributes in an
// attribute set (the engine's damageAttrs, base or folded).
func FitTipDamage(attrs map[int64]float64) float64 {
	if attrs == nil {
		return 0
	}
	return attrs[fit.AttrDamageEM] + attrs[fit.AttrDamageExplosive] +
		attrs[fit.AttrDamageKinetic] + attrs[fit.AttrDamageThermal]
}

// FitTipLines composes a fitted module's tooltip rows from
// its effective attributes against its base (raw SDE) row:
// what the module does with this pilot's skills and hull
// bonuses, colored where that differs from the unmodified
// module. Weapon damage comes from the engine's per-weapon
// record, so the figures are the stats panel's own. Rows
// are ordered the way a pilot reads them — output, reach,
// cost — and capped so the tooltip stays a quick look.
func FitTipLines(res *fit.Result, snap *fit.Snapshot, doc *FitDoc, typeID int64, state string, eff map[int64]float64) []FitTipLine {
	if eff == nil {
		return nil
	}
	var base map[int64]float64
	if snap != nil {
		base = snap.Attrs[typeID]
	}
	var lines []FitTipLine
	add := func(label, value, class string) {
		if len(lines) < FitTipMaxLines {
			lines = append(lines, FitTipLine{label, value, class})
		}
	}
	// A subsystem's mark on the hull: the slots and hardpoints it
	// adds, which is what choosing one changes.
	for _, sm := range FitTipSlotMods {
		if n := eff[sm.attr]; n != 0 {
			add(sm.label, fmt.Sprintf("%+d", int(math.Round(n))), "")
		}
	}
	// Resist bonuses (hardeners, invulnerability fields, energized
	// membranes, resist rigs): one row per damage type, tinted in
	// that type's color, the figure as a resistance gain. They are
	// state-folded like everything here, so an overheated hardener
	// shows its heated figures.
	for _, rb := range FitTipResistBonuses {
		if v := eff[rb.attr]; v < 0 {
			class := rb.class
			if c := FitTipClass(-v, -base[rb.attr], true); c != "" {
				class += " " + c
			}
			add(rb.label, "+"+FitVisualNum(-v)+"%", class)
		}
	}
	weapon := snap != nil && (snap.HasEffect(typeID, fit.EffectTurretFitted) || snap.HasEffect(typeID, fit.EffectLauncherFitted))
	chargeID := doc.Charges[typeID]
	if chargeID > 0 {
		key := fit.StateAttrKey(typeID, state)
		dps, volley := res.WeaponDPS[key], res.WeaponVolley[key]
		if dps == 0 && state != "" {
			// The engine records an unspecified state under
			// the default key; the slot's normalized state
			// may name it ("active").
			dps, volley = res.WeaponDPS[fit.StateAttrKey(typeID, "")], res.WeaponVolley[fit.StateAttrKey(typeID, "")]
		}
		if dps > 0 {
			// The good/bad tint is only exact for turrets:
			// a launcher's folded damage also carries the
			// pilot factor, which a base comparison here
			// cannot separate from the module's own change.
			dpsClass, volleyClass := "", ""
			if snap != nil && snap.HasEffect(typeID, fit.EffectTurretFitted) {
				cb := snap.Attrs[chargeID]
				baseVolley := FitTipDamage(cb) * base[fit.AttrDamageMultiplier]
				if bc := FitTipCycle(base); bc > 0 {
					dpsClass = FitTipClass(dps, baseVolley/bc, true)
				}
				volleyClass = FitTipClass(volley, baseVolley, true)
			}
			add("DPS", FitFmt1(dps), dpsClass)
			add("Volley", FitFmt0(volley), volleyClass)
		}
	}
	if rng := eff[fit.AttrMaxRange]; rng > 0 {
		add("Optimal", FitVisualNum(rng/1000)+" km", FitTipClass(rng, base[fit.AttrMaxRange], true))
	}
	if fall := eff[fit.AttrFalloff]; fall > 0 {
		add("Falloff", FitVisualNum(fall/1000)+" km", FitTipClass(fall, base[fit.AttrFalloff], true))
	}
	if trk := eff[fit.AttrTrackingSpeed]; trk > 0 {
		add("Tracking", FitTipNum3(trk), FitTipClass(trk, base[fit.AttrTrackingSpeed], true))
	}
	if neut := eff[fit.AttrEnergyNeutralizerAmount]; neut > 0 {
		add("Neuts", FitVisualNum(neut)+" GJ", FitTipClass(neut, base[fit.AttrEnergyNeutralizerAmount], true))
	}
	for _, repAttr := range []int64{fit.AttrShieldBonus, fit.AttrArmorDamageAmount} {
		if amt := eff[repAttr]; amt > 0 {
			if cyc := FitTipCycle(eff); cyc > 0 {
				class := ""
				if bc := FitTipCycle(base); bc > 0 {
					class = FitTipClass(amt/cyc, base[repAttr]/bc, true)
				}
				add("Reps", FitVisualNum(amt/cyc)+" HP/s", class)
			}
			break
		}
	}
	if sf := eff[fit.AttrSpeedFactor]; sf != 0 {
		label := "Boost"
		class := FitTipClass(sf, base[fit.AttrSpeedFactor], true)
		if sf < 0 {
			label = "Strength"
			class = FitTipClass(-sf, -base[fit.AttrSpeedFactor], true)
		}
		add(label, FitVisualNum(math.Abs(sf))+"%", class)
	}
	// Passive bonuses: what a damage mod, extender, plate, battery,
	// power relay or cap rig adds to the ship.
	if !weapon {
		if m := eff[fit.AttrDamageMultiplier]; m > 1 {
			add("Damage", "+"+FitVisualNum((m-1)*100)+"%", FitTipClass(m, base[fit.AttrDamageMultiplier], true))
		}
		if m := eff[FitAttrSpeedMultiplier]; m > 0 && m < 1 {
			add("Rate of fire", "+"+FitVisualNum((1-m)*100)+"%", FitTipClass(1-m, 1-base[FitAttrSpeedMultiplier], true))
		}
	}
	if hp := eff[FitAttrCapacityBonus]; hp > 0 {
		add("Shield HP", "+"+FitVisualNum(hp), FitTipClass(hp, base[FitAttrCapacityBonus], true))
	}
	if hp := eff[FitAttrArmorHPBonusAdd]; hp > 0 {
		add("Armor HP", "+"+FitVisualNum(hp), FitTipClass(hp, base[FitAttrArmorHPBonusAdd], true))
	}
	if c := eff[FitAttrCapacitorBonus]; c > 0 {
		add("Capacitor", "+"+FitVisualNum(c)+" GJ", FitTipClass(c, base[FitAttrCapacitorBonus], true))
	}
	if m := eff[FitAttrCapRechargeMult]; m > 0 && m < 1 {
		add("Cap recharge", "−"+FitVisualNum((1-m)*100)+"%", FitTipClass(1-m, 1-base[FitAttrCapRechargeMult], true))
	}
	if m := eff[FitAttrShieldRechargeMult]; m > 0 && m < 1 {
		add("Shield recharge", "−"+FitVisualNum((1-m)*100)+"%", FitTipClass(1-m, 1-base[FitAttrShieldRechargeMult], true))
	}
	if sig := eff[FitAttrSignatureRadiusBonus]; sig > 0 {
		add("Signature", "+"+FitVisualNum(sig)+"%", FitTipClass(sig, base[FitAttrSignatureRadiusBonus], false))
	}
	if capNeed := eff[fit.AttrCapacitorNeed]; capNeed > 0 {
		add("Cap", FitVisualNum(capNeed)+" GJ", FitTipClass(capNeed, base[fit.AttrCapacitorNeed], false))
	}
	if cpu := eff[fit.AttrCPU]; cpu > 0 {
		add("CPU", FitVisualNum(cpu)+" tf", FitTipClass(cpu, base[fit.AttrCPU], false))
	}
	if pg := eff[fit.AttrPower]; pg > 0 {
		add("PG", FitVisualNum(pg)+" MW", FitTipClass(pg, base[fit.AttrPower], false))
	}
	return lines
}

// FitTipMaxLines caps a tooltip: enough for a hardener's four
// resists plus its costs, still a quick look.
const FitTipMaxLines = 12

// Module attributes the tooltip reads that the engine does not name.
const (
	FitAttrCapacitorBonus       = 67   // capacitor battery: capacity added
	FitAttrCapacityBonus        = 72   // shield extender: shield HP added
	FitAttrShieldRechargeMult   = 134  // power diagnostics, rigs
	FitAttrCapRechargeMult      = 144  // cap power relay, cap control circuit
	FitAttrSpeedMultiplier      = 204  // damage mods: rate-of-fire multiplier
	FitAttrSignatureRadiusBonus = 554  // MWD signature bloom
	FitAttrArmorHPBonusAdd      = 1159 // armor plate: armor HP added
)

// FitTipResistBonuses are the four resist-bonus attributes, in EVE's
// own order, with each damage type's color class.
var FitTipResistBonuses = []struct {
	attr  int64
	label string
	class string
}{
	{984, "EM", "res-em"},
	{987, "Thermal", "res-th"},
	{986, "Kinetic", "res-kin"},
	{985, "Explosive", "res-exp"},
}

// FitTipSlotMods are a subsystem's slot and hardpoint modifiers.
var FitTipSlotMods = []struct {
	attr  int64
	label string
}{
	{fit.AttrHiSlotModifier, "High slots"},
	{fit.AttrMedSlotModifier, "Mid slots"},
	{fit.AttrLowSlotModifier, "Low slots"},
	{fit.AttrTurretHardPointModifier, "Turrets"},
	{fit.AttrLauncherHardPointModifier, "Launchers"},
}

// FitTipNum3 formats a small decimal (tracking speeds) with
// up to three decimal places, trailing zeros trimmed.
func FitTipNum3(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// FitBuildVisual shapes the visual fit display from the engine
// result and the document's own lines: one circle per slot,
// filled circles carrying their module, positioned on arcs around
// the ship render. Positions are precomputed here so the template
// stays declarative. snap may be nil (tests); then the tooltip
// figures stay empty.
func FitBuildVisual(res *fit.Result, doc *FitDoc, snap *fit.Snapshot, familyOf map[int64]string, nameOf func(int64) string, supportsSubsystems bool) *FitVisualView {
	v := &FitVisualView{ShipID: doc.ShipTypeID, ShipName: nameOf(doc.ShipTypeID)}
	maxOf := map[string]int{
		FitFamilyHigh:   res.HighSlots,
		FitFamilyMedium: res.MediumSlots,
		FitFamilyLow:    res.LowSlots,
		FitFamilyRig:    res.RigSlots,
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
		if _, ok := FitVisualArcs[fam]; !ok {
			continue
		}
		// The visual places at most a rack's worth of instances
		// per line and a bounded set per family, whatever the
		// document claims: it renders slots, not quantities.
		n := it.Qty
		if n > 8 {
			n = 8
		}
		for i := 0; i < n && len(fitted[fam]) < 16; i++ {
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
	maxOf[FitFamilySubsystem] = 0
	if supportsSubsystems || len(fitted[FitFamilySubsystem]) > 0 {
		maxOf[FitFamilySubsystem] = len(fitted[FitFamilySubsystem])
		if maxOf[FitFamilySubsystem] > 8 {
			maxOf[FitFamilySubsystem] = 8
		}
	}
	rendered := map[string]bool{}
	for _, fam := range []string{FitFamilyHigh, FitFamilyMedium, FitFamilyLow, FitFamilyRig, FitFamilySubsystem} {
		arc := FitVisualArcs[fam]
		max := maxOf[fam]
		if max <= 0 {
			continue
		}
		if max > 16 {
			max = 16
		}
		rendered[fam] = true
		span := arc[1] - arc[0]
		ids := fitted[fam]
		if len(ids) > max {
			ids = ids[:max]
		}
		for i := 0; i < max; i++ {
			ang := (arc[0] + (float64(i)+0.5)*span/float64(max)) * math.Pi / 180
			s := FitVisualSlot{
				GroupKey:   fam,
				GroupLabel: FitVisualGroupLabels[fam],
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
				eff := FitEffAttrs(res, snap, pm.TypeID, pm.State)
				if eff != nil {
					if cpu := eff[fit.AttrCPU]; cpu > 0 {
						s.CPU = FitVisualNum(cpu) + " tf"
					}
					if pg := eff[fit.AttrPower]; pg > 0 {
						s.PG = FitVisualNum(pg) + " MW"
					}
					s.Meta = FitVisualMetaOf(snap, eff, pm.TypeID)
					// Headline effective stat (post-dogma): web
					// strength and similar speedFactor bonuses
					// show the skill-scaled value, not base.
					if sf := eff[fit.AttrSpeedFactor]; sf != 0 {
						if sf < 0 {
							s.Stat = "Strength " + FitVisualNum(-sf) + "%"
						} else {
							s.Stat = "Boost " + FitVisualNum(sf) + "%"
						}
					}
				}
				// Loaded charge rides the slot as a badge; the
				// tooltip names it too.
				if chID := doc.Charges[pm.TypeID]; chID > 0 {
					s.ChargeID = chID
					s.ChargeName = nameOf(chID)
				}
				s.TipLines = FitTipLinesString(FitTipLines(res, snap, doc, pm.TypeID, pm.State, eff))
			}
			v.Slots = append(v.Slots, s)
		}
	}
	// Ember dividers between rendered groups, on the ring at their
	// shared boundary angle, plus an in-ring caption per group so
	// the rings read as distinct in-game-like sections.
	for i, fam := range FitVisualGroupOrder {
		next := FitVisualGroupOrder[(i+1)%len(FitVisualGroupOrder)]
		arc := FitVisualArcs[fam]
		if rendered[fam] {
			mid := (arc[0] + arc[1]) / 2 * math.Pi / 180
			v.Labels = append(v.Labels, FitVisualLabel{
				X:    50 + 30*math.Cos(mid),
				Y:    50 + 30*math.Sin(mid),
				Text: FitVisualGroupCaptions[fam],
			})
		}
		if rendered[fam] && rendered[next] {
			end := arc[1] * math.Pi / 180
			v.Separators = append(v.Separators, FitVisualSep{
				X:   50 + arc[2]*math.Cos(end),
				Y:   50 + arc[2]*math.Sin(end),
				Rot: 90 - arc[1],
			})
		}
		// Bordered arc segment behind the group's slots: the band
		// the slot ring (radius arc[2]) sits inside.
		if rendered[fam] {
			v.Segs = append(v.Segs, FitVisualSeg{
				D: FitSegPath(arc[0], arc[1], arc[2]-4.5, arc[2]+4.5),
			})
		}
	}
	return v
}

// FitChargeOption is one selectable charge for a weapon group.
type FitChargeOption struct {
	ID       int64
	Name     string
	Selected bool
}

// FitChargeSet is the ammunition picker for one weapon type.
type FitChargeSet struct {
	WeaponID   int64
	WeaponName string
	Options    []FitChargeOption
}

// Loaded is the selected charge, if any.
func (c FitChargeSet) Loaded() (o FitChargeOption) {
	for _, opt := range c.Options {
		if opt.Selected {
			return opt
		}
	}
	return o
}

// OptionIDs lists the charges the weapon takes, comma-separated, so
// the module search can load one without another round trip.
func (c FitChargeSet) OptionIDs() string {
	ids := make([]string, 0, len(c.Options))
	for _, opt := range c.Options {
		ids = append(ids, strconv.FormatInt(opt.ID, 10))
	}
	return strings.Join(ids, ",")
}

// FitImplantEffect is one stat the plugged-in implants move: the
// value without them, with them, and the change.
type FitImplantEffect struct {
	Label string
	From  string
	To    string
	Delta string
	Good  bool
}

// FitImplantEffects compares the fit computed without implants
// (base) to the fit with them, stat by stat, keeping the stats
// that changed.
func FitImplantEffects(base, with *fit.Result) []FitImplantEffect {
	km := func(v float64) string {
		if v >= 1000 {
			return FitFmt1(v/1000) + " km"
		}
		return FitFmt0(v) + " m"
	}
	unit := func(f func(float64) string, u string) func(float64) string {
		return func(v float64) string { return f(v) + u }
	}
	stats := []struct {
		label  string
		get    func(*fit.Result) float64
		format func(float64) string
		lower  bool // a smaller number is the better one
	}{
		{"DPS", func(r *fit.Result) float64 { return r.DPS }, FitFmt1, false},
		{"Volley", func(r *fit.Result) float64 { return r.Volley }, FitFmt1, false},
		{"Effective HP", func(r *fit.Result) float64 { return r.EHP.Omni }, FitEHPText, false},
		{"Shield HP", func(r *fit.Result) float64 { return r.ShieldHP }, FitFmt0, false},
		{"Armor HP", func(r *fit.Result) float64 { return r.ArmorHP }, FitFmt0, false},
		{"Hull HP", func(r *fit.Result) float64 { return r.HullHP }, FitFmt0, false},
		{"Shield recharge", func(r *fit.Result) float64 { return r.ShieldRegenPeak }, unit(FitFmt1, " HP/s"), false},
		{"Shield boosting", func(r *fit.Result) float64 { return r.ShieldBoostRate }, unit(FitFmt1, " HP/s"), false},
		{"Armor repair", func(r *fit.Result) float64 { return r.ArmorRepairRate }, unit(FitFmt1, " HP/s"), false},
		{"Capacitor", func(r *fit.Result) float64 { return r.CapacitorCapacity }, unit(FitFmt1, " GJ"), false},
		{"Cap recharge", func(r *fit.Result) float64 { return r.CapacitorPeakRecharge }, unit(FitFmt1, " GJ/s"), false},
		{"Cap use", func(r *fit.Result) float64 { return r.CapacitorDraw }, unit(FitFmt1, " GJ/s"), true},
		{"Powergrid", func(r *fit.Result) float64 { return r.PowergridMax }, unit(FitFmt1, " MW"), false},
		{"CPU", func(r *fit.Result) float64 { return r.CPUMax }, unit(FitFmt1, " tf"), false},
		{"Speed", func(r *fit.Result) float64 { return r.Velocity }, unit(FitFmt0, " m/s"), false},
		{"Align time", func(r *fit.Result) float64 { return r.AlignSeconds }, unit(FitFmt1, " s"), true},
		{"Signature radius", func(r *fit.Result) float64 { return r.SignatureRadius }, unit(FitFmt0, " m"), true},
		{"Targeting range", func(r *fit.Result) float64 { return r.MaxTargetRange }, km, false},
		{"Scan resolution", func(r *fit.Result) float64 { return r.ScanResolution }, unit(FitFmt0, " mm"), false},
		{"Sensor strength", func(r *fit.Result) float64 { return r.SensorStrength }, FitFmt1, false},
		{"Drone bandwidth", func(r *fit.Result) float64 { return r.DroneBandwidth }, unit(FitFmt1, " Mbit/s"), false},
		{"Cargo hold", func(r *fit.Result) float64 { return r.CargoCapacity }, unit(FitFmt1, " m³"), false},
	}
	var out []FitImplantEffect
	for _, st := range stats {
		from, to := st.get(base), st.get(with)
		if from == to || from == 0 {
			continue
		}
		pct := (to - from) / from * 100
		if pct > -0.05 && pct < 0.05 {
			continue
		}
		out = append(out, FitImplantEffect{
			Label: st.label,
			From:  st.format(from),
			To:    st.format(to),
			Delta: fmt.Sprintf("%+.1f%%", pct),
			Good:  (to > from) != st.lower,
		})
	}
	return out
}

// FitBarRow is one resource bar (powergrid, CPU).
type FitBarRow struct {
	Label string
	Used  string
	Max   string
	Left  string
	Pct   float64 // bar fill, 0..100
	Over  bool
}

// FitResistRow is one tank layer's line.
type FitResistRow struct {
	Layer     string
	HP        string
	EM        string
	Thermal   string
	Kinetic   string
	Explosive string
	EHP       string
	Resists   []FitResistCell // the four resists in EM, thermal, kinetic, explosive order
}

// FitResistCell is one resist chip: its damage type (the chip's
// colour), its text and its fill (0..100).
type FitResistCell struct {
	Kind string // "em", "th", "kin", "exp"
	Text string
	Pct  float64
}

// FitTile is one hardpoint tile under the slot ring.
type FitTile struct {
	Label string
	Value string
	Over  bool
}

// FitStatsView is the pyfa-vocabulary stat panel, preformatted.
type FitStatsView struct {
	Powergrid     FitBarRow
	CPU           FitBarRow
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
	SmartbombDPS  string // "" when no smartbomb is fitted
	TotalDPS      string
	TurretVolley  string
	MissileVolley string
	DroneVolley   string
	TotalVolley   string
	NeutDrain     string
	NosDrain      string
	Tank          []FitResistRow
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
	Calib         FitBarRow // calibration as a bar beside powergrid and CPU
	Tiles         []FitTile // turrets, launchers, calibration under the ring
	Problems      []string  // why the fit doesn't fit, one short line each
}

// FitMissingSkill is one unmet requirement for the chosen pilot.
type FitMissingSkill struct {
	Name string
	Have int
	Need int
}

// FitSimView is the workbench fragment (and its server-rendered
// first paint inside the Fittings page).
// FitCloneOption is one entry in the editor's Clone dropdown:
// the active clone (ID 0) or one of the pilot's jump clones.
type FitCloneOption struct {
	ID       int64
	Name     string // "Active clone" or the clone's name + location
	Implants int    // implant count, shown in the label
}

// FitImplantSlot is one of the clone's ten implant slots as the
// workbench shows it: what is plugged in (TypeID 0 = empty),
// whether the fit overrides the clone there, and whether the pilot
// lacks the skills to use it.
type FitImplantSlot struct {
	Slot       int
	TypeID     int64
	Name       string
	Overridden bool
	Missing    bool
}

type FitSimView struct {
	// ReadOnly renders the workbench as a mirror (public fit pages):
	// same look, no editing affordances (fit_workbench.html).
	ReadOnly   bool
	Cost       *FitCostView
	HasShip    bool
	DataNote   string // ship data still downloading, skills still warming, ...
	ShipID     int64
	ShipName   string
	FitName    string
	PilotID    int64
	PilotLabel string
	StateJSON  string // canonical fit document for the editor script
	// CloneID is the selected clone (0 = active clone); Clones
	// feeds the Clone dropdown; Implants is the ten slots (clone
	// plus the fit's overrides); ImplantNote covers warming /
	// re-login states.
	CloneID     int64
	Clones      []FitCloneOption
	Implants    []FitImplantSlot
	ImplantNote string
	// ImplantEffects is what the implants change on this fit's stats.
	ImplantEffects []FitImplantEffect
	Groups         []FitSlotGroup
	Visual         *FitVisualView // in-game-style ship + slot rings
	DroneBWNum     float64        // drone bandwidth (for the fittable filter)
	DroneBayNum    float64        // drone bay m3 (for the fittable filter)
	// SupportsSubsystems: the hull is a strategic cruiser (SDE
	// group), so the subsystem ring, group, and search family show.
	SupportsSubsystems bool
	// RestrictedJSON maps restricted module type IDs to their
	// requirement label ("Dreadnoughts"), for the client-side
	// add guard and drag highlighting.
	RestrictedJSON string
	ChargeSets     []FitChargeSet
	Stats          *FitStatsView
	Missing        []FitMissingSkill
	Notes          []string // plain-language modeling notes
}

// fitTypeIDs collects every type a fit document touches.
func FitDocTypeIDs(doc *FitDoc) []int64 {
	ids := []int64{doc.ShipTypeID}
	for _, it := range doc.Items {
		ids = append(ids, it.TypeID)
	}
	for _, charge := range doc.Charges {
		ids = append(ids, charge)
	}
	return ids
}

// FitPilotLevels resolves the simulate request's pilot levels:
// pilotID 0 is the All-V view; otherwise the pilot's trained
// skills load from their stored snapshot. The second return is a
// note when the pilot's skill snapshot hasn't landed yet. The
// caller owns pilot labeling and ownership checks.
func (app *Builder) FitPilotLevels(ctx context.Context, pilotID int64, snap *fit.Snapshot, doc *FitDoc) (map[int64]int, string) {
	if pilotID == 0 {
		itemIDs := make([]int64, 0, len(doc.Items))
		for _, it := range doc.Items {
			itemIDs = append(itemIDs, it.TypeID)
		}
		return fit.AllVSkillLevels(snap, doc.ShipTypeID, itemIDs), ""
	}
	var skills esi.Skills
	if !app.env.LoadSnapshot(ctx, pilotID, esi.SnapSkills, &skills) {
		return map[int64]int{}, "This pilot's skills are still on their way in — the fit below is calculated with no skills applied."
	}
	return ActiveSkillLevels(skills), ""
}

// FitPilotImplants resolves the simulate request's implant set. pilotID
// 0 (All V) has no character and so no implants; cloneID 0 is the
// active clone, otherwise the matching jump clone. Clone labels fall
// back to "Jump clone #<last 4 digits>" when ESI carries no name. Reads
// worker-warmed snapshots only. The second return is the effective
// clone ID (unknown IDs fall back to active); the note covers warming
// and re-login states.
func (app *Builder) FitPilotImplants(ctx context.Context, pilotID, cloneID int64) (ids []int64, effectiveClone int64, clones []FitCloneOption, note string) {
	if pilotID == 0 {
		return nil, 0, nil, ""
	}
	var active esi.Implants
	var jc esi.Clones
	activeOK := app.env.LoadSnapshot(ctx, pilotID, esi.SnapImplants, &active)
	clonesOK := app.env.LoadSnapshot(ctx, pilotID, esi.SnapClones, &jc)
	if !activeOK || !clonesOK {
		if app.env.SnapshotRoleMissing(ctx, pilotID, esi.SnapImplants) {
			return nil, 0, nil, "This character was linked before EveSynapse asked for clone access — sign in again to enable implants."
		}
		return nil, 0, nil, "This pilot's implant data is still on its way in — the fit below is calculated with no implants applied."
	}
	// Names the pilot gave their clones on the character sheet
	// (ESI has no naming API), so the dropdown reads the same.
	customNames := map[int64]string{}
	if userID := app.env.UserID(ctx); userID != 0 {
		if rows, err := app.queries.ListCloneNames(ctx, db.ListCloneNamesParams{UserID: userID, CharacterID: pilotID}); err != nil {
			logging.Errorf("fittings sim: clone names for character %d: %v", pilotID, err)
		} else {
			for _, r := range rows {
				customNames[r.CloneID] = strings.TrimSpace(r.CustomName)
			}
		}
	}
	clones = []FitCloneOption{{ID: 0, Name: "Active clone", Implants: len(active)}}
	for _, c := range jc.JumpClones {
		name := strings.TrimSpace(c.Name)
		if cn := customNames[c.JumpCloneID]; cn != "" {
			name = cn
		} else if name == "" || name == "Jump clone" {
			// EVE doesn't let players name jump clones, so ESI's
			// name field is usually empty or generic. Fall back to
			// the clone ID's last four digits so the dropdown
			// options stay distinguishable. A real custom name is
			// always kept. The implant count is NOT folded in here:
			// both the workbench template and handleFitClonesJSON
			// append " (n)" themselves, so it appears exactly once.
			name = fmt.Sprintf("Jump clone #%04d", c.JumpCloneID%10000)
		}
		loc := app.env.LocationTitle(ctx, c.LocationID, c.LocationType)
		clones = append(clones, FitCloneOption{
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

// BuildFitSimView runs the engine for one document and pilot and
// shapes the result for the workbench template. pilotID 0 = All V.
// ShipSupportsSubsystems reports whether the hull can fit
// subsystems. The four subsystem slots are a class rule, not a
// hull attribute, so the data-driven signal is the ship's SDE
// group: Strategic Cruiser. No hull list.
func (app *Builder) ShipSupportsSubsystems(ctx context.Context, snap *fit.Snapshot, shipTypeID int64) bool {
	gid, ok := snap.Groups[shipTypeID]
	if !ok || gid == 0 {
		return false
	}
	names := app.env.CachedGroupNames(ctx, []int64{gid})
	return names[gid] == "Strategic Cruiser"
}

func (app *Builder) BuildFitSimView(ctx context.Context, doc *FitDoc, pilotID int64, pilotLabel string, cloneID int64) *FitSimView {
	view := &FitSimView{PilotID: pilotID, FitName: doc.Name}
	state, _ := json.Marshal(doc)
	view.StateJSON = string(state)
	// What the fit costs, the one composition every page shows.
	view.Cost = app.FitCost(ctx, doc)
	if doc.ShipTypeID <= 0 {
		return view
	}

	// Implants resolve before the snapshot load so their SDE
	// types join the bounded type list.
	implantIDs, effectiveClone, clones, implantNote := app.FitPilotImplants(ctx, pilotID, cloneID)
	view.CloneID = effectiveClone
	view.Clones = clones
	view.ImplantNote = implantNote
	typeIDs := FitDocTypeIDs(doc)
	typeIDs = append(typeIDs, implantIDs...)
	for _, id := range doc.Implants {
		if id > 0 {
			typeIDs = append(typeIDs, id)
		}
	}
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
	// Bound the document's fitted quantities now that the
	// snapshot can classify its lines: every consumer below
	// (engine inputs, slot groups, the visual) trusts the
	// counts it is handed.
	SanitizeFitDoc(doc, snap)
	view.SupportsSubsystems = app.ShipSupportsSubsystems(ctx, snap, doc.ShipTypeID)

	slots := FitResolveImplantSlots(snap, implantIDs, doc.Implants)
	implantIDs = implantIDs[:0:0]
	for _, s := range slots {
		if s.TypeID > 0 {
			implantIDs = append(implantIDs, s.TypeID)
		}
	}

	levels, note := app.FitPilotLevels(ctx, pilotID, snap, doc)
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
		fam := FitSlotFamilyOf(snap, it.TypeID)
		familyOf[it.TypeID] = fam
		if !FitFamilyFitted(fam) {
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
	if len(implantIDs) > 0 {
		base := fit.Compute(snap, doc.ShipTypeID, engineItems, levels, doc.Charges, nil)
		view.ImplantEffects = FitImplantEffects(base, res)
	}

	// Names for everything the fragment prints.
	names := app.env.CachedTypeNames(ctx, typeIDs)
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok && n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", id)
	}
	view.ShipID = doc.ShipTypeID
	view.ShipName = nameOf(doc.ShipTypeID)
	for _, s := range slots {
		if s.TypeID > 0 {
			s.Name = nameOf(s.TypeID)
			s.Missing = pilotID != 0 && !FitSkillsMet(snap, s.TypeID, levels)
		}
		view.Implants = append(view.Implants, s)
	}

	view.Groups = FitBuildGroups(res, doc, snap, familyOf, nameOf, view.SupportsSubsystems)
	view.Visual = FitBuildVisual(res, doc, snap, familyOf, nameOf, view.SupportsSubsystems)
	view.DroneBWNum = res.DroneBandwidth
	view.DroneBayNum = res.DroneBayCapacity
	view.ChargeSets = app.FitBuildChargeSets(ctx, doc, snap, nameOf)
	view.Stats = FitBuildStats(res)
	// Implants count toward the pilot's missing skills like modules.
	skillItems := append([]fit.ItemInput{}, engineItems...)
	for _, id := range implantIDs {
		skillItems = append(skillItems, fit.ItemInput{TypeID: id, Quantity: 1})
	}
	view.Missing = FitMissingList(ctx, app, snap, doc, skillItems, levels, pilotID)
	view.Notes = FitPlainNotes(res.Unmodeled)
	view.RestrictedJSON = app.FitRestrictedJSON(ctx, res.Restricted, nameOf)
	for _, r := range res.Restricted {
		view.Notes = append(view.Notes,
			fmt.Sprintf("%s can only be fitted to %s.", nameOf(r.TypeID), app.FitRestrictionLabel(ctx, r)))
	}
	return view
}

// FitRestrictionLabel renders a fit.Restriction's requirement in
// plain language: "Dreadnoughts", "the Rorqual", or "X or Y".
func (app *Builder) FitRestrictionLabel(ctx context.Context, r fit.Restriction) string {
	var parts []string
	if len(r.NeedGroup) > 0 {
		gids := make([]int64, 0, len(r.NeedGroup))
		gids = append(gids, r.NeedGroup...)
		names := app.env.CachedGroupNames(ctx, gids)
		for _, g := range r.NeedGroup {
			if n, ok := names[g]; ok && n != "" {
				parts = append(parts, n+"s")
			}
		}
	}
	for _, t := range r.NeedType {
		names := app.env.CachedTypeNames(ctx, []int64{t})
		if n, ok := names[t]; ok && n != "" {
			parts = append(parts, "the "+n)
		}
	}
	if len(parts) == 0 {
		return "specific ships"
	}
	return strings.Join(parts, " or ")
}

// FitRestrictedJSON builds the client-side restriction map:
// module type ID -> requirement label.
func (app *Builder) FitRestrictedJSON(ctx context.Context, rs []fit.Restriction, nameOf func(int64) string) string {
	if len(rs) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(rs))
	for _, r := range rs {
		m[strconv.FormatInt(r.TypeID, 10)] = app.FitRestrictionLabel(ctx, r)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// EngineItemsAll returns engine inputs plus charge types, for
// requirement gathering (ammo skills count too).
func EngineItemsAll(engineItems []fit.ItemInput, doc *FitDoc) []fit.ItemInput {
	out := append([]fit.ItemInput{}, engineItems...)
	for _, charge := range doc.Charges {
		out = append(out, fit.ItemInput{TypeID: charge, Quantity: 1})
	}
	return out
}

// FitResolveImplantSlots lays the clone's implants into their slots
// (by the implantness attribute) and applies the fit's overrides.
// An override naming a type that isn't an implant for that slot is
// ignored, as is a clone implant whose slot the SDE doesn't know.
func FitResolveImplantSlots(snap *fit.Snapshot, clone []int64, overrides map[int]int64) []FitImplantSlot {
	slots := make([]FitImplantSlot, FitImplantSlots)
	for i := range slots {
		slots[i].Slot = i + 1
	}
	slotOf := func(id int64) int {
		return int(snap.Attrs[id][FitAttrImplantness])
	}
	for _, id := range clone {
		if n := slotOf(id); n >= 1 && n <= FitImplantSlots {
			slots[n-1].TypeID = id
		}
	}
	for n, id := range overrides {
		if n < 1 || n > FitImplantSlots || (id > 0 && slotOf(id) != n) {
			continue
		}
		slots[n-1].TypeID = id
		slots[n-1].Overridden = true
	}
	return slots
}

// FitSkillsMet reports whether levels cover every skill requirement
// of typeID the snapshot knows, prerequisites included.
func FitSkillsMet(snap *fit.Snapshot, typeID int64, levels map[int64]int) bool {
	seen := map[int64]bool{}
	queue := []int64{typeID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, req := range snap.Requirements[id] {
			if int64(levels[req.SkillTypeID]) < req.Level {
				return false
			}
			queue = append(queue, req.SkillTypeID)
		}
	}
	return true
}

// FitMissingList names the missing skills for a real pilot (the
// All-V view never misses anything).
func FitMissingList(ctx context.Context, app *Builder, snap *fit.Snapshot, doc *FitDoc, engineItems []fit.ItemInput, levels map[int64]int, pilotID int64) []FitMissingSkill {
	if pilotID == 0 {
		return nil
	}
	need := make(map[int64]int64)
	seenType := map[int64]bool{}
	queue := []int64{doc.ShipTypeID}
	for _, it := range EngineItemsAll(engineItems, doc) {
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
	names := app.env.CachedTypeNames(ctx, ids)
	out := make([]FitMissingSkill, 0, len(ids))
	for _, skillID := range ids {
		name := names[skillID]
		if name == "" {
			name = fmt.Sprintf("Type #%d", skillID)
		}
		out = append(out, FitMissingSkill{Name: name, Have: levels[skillID], Need: int(need[skillID])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FitBuildGroups shapes the slot grid from the engine result and
// the document's own lines.
func FitBuildGroups(res *fit.Result, doc *FitDoc, snap *fit.Snapshot, familyOf map[int64]string, nameOf func(int64) string, supportsSubsystems bool) []FitSlotGroup {
	chips := map[string][]FitSlotChip{}
	slotTotals := map[string]int{}
	for _, it := range doc.Items {
		fam := familyOf[it.TypeID]
		// Slot families chip at most a rack's worth per line and
		// a bounded total per family, whatever the document
		// claims; bay and cargo lines keep their real counts.
		qty := it.Qty
		if _, isSlot := FitVisualArcs[fam]; isSlot {
			if qty > 8 {
				qty = 8
			}
			if slotTotals[fam]+qty > 16 {
				qty = 16 - slotTotals[fam]
			}
			if qty <= 0 {
				continue
			}
			slotTotals[fam] += qty
		}
		// Uniform resolved state across the line's instances;
		// mixed lines carry "" (no single state to show).
		st, uniform := "", true
		for i := 0; i < qty; i++ {
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
		lines := FitTipLines(res, snap, doc, it.TypeID, st, FitEffAttrs(res, snap, it.TypeID, st))
		chip := FitSlotChip{TypeID: it.TypeID, Name: nameOf(it.TypeID), Qty: qty, State: st,
			Sub: FitChipSub(lines), TipLines: FitTipLinesString(lines)}
		// The states a fitted line may take, for the phone racks'
		// tap-to-cycle; bay and cargo lines are not fitted, so
		// they carry none.
		if _, isSlot := FitVisualArcs[fam]; isSlot && snap != nil {
			if vs := fit.ModuleValidStates(snap, it.TypeID); len(vs) > 0 {
				chip.StatesCSV = strings.Join(vs, ",")
			}
		}
		chips[fam] = append(chips[fam], chip)
	}
	for fam := range chips {
		sort.Slice(chips[fam], func(i, j int) bool { return chips[fam][i].Name < chips[fam][j].Name })
	}
	mk := func(key, label string, used, max int, hasMax bool) FitSlotGroup {
		if hasMax && max > 8 {
			max = 8
		}
		g := FitSlotGroup{Key: key, Label: label, Used: used, Max: max, HasMax: hasMax, Chips: chips[key]}
		if hasMax {
			g.Over = used > max
			for i := used; i < max; i++ {
				g.Empty = append(g.Empty, i)
			}
		}
		return g
	}
	droneUsed := 0
	for _, c := range chips[FitFamilyDrone] {
		droneUsed += c.Qty
	}
	cargoCount := 0
	for _, c := range chips[FitFamilyCargo] {
		cargoCount += c.Qty
	}
	groups := []FitSlotGroup{
		mk(FitFamilyHigh, "High slots", res.HighSlotsUsed, res.HighSlots, true),
		mk(FitFamilyMedium, "Mid slots", res.MediumSlotsUsed, res.MediumSlots, true),
		mk(FitFamilyLow, "Low slots", res.LowSlotsUsed, res.LowSlots, true),
		mk(FitFamilyRig, "Rigs", res.RigSlotsUsed, res.RigSlots, true),
	}
	// Subsystems only where they belong: strategic-cruiser hulls.
	// Fitted subsystems still show on any hull so an import never
	// hides modules, but the empty group does not.
	if supportsSubsystems || len(chips[FitFamilySubsystem]) > 0 {
		groups = append(groups, mk(FitFamilySubsystem, "Subsystems", len(chips[FitFamilySubsystem]), 0, false))
	}
	groups = append(groups,
		mk(FitFamilyDrone, "Drones", droneUsed, 0, false),
		mk(FitFamilyCargo, "Cargo", cargoCount, 0, false),
	)
	return groups
}

// FitChargeCandidates lists the charge types a weapon can load:
// the types in its charge groups, size-matched when the weapon
// declares a charge size.
func (app *Builder) FitChargeCandidates(ctx context.Context, weaponTypeID int64) []db.ListFitChargeTypesRow {
	attrs, err := app.queries.ListSDETypeAttributes(ctx, weaponTypeID)
	if err != nil {
		logging.Errorf("fittings sim: weapon %d attributes: %v", weaponTypeID, err)
		return nil
	}
	var groups []int64
	var size float64
	for _, a := range attrs {
		switch {
		case a.AttributeID == FitAttrChargeSize:
			size = a.Value
		default:
			for _, g := range FitChargeGroupAttrs {
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

// FitBuildChargeSets builds the per-weapon ammunition pickers.
func (app *Builder) FitBuildChargeSets(ctx context.Context, doc *FitDoc, snap *fit.Snapshot, nameOf func(int64) string) []FitChargeSet {
	var sets []FitChargeSet
	seen := map[int64]bool{}
	for _, it := range doc.Items {
		if seen[it.TypeID] {
			continue
		}
		seen[it.TypeID] = true
		if !snap.HasEffect(it.TypeID, fit.EffectTurretFitted) && !snap.HasEffect(it.TypeID, fit.EffectLauncherFitted) {
			continue
		}
		set := FitChargeSet{WeaponID: it.TypeID, WeaponName: nameOf(it.TypeID)}
		selected := doc.Charges[it.TypeID]
		for _, row := range app.FitChargeCandidates(ctx, it.TypeID) {
			set.Options = append(set.Options, FitChargeOption{
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
				set.Options = append([]FitChargeOption{{ID: selected, Name: nameOf(selected), Selected: true}}, set.Options...)
			}
		}
		sets = append(sets, set)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].WeaponName < sets[j].WeaponName })
	return sets
}

// FitPlainNotes turns the engine's unmodeled-shape list into a
// few plain-language lines (never the raw engine strings).
func FitPlainNotes(raw []string) []string {
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

// FitFmt1 formats with up to one decimal, thousands-grouped.
func FitFmt1(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return GroupThousandsFit(s)
}

// FitFmt0 formats rounded, thousands-grouped.
func FitFmt0(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	return esi.FormatInt(int64(math.Round(v)))
}

// GroupThousandsFit groups the integer part of a decimal string.
func GroupThousandsFit(s string) string {
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

// FitSeconds renders a duration the way the fitting window does.
func FitSeconds(secs float64) string {
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

// FitResist renders a resonance as a resist percentage.
func FitResist(resonance float64) string {
	pct := (1 - resonance) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return FitFmt1(pct) + "%"
}

// FitEHPText renders one EHP figure (infinite shows as a dash).
func FitEHPText(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "—"
	}
	return FitFmt0(v)
}

// FitBar builds one resource bar row.
func FitBar(label string, used, max float64) FitBarRow {
	row := FitBarRow{Label: label, Used: FitFmt1(used), Max: FitFmt1(max)}
	left := max - used
	row.Left = FitFmt1(left)
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

// FitBuildStats preformats the pyfa-vocabulary stat panel.
func FitBuildStats(res *fit.Result) *FitStatsView {
	st := &FitStatsView{}
	st.Powergrid = FitBar("Powergrid", res.PowergridUsed, res.PowergridMax)
	st.Powergrid.Used += " MW"
	st.Powergrid.Max += " MW"
	st.Powergrid.Left += " MW"
	st.CPU = FitBar("CPU", res.CPUUsed, res.CPUMax)
	st.CPU.Used += " tf"
	st.CPU.Max += " tf"
	st.CPU.Left += " tf"

	st.Capacitor = FitFmt1(res.CapacitorCapacity) + " GJ"
	st.CapPeak = FitFmt1(res.CapacitorPeakRecharge) + " GJ/s"
	st.CapDraw = FitFmt1(res.CapacitorDraw) + " GJ/s"
	if res.CapacitorStable {
		st.CapStable = true
		if res.CapacitorStablePercent >= 99.95 {
			st.CapState = "Stable"
		} else {
			st.CapState = fmt.Sprintf("Stable at %s%%", FitFmt1(res.CapacitorStablePercent))
		}
	} else {
		st.CapState = "Runs out in " + FitSeconds(res.CapacitorDepletionSecs)
	}

	st.TurretDPS = FitFmt1(res.TurretDPS)
	st.MissileDPS = FitFmt1(res.MissileDPS)
	st.DroneDPS = FitFmt1(res.DroneDPS)
	if res.SmartbombDPS > 0 {
		st.SmartbombDPS = FitFmt1(res.SmartbombDPS)
	}
	st.TotalDPS = FitFmt1(res.DPS)

	st.TurretVolley = FitFmt1(res.TurretVolley)
	st.MissileVolley = FitFmt1(res.MissileVolley)
	st.DroneVolley = FitFmt1(res.DroneVolley)
	st.TotalVolley = FitFmt1(res.Volley)

	if res.NeutDrainPerSec > 0 {
		st.NeutDrain = fmt.Sprintf("%s GJ/s (%s GJ per cycle)",
			FitFmt1(res.NeutDrainPerSec), FitFmt1(res.NeutDrainPerCycle))
	}
	if res.NosDrainPerSec > 0 {
		st.NosDrain = fmt.Sprintf("%s GJ/s (%s GJ per cycle)",
			FitFmt1(res.NosDrainPerSec), FitFmt1(res.NosDrainPerCycle))
	}

	st.Tank = []FitResistRow{
		{Layer: "Shield", HP: FitFmt0(res.ShieldHP),
			EM: FitResist(ResistOf(res, 0, true)), Thermal: FitResist(ResistOf(res, 1, true)),
			Kinetic: FitResist(ResistOf(res, 2, true)), Explosive: FitResist(ResistOf(res, 3, true)),
			EHP: FitEHPText(res.ShieldEHP.Omni)},
		{Layer: "Armor", HP: FitFmt0(res.ArmorHP),
			EM: FitResist(ResistOf(res, 0, false)), Thermal: FitResist(ResistOf(res, 1, false)),
			Kinetic: FitResist(ResistOf(res, 2, false)), Explosive: FitResist(ResistOf(res, 3, false)),
			EHP: FitEHPText(res.ArmorEHP.Omni)},
	}
	// Hull resists ride the generic resonance attributes in the
	// engine's ShipAttrs; the layer row uses EHP only for resists.
	st.Tank = append(st.Tank, FitResistRow{Layer: "Hull", HP: FitFmt0(res.HullHP),
		EM: FitResist(res.ShipAttrs[fit.AttrResonanceEM]), Thermal: FitResist(res.ShipAttrs[fit.AttrResonanceThermal]),
		Kinetic: FitResist(res.ShipAttrs[fit.AttrResonanceKinetic]), Explosive: FitResist(res.ShipAttrs[fit.AttrResonanceExplosive]),
		EHP: FitEHPText(res.HullEHP.Omni)})
	st.EHPOmni = FitEHPText(res.EHP.Omni)

	st.ShieldReg = FitFmt1(res.ShieldRegenPeak) + " HP/s"
	st.ShieldBoost = FitFmt1(res.ShieldBoostRate) + " HP/s"
	st.ArmorRep = FitFmt1(res.ArmorRepairRate) + " HP/s"
	st.HullRep = FitFmt1(res.HullRepairRate) + " HP/s"

	st.Velocity = FitFmt0(res.Velocity) + " m/s"
	st.Align = FitFmt1(res.AlignSeconds) + " s"
	st.Signature = FitFmt0(res.SignatureRadius) + " m"

	if res.MaxTargetRange >= 1000 {
		st.TargetRng = FitFmt1(res.MaxTargetRange/1000) + " km"
	} else {
		st.TargetRng = FitFmt0(res.MaxTargetRange) + " m"
	}
	st.ScanRes = FitFmt0(res.ScanResolution) + " mm"
	st.Sensor = FitFmt1(res.SensorStrength)
	st.LockedTgts = FitFmt0(res.MaxLockedTargets)
	st.Cargo = FitFmt1(res.CargoCapacity) + " m³"

	st.DroneBW = fmt.Sprintf("%s / %s Mbit/s", FitFmt1(res.DroneBandwidthUsed), FitFmt1(res.DroneBandwidth))
	st.DroneBay = fmt.Sprintf("%s / %s m³", FitFmt1(res.DroneBayUsed), FitFmt1(res.DroneBayCapacity))
	st.DronesUp = fmt.Sprintf("%d in space of %d carried", res.DronesActive, res.DronesFitted)

	st.Hardpoints = fmt.Sprintf("Turrets %d/%d · Launchers %d/%d",
		res.TurretHardpointsUsed, res.TurretHardpoints, res.LauncherHardpointsUsed, res.LauncherHardpoints)
	st.HPOver = res.TurretHardpointsUsed > res.TurretHardpoints || res.LauncherHardpointsUsed > res.LauncherHardpoints
	st.Calibration = fmt.Sprintf("%s / %s", FitFmt0(res.CalibrationUsed), FitFmt0(res.CalibrationMax))
	st.CalibOver = res.CalibrationUsed > res.CalibrationMax && res.CalibrationMax > 0
	st.SlotLine = fmt.Sprintf("High %d/%d · Mid %d/%d · Low %d/%d · Rigs %d/%d",
		res.HighSlotsUsed, res.HighSlots, res.MediumSlotsUsed, res.MediumSlots,
		res.LowSlotsUsed, res.LowSlots, res.RigSlotsUsed, res.RigSlots)
	st.SlotsOver = res.HighSlotsUsed > res.HighSlots || res.MediumSlotsUsed > res.MediumSlots ||
		res.LowSlotsUsed > res.LowSlots || res.RigSlotsUsed > res.RigSlots

	st.Calib = FitBar("Calibration", res.CalibrationUsed, res.CalibrationMax)
	st.Calib.Used = FitFmt0(res.CalibrationUsed)
	st.Calib.Max = FitFmt0(res.CalibrationMax)
	st.Calib.Left = FitFmt0(res.CalibrationMax - res.CalibrationUsed)
	st.Tiles = []FitTile{
		{"Turrets", fmt.Sprintf("%d / %d", res.TurretHardpointsUsed, res.TurretHardpoints), res.TurretHardpointsUsed > res.TurretHardpoints},
		{"Launchers", fmt.Sprintf("%d / %d", res.LauncherHardpointsUsed, res.LauncherHardpoints), res.LauncherHardpointsUsed > res.LauncherHardpoints},
		{"Calibration", st.Calibration, st.CalibOver},
	}
	layers := [][4]float64{
		{ResistOf(res, 0, true), ResistOf(res, 1, true), ResistOf(res, 2, true), ResistOf(res, 3, true)},
		{ResistOf(res, 0, false), ResistOf(res, 1, false), ResistOf(res, 2, false), ResistOf(res, 3, false)},
		{res.ShipAttrs[fit.AttrResonanceEM], res.ShipAttrs[fit.AttrResonanceThermal], res.ShipAttrs[fit.AttrResonanceKinetic], res.ShipAttrs[fit.AttrResonanceExplosive]},
	}
	for i := range st.Tank {
		for k, kind := range []string{"em", "th", "kin", "exp"} {
			st.Tank[i].Resists = append(st.Tank[i].Resists, FitResistCellOf(kind, layers[i][k]))
		}
	}
	st.Problems = FitProblems(res)
	return st
}

// FitResistCellOf is one resist chip from a layer resonance.
func FitResistCellOf(kind string, resonance float64) FitResistCell {
	pct := math.Max(0, math.Min(100, (1-resonance)*100))
	return FitResistCell{Kind: kind, Text: FitResist(resonance), Pct: pct}
}

// FitProblems lists what keeps a fit from fitting: resources over
// their output, slots and hardpoints over the hull's count, and
// calibration over the rig budget, one short line each.
func FitProblems(res *fit.Result) []string {
	var out []string
	if res.PowergridMax > 0 && res.PowergridUsed > res.PowergridMax {
		out = append(out, "Powergrid over by "+FitFmt1(res.PowergridUsed-res.PowergridMax)+" MW")
	}
	if res.CPUMax > 0 && res.CPUUsed > res.CPUMax {
		out = append(out, "CPU over by "+FitFmt1(res.CPUUsed-res.CPUMax)+" tf")
	}
	if res.CalibrationMax > 0 && res.CalibrationUsed > res.CalibrationMax {
		out = append(out, "Calibration over by "+FitFmt0(res.CalibrationUsed-res.CalibrationMax))
	}
	for _, s := range []struct {
		label     string
		used, max int
	}{
		{"High slots", res.HighSlotsUsed, res.HighSlots},
		{"Mid slots", res.MediumSlotsUsed, res.MediumSlots},
		{"Low slots", res.LowSlotsUsed, res.LowSlots},
		{"Rig slots", res.RigSlotsUsed, res.RigSlots},
		{"Turret hardpoints", res.TurretHardpointsUsed, res.TurretHardpoints},
		{"Launcher hardpoints", res.LauncherHardpointsUsed, res.LauncherHardpoints},
	} {
		if s.used > s.max {
			out = append(out, fmt.Sprintf("%s %d / %d", s.label, s.used, s.max))
		}
	}
	return out
}

// ResistOf reads a layer resonance back out of the computed
// ship attributes (shield when shield=true, armor otherwise);
// kind: 0 EM, 1 thermal, 2 kinetic, 3 explosive.
func ResistOf(res *fit.Result, kind int, shield bool) float64 {
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

// ParseFitTags turns the header's comma-separated tags field into
// a clean array: trimmed, deduped, capped.
func ParseFitTags(raw string) []string {
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

// FitDocFromESI turns one saved EVE fitting into an editor
// document. Cargo ammunition that fits exactly one fitted
// weapon group is loaded into that group; the rest stays cargo.
func (app *Builder) FitDocFromESI(ctx context.Context, f esi.Fitting) *FitDoc {
	doc := &FitDoc{Name: f.Name, ShipTypeID: f.ShipTypeID, Charges: map[int64]int64{}}
	var cargoIDs []int64
	for _, it := range f.Items {
		qty := int(it.Quantity)
		if qty < 1 {
			qty = 1
		}
		doc.Items = append(doc.Items, FitDocItem{TypeID: it.TypeID, Qty: qty})
		if strings.HasPrefix(it.Flag, "Cargo") {
			cargoIDs = append(cargoIDs, it.TypeID)
		}
	}
	if len(cargoIDs) == 0 || len(doc.Items) == 0 {
		return doc
	}

	snap, err := fit.LoadSnapshot(ctx, app.queries, FitDocTypeIDs(doc))
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
			for _, row := range app.FitChargeCandidates(ctx, weapon) {
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

var (
	EftHeaderRe = regexp.MustCompile(`^\[(.+),\s*(.+)\]$`)
	EftQtyRe    = regexp.MustCompile(`^(.*?)\s+x(\d+)$`)
)

// ParseEFT turns pasted EFT text into a fit document. Item names
// resolve against the local type table; anything unresolved is
// reported as a note, never silently dropped. [empty] slot
// lines are accepted and ignored.
func (app *Builder) ParseEFT(ctx context.Context, text string) (*FitDoc, []string) {
	// The paste is bounded before any line is read: a real fit
	// is a few dozen lines, so 64 KiB / 500 lines is generous
	// headroom, and anything past it is not a fit. Name lookups
	// are memoized per call — a pasted fit repeats its modules
	// line after line, and each distinct name costs one query.
	if len(text) > 64<<10 {
		text = text[:64<<10]
	}
	doc := &FitDoc{Charges: map[int64]int64{}}
	var notes []string

	memo := make(map[string]db.GetSDETypeByNameRow)
	missed := make(map[string]bool)
	lookup := func(name string) (int64, bool) {
		if row, ok := memo[name]; ok {
			return row.TypeID, true
		}
		if missed[name] {
			return 0, false
		}
		row, err := app.queries.GetSDETypeByName(ctx, name)
		if err != nil {
			missed[name] = true
			return 0, false
		}
		memo[name] = row
		return row.TypeID, true
	}

	lines := strings.Split(text, "\n")
	if len(lines) > 500 {
		lines = lines[:500]
	}
	idx := 0
	for ; idx < len(lines); idx++ {
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			continue
		}
		m := EftHeaderRe.FindStringSubmatch(line)
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
		if m := EftQtyRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
				qty = n
				body = strings.TrimSpace(m[1])
			}
		}
		if id, ok := lookup(body); ok {
			doc.Items = append(doc.Items, FitDocItem{TypeID: id, Qty: qty})
			continue
		}
		// pyfa-style "Module, Charge" line.
		if cut := strings.LastIndex(body, ", "); cut > 0 {
			if modID, ok := lookup(strings.TrimSpace(body[:cut])); ok {
				if chargeID, ok := lookup(strings.TrimSpace(body[cut+2:])); ok {
					doc.Items = append(doc.Items, FitDocItem{TypeID: modID, Qty: qty})
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
	SanitizeFitDoc(doc)
	return doc, notes
}

// FormatEFT renders a fit document as EFT text: the [Ship,
// Name] header, low/mid/high/rig/subsystem groups in EFT order,
// then drones and cargo with quantities. Loaded ammunition rides
// on its weapon's line ("Module, Charge"), the shape fitting
// tools read back.
func (app *Builder) FormatEFT(ctx context.Context, doc *FitDoc) string {
	docCopy := *doc
	SanitizeFitDoc(&docCopy)
	doc = &docCopy
	if doc.ShipTypeID <= 0 {
		return ""
	}
	names := app.env.CachedTypeNames(ctx, FitDocTypeIDs(doc))
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
	if snap, err := fit.LoadSnapshot(ctx, app.queries, FitDocTypeIDs(doc)); err == nil {
		for _, it := range doc.Items {
			familyOf[it.TypeID] = FitSlotFamilyOf(snap, it.TypeID)
		}
	}
	weaponCharge := func(typeID int64) string {
		if charge, ok := doc.Charges[typeID]; ok && charge > 0 {
			return ", " + nameOf(charge)
		}
		return ""
	}

	var sections []string
	for _, family := range []string{FitFamilyLow, FitFamilyMedium, FitFamilyHigh, FitFamilyRig, FitFamilySubsystem} {
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
	for _, family := range []string{FitFamilyDrone, FitFamilyCargo} {
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
	known := map[string]bool{FitFamilyLow: true, FitFamilyMedium: true, FitFamilyHigh: true,
		FitFamilyRig: true, FitFamilySubsystem: true, FitFamilyDrone: true, FitFamilyCargo: true}
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

// FittingItemRow is one fitted item line.
type FittingItemRow struct {
	Name     string
	TypeID   int64
	Quantity string // thousands-separated
}

// FittingGroup is one slot-category block of a fitting.
type FittingGroup struct {
	Label string
	Items []FittingItemRow
}

// PublicFitSections groups a stored document's items the way the
// saved-fits list does, resolving every name from the local
// caches. Classification needs the dogma snapshot; without it
// every line lands in Other rather than vanishing.
func (app *Builder) PublicFitSections(ctx context.Context, doc *FitDoc) []FittingGroup {
	typeIDs := []int64{doc.ShipTypeID}
	for _, it := range doc.Items {
		typeIDs = append(typeIDs, it.TypeID)
	}
	names := app.env.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok && n != "" {
			return n
		}
		return "Type #" + idString(typeID)
	}
	familyOf := map[int64]string{}
	if snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs); err == nil && snap != nil {
		for _, id := range typeIDs {
			familyOf[id] = FitSlotFamilyOf(snap, id)
		}
	}
	familyLabel := map[string]string{
		FitFamilyHigh: "High slots", FitFamilyMedium: "Medium slots",
		FitFamilyLow: "Low slots", FitFamilyRig: "Rig slots",
		FitFamilySubsystem: "Subsystem slots", FitFamilyDrone: "Drone bay",
		FitFamilyCargo: "Cargo hold",
	}
	byFamily := map[string][]FittingItemRow{}
	order := []string{}
	for _, it := range doc.Items {
		family := familyOf[it.TypeID]
		if _, ok := familyLabel[family]; !ok {
			family = "other"
		}
		if _, ok := byFamily[family]; !ok {
			order = append(order, family)
		}
		byFamily[family] = append(byFamily[family], FittingItemRow{
			Name: nameOf(it.TypeID), TypeID: it.TypeID,
			Quantity: esi.FormatInt(int64(it.Qty)),
		})
	}
	ordered := []string{FitFamilyHigh, FitFamilyMedium, FitFamilyLow, FitFamilyRig, FitFamilySubsystem, FitFamilyDrone, FitFamilyCargo}
	seen := map[string]bool{}
	var sections []FittingGroup
	for _, family := range ordered {
		items, ok := byFamily[family]
		if !ok {
			continue
		}
		seen[family] = true
		sections = append(sections, FittingGroup{Label: familyLabel[family], Items: items})
	}
	for _, family := range order {
		if seen[family] {
			continue
		}
		sections = append(sections, FittingGroup{Label: "Other", Items: byFamily[family]})
	}
	return sections
}

// FitCostView is what a fit costs, as every page shows it: the
// fitting tool's stats panel, the public fit page and its card, and
// the browser's price filter all read this one composition.
type FitCostView struct {
	Known    bool
	Hull     string // the ship itself
	Parts    string // every line on the hull: modules, rigs, drones, cargo
	Total    string
	TotalRaw float64
	HullRaw  float64
	PartsRaw float64
	Unpriced int // lines no house price source knows (hull included)
}

// FitUnitPrice resolves one unit's price for fit valuation in
// the house order: the swept typical sell (The Forge's book,
// with a global-market type's own global book filling in where
// The Forge carries no row for it), then the
// stored true-value estimated sell (trueValues, nil when the
// worker has stored none). A type neither source knows is
// unpriced — CCP's guide average is banned as a price source
// (owner doctrine, 2026-10-17) and its adjusted price is not
// one either; it survives only as the EIV input to job fees.
// The estimated sell is what gives a supercapital hull an
// actual value: it has no swept market to take a typical from,
// while its stored estimate tracks real contract trades.
// RepriceLocalFittings makes the same choices in SQL: change
// both.
func FitUnitPrice(typicals, trueValues map[int64]float64, typeID int64) (float64, bool) {
	if v, ok := typicals[typeID]; ok && v > 0 {
		return v, true
	}
	if est, ok := trueValues[typeID]; ok && est > 0 {
		return est, true
	}
	return 0, false
}

// FitCost prices a document from the stored price sources: the
// hull plus every item line. Known is false when nothing in it
// is priced (no sweep or estimate stored yet); the browser
// treats a 0 total as unknown.
func (app *Builder) FitCost(ctx context.Context, doc *FitDoc) *FitCostView {
	c := &FitCostView{}
	typicals := app.env.TypicalSells(ctx)
	trueValues := app.env.TrueValues(ctx)
	if (len(typicals) == 0 && len(trueValues) == 0) || doc == nil || doc.ShipTypeID == 0 {
		return c
	}
	if p, ok := FitUnitPrice(typicals, trueValues, doc.ShipTypeID); ok {
		c.HullRaw = p
	} else {
		c.Unpriced++
	}
	for _, it := range doc.Items {
		if p, ok := FitUnitPrice(typicals, trueValues, it.TypeID); ok {
			c.PartsRaw += float64(it.Qty) * p
		} else {
			c.Unpriced++
		}
	}
	c.TotalRaw = c.HullRaw + c.PartsRaw
	if c.TotalRaw > 0 {
		c.Known = true
		c.Hull = esi.FormatISK(c.HullRaw)
		c.Parts = esi.FormatISK(c.PartsRaw)
		c.Total = esi.FormatISK(c.TotalRaw)
	}
	return c
}

// FitDocPrice is FitCost's total, stored on the fit for the
// browser's max-price filter and sorts (0 = unknown). The worker
// makes the same sum in SQL whenever prices refresh
// (RepriceLocalFittings): change both together.
func (app *Builder) FitDocPrice(ctx context.Context, doc *FitDoc) float64 {
	return app.FitCost(ctx, doc).TotalRaw
}

// Env is what the foundation needs from the application that is not the
// static data: the name caches, stored snapshots, who is asking, and the
// price sources. Core fills it; nothing here reaches the network.
type Env interface {
	CachedTypeNames(ctx context.Context, ids []int64) map[int64]string
	CachedGroupNames(ctx context.Context, ids []int64) map[int64]string
	// LoadSnapshot reads one stored snapshot of a character; false when
	// it is not there yet. It never fetches.
	LoadSnapshot(ctx context.Context, characterID int64, kind string, out any) bool
	// UserID is the signed-in account, 0 when anonymous.
	UserID(ctx context.Context) int64
	LocationTitle(ctx context.Context, id int64, locType string) string
	// SnapshotRoleMissing reports that the last fetch of a snapshot
	// failed because the character was linked without the needed scope.
	SnapshotRoleMissing(ctx context.Context, characterID int64, kind string) bool
	// TypicalSells is the swept typical sell per type (The
	// Forge's book, global-market types filled from their own
	// global book; nil when no sweep has landed yet): fit
	// valuation's first tier.
	TypicalSells(ctx context.Context) map[int64]float64
	// TrueValues is the worker's estimated sell per type for
	// types the market cannot price (nil when none are
	// stored): fit valuation's second tier.
	TrueValues(ctx context.Context) map[int64]float64
}

// Builder builds fit views from a fit document over the static data
// (queries) and the application's stored state (env).
type Builder struct {
	queries *db.Queries
	env     Env
}

// New returns a Builder.
func New(queries *db.Queries, env Env) *Builder { return &Builder{queries: queries, env: env} }

// ActiveSkillLevels is each trained skill's active level.
func ActiveSkillLevels(skills esi.Skills) map[int64]int {
	levels := make(map[int64]int, len(skills.Skills))
	for _, s := range skills.Skills {
		levels[s.SkillID] = s.ActiveSkillLevel
	}
	return levels
}

func idString(id int64) string { return strconv.FormatInt(id, 10) }
