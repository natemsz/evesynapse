package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Fitting simulator UI (v0.3.21): the editor on the Fittings page.
//
// The browser keeps a small fit document (ship, item lines, charge
// choices) and posts it to /fittings/simulate/ on every change; the
// server runs the dogma engine (fitengine.go) over local SDE rows
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
	fitFamilyHigh:      fitEffectHiPower,
	fitFamilyMedium:    fitEffectMedPower,
	fitFamilyLow:       fitEffectLoPower,
	fitFamilyRig:       fitEffectRigSlot,
	fitFamilySubsystem: fitEffectSubsystemSlot,
}

// Dogma attributes the charge picker reads off a weapon: the five
// charge-group slots and the charge size (dgmAttributeTypes,
// verified against the live dump).
var fitChargeGroupAttrs = []int64{604, 605, 606, 609, 610}

const fitAttrChargeSize = 128

// fitDocItem is one line of a fit document.
type fitDocItem struct {
	TypeID int64 `json:"typeId"`
	Qty    int   `json:"qty"`
}

// fitDoc is one fitting: the editor's working state and the
// stored shape of a local fitting (local_fittings.items_json).
type fitDoc struct {
	Name       string          `json:"name"`
	ShipTypeID int64           `json:"shipTypeId"`
	Items      []fitDocItem    `json:"items"`
	Charges    map[int64]int64 `json:"charges"` // weapon type -> loaded charge type
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
func fitSlotFamilyOf(snap *fitSnapshot, typeID int64) string {
	switch {
	case snap.hasEffect(typeID, fitEffectRigSlot):
		return fitFamilyRig
	case snap.hasEffect(typeID, fitEffectSubsystemSlot):
		return fitFamilySubsystem
	case snap.attrs[typeID][fitAttrDroneBandwidthUsed] > 0:
		return fitFamilyDrone
	case snap.hasEffect(typeID, fitEffectHiPower):
		return fitFamilyHigh
	case snap.hasEffect(typeID, fitEffectMedPower):
		return fitFamilyMedium
	case snap.hasEffect(typeID, fitEffectLoPower):
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

// fitSlotChip is one fitted item line in the slot grid.
type fitSlotChip struct {
	TypeID int64
	Name   string
	Qty    int
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
	X, Y       float64 // center position, percent of the visual box
}

// fitVisualView is the ship render with module slots arranged
// around it, in-game style.
type fitVisualView struct {
	ShipID   int64
	ShipName string
	Slots    []fitVisualSlot
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

// fitBuildVisual shapes the visual fit display from the engine
// result and the document's own lines: one circle per slot,
// filled circles carrying their module, positioned on arcs around
// the ship render. Positions are precomputed here so the template
// stays declarative.
func fitBuildVisual(res *fitResult, doc *fitDoc, familyOf map[int64]string, nameOf func(int64) string) *fitVisualView {
	v := &fitVisualView{ShipID: doc.ShipTypeID, ShipName: nameOf(doc.ShipTypeID)}
	maxOf := map[string]int{
		fitFamilyHigh:   res.HighSlots,
		fitFamilyMedium: res.MediumSlots,
		fitFamilyLow:    res.LowSlots,
		fitFamilyRig:    res.RigSlots,
	}
	// Fitted modules per family, quantities expanded, doc order.
	fitted := map[string][]int64{}
	for _, it := range doc.Items {
		fam := familyOf[it.TypeID]
		if _, ok := fitVisualArcs[fam]; !ok {
			continue
		}
		for i := 0; i < it.Qty; i++ {
			fitted[fam] = append(fitted[fam], it.TypeID)
		}
	}
	maxOf[fitFamilySubsystem] = len(fitted[fitFamilySubsystem])
	for _, fam := range []string{fitFamilyHigh, fitFamilyMedium, fitFamilyLow, fitFamilyRig, fitFamilySubsystem} {
		arc := fitVisualArcs[fam]
		max := maxOf[fam]
		if max <= 0 {
			continue
		}
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
				X:          50 + arc[2]*math.Cos(ang),
				Y:          50 + arc[2]*math.Sin(ang),
			}
			if i < len(ids) {
				s.Filled = true
				s.TypeID = ids[i]
				s.Name = nameOf(ids[i])
			}
			v.Slots = append(v.Slots, s)
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
	Powergrid   fitBarRow
	CPU         fitBarRow
	Align       string
	Velocity    string
	Signature   string
	Capacitor   string // capacity
	CapPeak     string
	CapDraw     string
	CapState    string
	CapStable   bool
	TurretDPS   string
	MissileDPS  string
	DroneDPS    string
	TotalDPS    string
	Tank        []fitResistRow
	EHPOmni     string
	ShieldReg   string
	ShieldBoost string
	ArmorRep    string
	HullRep     string
	TargetRng   string
	ScanRes     string
	Sensor      string
	LockedTgts  string
	Cargo       string
	DroneBW     string
	DroneBay    string
	DronesUp    string
	Hardpoints  string // "Turrets 3/4 · Launchers 0/0"
	HPOver      bool
	Calibration string
	CalibOver   bool
	SlotLine    string // "High 3/3 · Medium 2/3 · Low 1/2 · Rigs 0/3"
	SlotsOver   bool
}

// fitMissingSkill is one unmet requirement for the chosen pilot.
type fitMissingSkill struct {
	Name string
	Have int
	Need int
}

// fitSimView is the workbench fragment (and its server-rendered
// first paint inside the Fittings page).
type fitSimView struct {
	HasShip     bool
	DataNote    string // ship data still downloading, skills still warming, ...
	ShipID      int64
	ShipName    string
	FitName     string
	PilotID     int64
	PilotLabel  string
	StateJSON   string // canonical fit document for the editor script
	Groups      []fitSlotGroup
	Visual      *fitVisualView // in-game-style ship + slot rings
	DroneBWNum  float64        // drone bandwidth (for the fittable filter)
	DroneBayNum float64        // drone bay m3 (for the fittable filter)
	ChargeSets  []fitChargeSet
	Stats       *fitStatsView
	Missing     []fitMissingSkill
	Notes       []string // plain-language modeling notes
}

// fitEditorView is the editor chrome around the workbench.
type fitEditorView struct {
	PilotID   int64
	Pilots    []assetCharLink // the user's characters; the template adds All V
	Sim       *fitSimView
	LocalID   int64 // saved fit currently open (0 = unsaved)
	LocalFits []localFitRow
	Notes     []string // import notes ("couldn't place ..."), shown once
}

// localFitRow is one saved fit in the page's list.
type localFitRow struct {
	ID       int64
	Name     string
	ShipName string
	Updated  string
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
func (app *Application) fitPilotLevels(ctx context.Context, pilotID int64, snap *fitSnapshot, doc *fitDoc) (map[int64]int, string) {
	if pilotID == 0 {
		itemIDs := make([]int64, 0, len(doc.Items))
		for _, it := range doc.Items {
			itemIDs = append(itemIDs, it.TypeID)
		}
		return fitAllVSkillLevels(snap, doc.ShipTypeID, itemIDs), ""
	}
	var skills esi.Skills
	if !app.loadCorpSnapshot(ctx, pilotID, esi.SnapSkills, &skills) {
		return map[int64]int{}, "This pilot's skills are still on their way in — the fit below is calculated with no skills applied."
	}
	return briefingSkillLevels(skills), ""
}

// buildFitSimView runs the engine for one document and pilot and
// shapes the result for the workbench template. pilotID 0 = All V.
func (app *Application) buildFitSimView(ctx context.Context, doc *fitDoc, pilotID int64, pilotLabel string) *fitSimView {
	view := &fitSimView{PilotID: pilotID, FitName: doc.Name}
	state, _ := json.Marshal(doc)
	view.StateJSON = string(state)
	if doc.ShipTypeID <= 0 {
		return view
	}

	typeIDs := fitDocTypeIDs(doc)
	snap, err := loadFitSnapshot(ctx, app.queries, typeIDs)
	if err != nil {
		log.Printf("fittings sim: load snapshot for ship %d: %v", doc.ShipTypeID, err)
		view.DataNote = "Something went wrong reading the ship data — check the server log."
		return view
	}
	if len(snap.attrs[doc.ShipTypeID]) == 0 {
		view.DataNote = "The ship and module data is still downloading — give it a little while and try again."
		return view
	}
	view.HasShip = true

	levels, note := app.fitPilotLevels(ctx, pilotID, snap, doc)
	view.PilotLabel = pilotLabel
	if note != "" {
		view.DataNote = note
	}

	// Classify the fit's lines; only fitted families reach the
	// engine (cargo is display-only).
	familyOf := make(map[int64]string, len(doc.Items))
	engineItems := make([]fitItemInput, 0, len(doc.Items))
	for _, it := range doc.Items {
		fam := fitSlotFamilyOf(snap, it.TypeID)
		familyOf[it.TypeID] = fam
		if fitFamilyFitted(fam) {
			engineItems = append(engineItems, fitItemInput{TypeID: it.TypeID, Quantity: it.Qty})
		}
	}
	res := computeFit(snap, doc.ShipTypeID, engineItems, levels, doc.Charges)

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

	view.Groups = fitBuildGroups(res, doc, familyOf, nameOf)
	view.Visual = fitBuildVisual(res, doc, familyOf, nameOf)
	view.DroneBWNum = res.DroneBandwidth
	view.DroneBayNum = res.DroneBayCapacity
	view.ChargeSets = app.fitBuildChargeSets(ctx, doc, snap, nameOf)
	view.Stats = fitBuildStats(res)
	view.Missing = fitMissingList(ctx, app, snap, doc, engineItems, levels, pilotID)
	view.Notes = fitPlainNotes(res.Unmodeled)
	return view
}

// engineItemsAll returns engine inputs plus charge types, for
// requirement gathering (ammo skills count too).
func engineItemsAll(engineItems []fitItemInput, doc *fitDoc) []fitItemInput {
	out := append([]fitItemInput{}, engineItems...)
	for _, charge := range doc.Charges {
		out = append(out, fitItemInput{TypeID: charge, Quantity: 1})
	}
	return out
}

// fitMissingList names the missing skills for a real pilot (the
// All-V view never misses anything).
func fitMissingList(ctx context.Context, app *Application, snap *fitSnapshot, doc *fitDoc, engineItems []fitItemInput, levels map[int64]int, pilotID int64) []fitMissingSkill {
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
		for _, req := range snap.requirements[typeID] {
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
func fitBuildGroups(res *fitResult, doc *fitDoc, familyOf map[int64]string, nameOf func(int64) string) []fitSlotGroup {
	chips := map[string][]fitSlotChip{}
	for _, it := range doc.Items {
		fam := familyOf[it.TypeID]
		chips[fam] = append(chips[fam], fitSlotChip{TypeID: it.TypeID, Name: nameOf(it.TypeID), Qty: it.Qty})
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
	return []fitSlotGroup{
		mk(fitFamilyHigh, "High slots", res.HighSlotsUsed, res.HighSlots, true),
		mk(fitFamilyMedium, "Mid slots", res.MediumSlotsUsed, res.MediumSlots, true),
		mk(fitFamilyLow, "Low slots", res.LowSlotsUsed, res.LowSlots, true),
		mk(fitFamilyRig, "Rigs", res.RigSlotsUsed, res.RigSlots, true),
		mk(fitFamilySubsystem, "Subsystems", len(chips[fitFamilySubsystem]), 0, false),
		mk(fitFamilyDrone, "Drones", droneUsed, 0, false),
		mk(fitFamilyCargo, "Cargo", cargoCount, 0, false),
	}
}

// fitChargeCandidates lists the charge types a weapon can load:
// the types in its charge groups, size-matched when the weapon
// declares a charge size.
func (app *Application) fitChargeCandidates(ctx context.Context, weaponTypeID int64) []db.ListFitChargeTypesRow {
	attrs, err := app.queries.ListSDETypeAttributes(ctx, weaponTypeID)
	if err != nil {
		log.Printf("fittings sim: weapon %d attributes: %v", weaponTypeID, err)
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
		log.Printf("fittings sim: charge candidates for %d: %v", weaponTypeID, err)
		return nil
	}
	return rows
}

// fitBuildChargeSets builds the per-weapon ammunition pickers.
func (app *Application) fitBuildChargeSets(ctx context.Context, doc *fitDoc, snap *fitSnapshot, nameOf func(int64) string) []fitChargeSet {
	var sets []fitChargeSet
	seen := map[int64]bool{}
	for _, it := range doc.Items {
		if seen[it.TypeID] {
			continue
		}
		seen[it.TypeID] = true
		if !snap.hasEffect(it.TypeID, fitEffectTurretFitted) && !snap.hasEffect(it.TypeID, fitEffectLauncherFitted) {
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
		case strings.Contains(s, "category 5"):
			add("Heating a module (overload) isn't included in these numbers.")
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
func fitBuildStats(res *fitResult) *fitStatsView {
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
		EM: fitResist(res.ShipAttrs[fitAttrResonanceEM]), Thermal: fitResist(res.ShipAttrs[fitAttrResonanceThermal]),
		Kinetic: fitResist(res.ShipAttrs[fitAttrResonanceKinetic]), Explosive: fitResist(res.ShipAttrs[fitAttrResonanceExplosive]),
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
func resistOf(res *fitResult, kind int, shield bool) float64 {
	attrs := res.ShipAttrs
	switch {
	case shield && kind == 0:
		return attrs[fitAttrShieldEM]
	case shield && kind == 1:
		return attrs[fitAttrShieldThermal]
	case shield && kind == 2:
		return attrs[fitAttrShieldKinetic]
	case shield && kind == 3:
		return attrs[fitAttrShieldExplosive]
	case !shield && kind == 0:
		return attrs[fitAttrArmorEM]
	case !shield && kind == 1:
		return attrs[fitAttrArmorThermal]
	case !shield && kind == 2:
		return attrs[fitAttrArmorKinetic]
	default:
		return attrs[fitAttrArmorExplosive]
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
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))

	pilotLabel := "All V"
	if req.Pilot != 0 {
		label, owned := app.fitPilotLabel(ctx, userID, req.Pilot)
		if !owned {
			http.Error(w, "That pilot isn't one of your characters.", http.StatusBadRequest)
			return
		}
		pilotLabel = label
	}
	view := app.buildFitSimView(ctx, &req.fitDoc, req.Pilot, pilotLabel)
	app.renderFragment(w, "fittings.html", "fit-workbench", view)
}

// fitPilotLabel resolves a pilot character ID to its name when
// it belongs to the signed-in user.
func (app *Application) fitPilotLabel(ctx context.Context, userID, pilotID int64) (string, bool) {
	if userID == 0 || pilotID == 0 {
		return "", false
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("fittings sim: list characters for user %d: %v", userID, err)
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
				log.Printf("fittings picker: requirements: %v", err)
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
		{fitFamilyHigh, fitEffectHiPower, "high", 8},
		{fitFamilyMedium, fitEffectMedPower, "medium", 8},
		{fitFamilyLow, fitEffectLoPower, "low", 8},
		{fitFamilyRig, fitEffectRigSlot, "rig", 6},
		{fitFamilySubsystem, fitEffectSubsystemSlot, "subsystem", 6},
	}
	querySlotFamily := func(family, kind string, lim int64) []suggestItem {
		effectID := fitSlotEffectByFamily[family]
		rows, err := app.queries.ListFitSlotTypes(ctx, db.ListFitSlotTypesParams{
			EffectID: effectID, Q: query, Lim: lim, Meta: meta,
		})
		if err != nil {
			log.Printf("fittings picker: family %s: %v", family, err)
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
			log.Printf("fittings picker: ships: %v", err)
		}
		for _, sk := range slotKinds {
			out = append(out, querySlotFamily(sk.family, sk.kind, sk.lim)...)
		}
		if rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 6, Meta: meta}); err == nil {
			for _, row := range rows {
				out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "drone"})
			}
		} else {
			log.Printf("fittings picker: drones: %v", err)
		}
	case family == "ship":
		rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: query, Lim: 12})
		if err != nil {
			log.Printf("fittings picker: ships: %v", err)
			break
		}
		for _, row := range rows {
			out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: row.GroupName, Kind: "ship"})
		}
	case family == fitFamilyDrone:
		rows, err := app.queries.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: query, Lim: 25, Meta: meta})
		if err != nil {
			log.Printf("fittings picker: drones: %v", err)
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

// handleFitSave serves POST /fittings/save/: create or update
// one of the user's local fittings.
func (app *Application) handleFitSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Error(w, "Sign in before saving a fit.", http.StatusForbidden)
		return
	}
	var req struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Fit  fitDoc `json:"fit"`
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
	req.Fit.Name = name
	raw, err := json.Marshal(req.Fit)
	if err != nil {
		http.Error(w, "That fitting couldn't be saved.", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)

	id := req.ID
	if id > 0 {
		if _, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: id, UserID: userID}); err != nil {
			http.Error(w, "That saved fit couldn't be found.", http.StatusNotFound)
			return
		}
		if err := app.queries.UpdateLocalFitting(ctx, db.UpdateLocalFittingParams{
			Name: name, ShipTypeID: req.Fit.ShipTypeID, ItemsJson: string(raw),
			UpdatedAt: now, ID: id, UserID: userID,
		}); err != nil {
			log.Printf("fittings save: update %d: %v", id, err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
	} else {
		row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: userID, Name: name, ShipTypeID: req.Fit.ShipTypeID,
			ItemsJson: string(raw), CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			log.Printf("fittings save: create: %v", err)
			http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
			return
		}
		id = row.ID
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

// handleFitDelete serves POST /fittings/delete/ (form field id).
func (app *Application) handleFitDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err == nil {
		if id, perr := strconv.ParseInt(r.FormValue("id"), 10, 64); perr == nil && id > 0 {
			if err := app.queries.DeleteLocalFitting(ctx, db.DeleteLocalFittingParams{ID: id, UserID: userID}); err != nil {
				log.Printf("fittings delete: %d: %v", id, err)
			}
		}
	}
	http.Redirect(w, r, "/fittings/#fit-editor", http.StatusSeeOther)
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
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
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
	for _, ch := range characters {
		editor.Pilots = append(editor.Pilots, assetCharLink{
			ID: ch.CharacterID, Name: ch.Name, Active: ch.CharacterID == pilotID,
		})
	}
	editor.Sim = app.buildFitSimView(ctx, doc, pilotID, pilotLabel)

	if userID > 0 {
		if rows, err := app.queries.ListLocalFittings(ctx, userID); err == nil {
			shipIDs := make([]int64, 0, len(rows))
			for _, row := range rows {
				shipIDs = append(shipIDs, row.ShipTypeID)
			}
			names := app.esi.CachedTypeNames(ctx, shipIDs)
			for _, row := range rows {
				shipName := names[row.ShipTypeID]
				if shipName == "" {
					shipName = fmt.Sprintf("Type #%d", row.ShipTypeID)
				}
				updated := row.UpdatedAt
				if t, terr := time.Parse(time.RFC3339, updated); terr == nil {
					updated = t.Format("2006-01-02 15:04")
				}
				editor.LocalFits = append(editor.LocalFits, localFitRow{
					ID: row.ID, Name: row.Name, ShipName: shipName, Updated: updated,
				})
			}
		} else {
			log.Printf("fittings: list local fits for user %d: %v", userID, err)
		}
	}
	data.FitEditor = editor
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

	snap, err := loadFitSnapshot(ctx, app.queries, fitDocTypeIDs(doc))
	if err != nil {
		return doc
	}
	var weapons []int64
	seenWeapon := map[int64]bool{}
	for _, it := range doc.Items {
		if seenWeapon[it.TypeID] {
			continue
		}
		if snap.hasEffect(it.TypeID, fitEffectTurretFitted) || snap.hasEffect(it.TypeID, fitEffectLauncherFitted) {
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
	if snap, err := loadFitSnapshot(ctx, app.queries, fitDocTypeIDs(doc)); err == nil {
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
