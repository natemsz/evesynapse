package fit

import (
	"fmt"
	"math"
	"sort"
)

// Compute runs the stat engine over one fit. charges maps a
// weapon's type ID to its loaded charge type ID. Skill levels
// are clamped to 0..5; absent skills are untrained. implants are
// the active clone's implant type IDs: they become char-located
// entities so their own effects (ItemModifier on the ship, the
// charID LocationGroupModifier that builds pirate set bonuses)
// flow through the same three-pass fold as everything else.
// Implant sources never join the stacking-penalty group,
// pirate set totals are documented unpenalized (a full
// High-grade Snake set is +24.73% velocity, which only the
// unpenalized product reproduces).
func Compute(snap *Snapshot, shipTypeID int64, items []ItemInput, levels map[int64]int, charges map[int64]int64, implants []int64) *Result {
	f := newFitting(snap, shipTypeID, levels)

	// What is being computed: the hull, the pilot, and everything
	// fitted, loaded, trained or plugged in.
	f.addItems(items)
	f.addCharges(charges)
	f.addSkills()
	f.addImplants(implants)

	f.checkShipRestrictions()

	// Every effect is bound to the attributes it modifies, and the
	// attributes are folded to their final values.
	f.bindModifiers()
	f.foldAttributes()
	f.adaptReactiveHardeners()
	f.recordAttrs()

	// The statistics are read off the folded attributes.
	f.resources()
	f.slotsAndHardpoints()
	f.capacitor()
	f.offense()
	f.fighterStats()
	f.droneStats()
	f.tank()
	f.mobilityAndTargeting()

	f.res.Unmodeled = f.unmodeledList()
	for id := range f.unmodeledBy {
		f.res.UnmodeledBy = append(f.res.UnmodeledBy, id)
	}
	sort.Slice(f.res.UnmodeledBy, func(i, j int) bool { return f.res.UnmodeledBy[i] < f.res.UnmodeledBy[j] })
	return f.res
}

// fitting is one fit while it is being computed: what Compute was
// given, the entities built from it, and the result being filled in.
// Each step of Compute is a method on it.
type fitting struct {
	snap       *Snapshot
	shipTypeID int64
	levels     map[int64]int
	res        *Result

	ship *entity // the hull
	char *entity // the character pseudo-item

	// entities is everything whose attributes get computed, in the
	// order it was added.
	entities []*entity
	// fitted is the modules, rigs, subsystems and drones.
	// fittedByType finds a type's entities: one per module state.
	fitted       []*entity
	fittedByType map[int64][]*entity
	drones       []*entity // the drones among fitted
	fighters     []*entity // the fighters among fitted
	// weaponCharge is each weapon type's loaded charge or missile.
	weaponCharge map[int64]*entity
	skills       map[int64]*entity
	implants     []*entity

	// apps is every modifier bound to a target (bindModifiers).
	apps []application

	// unmodeled collects what the engine met and does not model;
	// unmodeledBy the types whose effects those were.
	unmodeled   map[string]bool
	unmodeledBy map[int64]bool

	// spoolExtra is the DPS spool-up weapons add at full spool.
	spoolExtra float64
}

// damageAttrs are the four damage types a charge, missile or drone
// deals.
var damageAttrs = []int64{AttrDamageEM, AttrDamageExplosive, AttrDamageKinetic, AttrDamageThermal}

// newFitting starts a computation with the two entities every fit
// has: the hull and the character pseudo-item.
func newFitting(snap *Snapshot, shipTypeID int64, levels map[int64]int) *fitting {
	shipBase := cloneAttrMap(snap.Attrs[shipTypeID])
	if phys, ok := snap.Physics[shipTypeID]; ok && phys.Mass > 0 {
		if _, has := shipBase[AttrMass]; !has {
			shipBase[AttrMass] = phys.Mass
		}
	}
	ship := &entity{key: "ship", kind: entShip, typeID: shipTypeID, instances: 1, base: shipBase, calc: map[int64]float64{}}
	char := &entity{key: "char", kind: entChar, base: map[int64]float64{}, calc: map[int64]float64{}}
	return &fitting{
		snap:       snap,
		shipTypeID: shipTypeID,
		levels:     levels,
		res: &Result{
			ShipTypeID:   shipTypeID,
			ItemAttrs:    make(map[int64]map[int64]float64),
			StateAttrs:   make(map[string]map[int64]float64),
			WeaponDPS:    make(map[string]float64),
			WeaponVolley: make(map[string]float64),
		},
		ship:         ship,
		char:         char,
		entities:     []*entity{ship, char},
		fittedByType: make(map[int64][]*entity),
		weaponCharge: make(map[int64]*entity),
		skills:       make(map[int64]*entity),
		unmodeled:    make(map[string]bool),
		unmodeledBy:  make(map[int64]bool),
	}
}

// note records something the engine met and does not model.
func (f *fitting) note(s string) { f.unmodeled[s] = true }

// noteFrom is note for an effect a fitted item (or the hull, an
// implant, a charge) carries, so the gap can name what it is about.
// Skills and the character are no single item to name.
func (f *fitting) noteFrom(source *entity, s string) {
	f.note(s)
	if source != nil && source.kind != entSkill && source.kind != entChar && source.typeID != 0 {
		f.unmodeledBy[source.typeID] = true
	}
}

// shipAttr is one of the hull's attributes after the fold.
func (f *fitting) shipAttr(attr int64) float64 { return f.ship.get(f.snap, attr) }

// addItems builds the module, rig, subsystem and drone entities:
// one per fitted type, with instances counting duplicates.
// Modules fold by (type, state): two identical modules in
// different states are separate entities so each binds only
// the effect categories its state allows.
func (f *fitting) addItems(items []ItemInput) {
	snap := f.snap
	totalRackInstances := 0
	for _, it := range items {
		if it.TypeID == f.shipTypeID {
			continue
		}
		qty := it.Quantity
		kind := entModule
		switch {
		case snap.HasEffect(it.TypeID, EffectRigSlot):
			kind = entRig
		case snap.HasEffect(it.TypeID, EffectSubsystemSlot):
			kind = entSubsystem
		case snap.Attrs[it.TypeID][AttrFighterSquadronMaxSize] > 0:
			kind = entFighter
		case snap.Attrs[it.TypeID][AttrDroneBandwidthUsed] > 0:
			kind = entDrone
		}
		if qty <= 0 {
			qty = 1
			if kind == entDrone || kind == entFighter {
				qty = 0 // a drone line with no count carries nothing
			}
		}
		// Rack entities (modules, rigs, subsystems) bind their
		// modifiers once per instance, so an unbounded quantity is
		// unbounded engine work. No hull fits more than 16 of one
		// type or 64 rack items in total; anything past that is a
		// hostile or corrupt document, and the excess is dropped.
		isRack := kind == entModule || kind == entRig || kind == entSubsystem
		if isRack {
			if qty > 16 {
				qty = 16
			}
			if totalRackInstances+qty > 64 {
				qty = 64 - totalRackInstances
			}
			if qty <= 0 {
				continue
			}
			totalRackInstances += qty
		}
		state := ""
		if kind == entModule {
			state = NormalizeModuleState(snap, it.TypeID, it.State)
		}
		var ent *entity
		for _, e := range f.fittedByType[it.TypeID] {
			if e.state == state {
				ent = e
				break
			}
		}
		if ent != nil {
			ent.instances += qty
			if isRack && ent.instances > 16 {
				ent.instances = 16
			}
			continue
		}
		ent = &entity{
			key:       fmt.Sprintf("fit:%d:%s", it.TypeID, state),
			kind:      kind,
			typeID:    it.TypeID,
			instances: qty,
			state:     state,
			base:      cloneAttrMap(snap.Attrs[it.TypeID]),
			calc:      map[int64]float64{},
		}
		f.fittedByType[it.TypeID] = append(f.fittedByType[it.TypeID], ent)
		f.fitted = append(f.fitted, ent)
		f.entities = append(f.entities, ent)
		switch kind {
		case entDrone:
			f.drones = append(f.drones, ent)
		case entFighter:
			f.fighters = append(f.fighters, ent)
		}
	}
}

// addCharges builds the charge entities (missile entities for
// launchers), paired with the weapon type that carries them. A
// charge is only modeled when at least one weapon of its type is
// cycling, a loaded charge in an offline or inactive weapon
// does nothing (the doc still remembers it).
func (f *fitting) addCharges(charges map[int64]int64) {
	for weaponType, chargeType := range charges {
		ents := f.fittedByType[weaponType]
		if len(ents) == 0 || chargeType == 0 {
			continue
		}
		cycling := false
		for _, e := range ents {
			if e.cycling() {
				cycling = true
				break
			}
		}
		if !cycling {
			continue
		}
		kind := entCharge
		if f.snap.HasEffect(weaponType, EffectLauncherFitted) {
			kind = entMissile
		}
		ent := &entity{
			key:       fmt.Sprintf("charge:%d", chargeType),
			kind:      kind,
			typeID:    chargeType,
			instances: 1,
			base:      cloneAttrMap(f.snap.Attrs[chargeType]),
			calc:      map[int64]float64{},
		}
		f.weaponCharge[weaponType] = ent
		f.entities = append(f.entities, ent)
	}
}

// addSkills makes each trained skill a pseudo-item, its skillLevel
// set from the pilot's levels.
func (f *fitting) addSkills() {
	for skillID, lvl := range f.levels {
		if lvl <= 0 {
			continue
		}
		if lvl > 5 {
			lvl = 5
		}
		base := cloneAttrMap(f.snap.Attrs[skillID])
		base[AttrSkillLevel] = float64(lvl)
		ent := &entity{
			key:       fmt.Sprintf("skill:%d", skillID),
			kind:      entSkill,
			typeID:    skillID,
			instances: 1,
			base:      base,
			calc:      map[int64]float64{},
		}
		f.skills[skillID] = ent
		f.entities = append(f.entities, ent)
	}
}

// addImplants makes each implant a char-located pseudo-item (one
// entity per implant type; slots are unique so instances stays 1).
func (f *fitting) addImplants(implants []int64) {
	seen := make(map[int64]bool)
	for _, implantID := range implants {
		if implantID <= 0 || implantID == f.shipTypeID || seen[implantID] {
			continue
		}
		seen[implantID] = true
		ent := &entity{
			key:       fmt.Sprintf("implant:%d", implantID),
			kind:      entImplant,
			typeID:    implantID,
			instances: 1,
			base:      cloneAttrMap(f.snap.Attrs[implantID]),
			calc:      map[int64]float64{},
		}
		f.implants = append(f.implants, ent)
		f.entities = append(f.entities, ent)
	}
}

// checkShipRestrictions flags the fit errors: modules carrying
// canFitShipGroup*/canFitShipType*/fitsToShipType may only fly on the
// named hulls, a group may not be fitted past its maxGroupFitted, and
// a hull takes one subsystem per subsystem slot.
func (f *fitting) checkShipRestrictions() {
	shipGroup := f.snap.Groups[f.shipTypeID]
	fitted := map[int64]int{}
	allowed := map[int64]int{}
	for _, ent := range f.fitted {
		if r := restrictionFor(f.snap, f.shipTypeID, shipGroup, ent.typeID); r != nil {
			f.res.Restricted = append(f.res.Restricted, *r)
		}
		if ent.kind != entModule && ent.kind != entRig && ent.kind != entSubsystem {
			continue
		}
		g := f.snap.Groups[ent.typeID]
		fitted[g] += maxInt(ent.instances, 1)
		n := int(f.snap.Attrs[ent.typeID][AttrMaxGroupFitted])
		if ent.kind == entSubsystem && f.snap.Attrs[ent.typeID][AttrSubSystemSlot] > 0 {
			n = 1 // each subsystem slot is its own group: one per hull
		}
		if n > 0 && (allowed[g] == 0 || n < allowed[g]) {
			allowed[g] = n
		}
	}
	for g, n := range allowed {
		if fitted[g] > n {
			f.res.OverGroupLimit = append(f.res.OverGroupLimit, GroupLimit{GroupID: g, Allowed: n, Fitted: fitted[g]})
		}
	}
	sort.Slice(f.res.OverGroupLimit, func(i, j int) bool { return f.res.OverGroupLimit[i].GroupID < f.res.OverGroupLimit[j].GroupID })
}

// skillSelf stands for "the skill carrying the effect" in a
// codeEffects modifier's SkillTypeID.
const skillSelf = -1

// codeEffect is an effect the SDE carries with no modifier rows
// (the game applies it in code), written out as the modifiers it
// stands for. Effect.Category gates it like any other effect.
type codeEffect struct {
	Effect
	modifiers []Modifier
}

// codeEffects are the effects CCP ships without modifiers that
// change a fit's own numbers: missile damage by missile skill, T2
// launcher rate of fire by specialization, drone damage by drone
// skill, and the Reactive Armor Hardener's resistances. Each skill
// already scales its bonus by level in a modifier effect of its own
// (152 or 146 for damageMultiplierBonus 292, 163 for rofBonus 293),
// so the bonus applies as folded. The hardener applies its adapted
// split (adaptReactiveHardeners), stacking-penalized with the other
// armor hardeners.
var codeEffects = map[int64]codeEffect{
	660: missileSkillDamage("missileEMDmgBonus", AttrDamageEM),
	661: missileSkillDamage("missileExplosiveDmgBonus", AttrDamageExplosive),
	662: missileSkillDamage("missileThermalDmgBonus", AttrDamageThermal),
	668: missileSkillDamage("missileKineticDmgBonus2", AttrDamageKinetic),
	1730: {Effect: Effect{Name: "droneDmgBonus"}, modifiers: []Modifier{
		{Domain: "charID", Func: "OwnerRequiredSkillModifier", ModifiedAttr: AttrDamageMultiplier, ModifyingAttr: attrDamageMultiplierBonus, Operation: 6, SkillTypeID: skillSelf},
	}},
	1851: {Effect: Effect{Name: "selfRof"}, modifiers: []Modifier{
		{Domain: "shipID", Func: "LocationRequiredSkillModifier", ModifiedAttr: AttrSpeed, ModifyingAttr: attrRofBonus, Operation: 6, SkillTypeID: skillSelf},
	}},
	EffectAdaptiveArmorHardener: {Effect: Effect{Name: "adaptiveArmorHardener", Category: 1}, modifiers: []Modifier{
		{Domain: "shipID", Func: "ItemModifier", ModifiedAttr: AttrArmorEM, ModifyingAttr: AttrArmorEM, Operation: opPostMult},
		{Domain: "shipID", Func: "ItemModifier", ModifiedAttr: AttrArmorExplosive, ModifyingAttr: AttrArmorExplosive, Operation: opPostMult},
		{Domain: "shipID", Func: "ItemModifier", ModifiedAttr: AttrArmorKinetic, ModifyingAttr: AttrArmorKinetic, Operation: opPostMult},
		{Domain: "shipID", Func: "ItemModifier", ModifiedAttr: AttrArmorThermal, ModifyingAttr: AttrArmorThermal, Operation: opPostMult},
	}},
}

const (
	attrDamageMultiplierBonus = 292
	attrRofBonus              = 293
	attrResistanceShift       = 1849
)

func missileSkillDamage(name string, attr int64) codeEffect {
	return codeEffect{Effect: Effect{Name: name}, modifiers: []Modifier{
		{Domain: "charID", Func: "OwnerRequiredSkillModifier", ModifiedAttr: attr, ModifyingAttr: attrDamageMultiplierBonus, Operation: 6, SkillTypeID: skillSelf},
	}}
}

// bindModifiers walks every entity's effects and binds each
// modifier to the entities it applies to, filling f.apps.
func (f *fitting) bindModifiers() {
	snap := f.snap

	// atShip are the entities sitting at the ship's location for
	// Location* selectors: fitted equipment plus launched-craft
	// types (drones, missiles).
	atShip := make([]*entity, 0, len(f.fitted)+len(f.weaponCharge))
	atShip = append(atShip, f.fitted...)
	for _, ent := range f.weaponCharge {
		atShip = append(atShip, ent)
	}

	// Sources in a fixed order (ship, char, skills by type,
	// fitted items, charges) so PreAssign outcomes are stable.
	sources := make([]*entity, 0, len(f.entities))
	sources = append(sources, f.ship)
	skillIDs := make([]int64, 0, len(f.skills))
	for id := range f.skills {
		skillIDs = append(skillIDs, id)
	}
	sort.Slice(skillIDs, func(i, j int) bool { return skillIDs[i] < skillIDs[j] })
	for _, id := range skillIDs {
		sources = append(sources, f.skills[id])
	}
	sources = append(sources, f.implants...)
	sources = append(sources, f.fitted...)
	for _, ent := range f.weaponCharge {
		sources = append(sources, ent)
	}

	for _, source := range sources {
		// Effect categories bind per module state: an
		// offline module binds nothing, an
		// online-but-inactive one skips active and overload,
		// overheated adds the overload category.
		allowed := sourceCategories(source)
		if allowed == nil {
			continue
		}
		for _, effectID := range snap.TypeEffects[source.typeID] {
			eff := snap.Effects[effectID]
			if eff == nil || len(eff.Modifiers) == 0 {
				if code, ok := codeEffects[effectID]; ok {
					if !allowed[code.Category] {
						continue
					}
					for _, m := range code.modifiers {
						if m.SkillTypeID == skillSelf {
							m.SkillTypeID = source.typeID
						}
						f.bind(atShip, source, effectID, &code.Effect, m)
					}
					continue
				}
			}
			if eff == nil || len(eff.Modifiers) == 0 {
				continue // carries no modifiers; nothing to apply
			}
			if !allowed[eff.Category] {
				// A heatable module that simply isn't heated is
				// a fitting choice, not a modeling gap, skip
				// silently so the heat note only fires for
				// genuinely unmodeled categories.
				if eff.Category == catOverload && source.kind == entModule {
					continue
				}
				f.noteFrom(source, fmt.Sprintf("effect %d %s: category %d skipped (overload/system/target mechanics not modeled)", effectID, eff.Name, eff.Category))
				continue
			}
			for _, m := range eff.Modifiers {
				// Instances of the same fitted type bind once per
				// instance so stacking penalties see each copy.
				n := 1
				if source.kind == entModule || source.kind == entRig || source.kind == entSubsystem {
					n = source.instances
					if n < 1 {
						n = 1
					}
				}
				for i := 0; i < n; i++ {
					f.bind(atShip, source, effectID, eff, m)
				}
			}
		}
	}
}

// bind resolves one modifier's selector (its func and domain) to
// the entities it reaches and records an application for each.
// atShip is what sits at the ship's location (see bindModifiers).
func (f *fitting) bind(atShip []*entity, source *entity, effectID int64, eff *Effect, m Modifier) {
	if m.ModifiedAttr == AttrSkillLevel {
		return // skill levels are engine inputs
	}
	if m.Domain == "structureID" {
		return // structure skills and bonuses never reach a ship
	}
	snap := f.snap
	targets := func(list ...*entity) {
		for _, t := range list {
			if t != nil {
				f.apps = append(f.apps, application{source: source, target: t, effectID: effectID, mod: m})
			}
		}
	}
	shape := fmt.Sprintf("%s/%s (effect %d %s)", m.Func, m.Domain, effectID, eff.Name)
	switch m.Func {
	case "ItemModifier":
		switch m.Domain {
		case "shipID":
			targets(f.ship)
		case "charID":
			targets(f.char)
		case "itemID":
			targets(source)
		case "otherID":
			// A charge modifier reaching its host weapon.
			if source.kind == entCharge || source.kind == entMissile {
				for weaponType, ent := range f.weaponCharge {
					if ent == source {
						targets(f.fittedByType[weaponType]...)
					}
				}
			} else {
				f.noteFrom(source, shape+": no charge host")
			}
		default:
			f.noteFrom(source, shape+": domain not modeled")
		}
	case "LocationModifier":
		switch m.Domain {
		case "shipID":
			for _, t := range atShip {
				switch t.kind {
				case entModule, entRig, entSubsystem:
					targets(t)
				}
			}
		case "charID":
			// Attribute implants and similar char-located
			// effects land on the character pseudo-item.
			// Nothing downstream reads its stats, so
			// this is modeling completeness, not behavior.
			targets(f.char)
		default:
			f.noteFrom(source, shape+": domain not modeled")
			return
		}
	case "LocationGroupModifier":
		switch m.Domain {
		case "shipID":
			for _, t := range atShip {
				if t.kind == entCharge {
					continue // contained in its weapon, not at the ship
				}
				if snap.Groups[t.typeID] == m.GroupID && m.GroupID != 0 {
					targets(t)
				}
			}
		case "charID":
			// Pirate implant set bonuses: each set implant
			// carries an effect like setBonusSerpentis that
			// pre-multiplies the set's bonus attribute on
			// every implant of the set group plugged into
			// the character (SDE: implantSet* attributes,
			// e.g. 802 implantSetSerpentis -> 315
			// velocityBonus, operator 0 = pre-multiply).
			for _, t := range f.implants {
				if snap.Groups[t.typeID] == m.GroupID && m.GroupID != 0 {
					targets(t)
				}
			}
		default:
			f.noteFrom(source, shape+": domain not modeled")
			return
		}
	case "LocationRequiredSkillModifier":
		if m.Domain != "shipID" {
			f.noteFrom(source, shape+": domain not modeled")
			return
		}
		for _, t := range atShip {
			if t.kind == entCharge {
				continue
			}
			if m.SkillTypeID != 0 && snap.requiresSkill(t.typeID, m.SkillTypeID) {
				targets(t)
			}
		}
	case "OwnerRequiredSkillModifier":
		if m.Domain != "charID" {
			f.noteFrom(source, shape+": domain not modeled")
			return
		}
		// The character owns its drones, fighters and every
		// loaded charge and missile: this is how hull and skill bonuses reach
		// missile damage (Drake, Naga Navy Issue, Heavy Missiles).
		for _, t := range f.entities {
			if t.kind != entDrone && t.kind != entFighter && t.kind != entCharge && t.kind != entMissile {
				continue
			}
			if m.SkillTypeID != 0 && snap.requiresSkill(t.typeID, m.SkillTypeID) {
				targets(t)
			}
		}
	default:
		f.noteFrom(source, shape+": modifier func not modeled")
	}
}

// foldAttributes groups the applications per (target, attribute)
// and folds every modified attribute from base, each source value
// resolved before the attribute that reads it.
func (f *fitting) foldAttributes() {
	type appKey struct {
		target *entity
		attr   int64
	}
	grouped := make(map[appKey][]application)
	for _, app := range f.apps {
		k := appKey{app.target, app.mod.ModifiedAttr}
		grouped[k] = append(grouped[k], app)
	}
	// Each modified attribute is resolved on demand: a modifier's
	// source value is folded before the attribute it feeds, however
	// deep the chain (a synchronizer rig's module count feeds its
	// bonus, which feeds a hidden turret multiplier, which feeds
	// the turret's damage). A cycle reads the value as it stands.
	const (
		pending = 1
		done    = 2
	)
	state := make(map[appKey]int, len(grouped))
	var resolve func(e *entity, attr int64) float64
	resolve = func(e *entity, attr int64) float64 {
		k := appKey{e, attr}
		list, ok := grouped[k]
		if !ok || state[k] != 0 {
			return e.get(f.snap, attr)
		}
		state[k] = pending
		base, ok := e.base[attr]
		if !ok {
			base = f.snap.attrDefault(attr)
		}
		e.calc[attr] = foldAttribute(f.snap, base, list, resolve)
		state[k] = done
		return e.calc[attr]
	}
	for _, app := range f.apps {
		resolve(app.target, app.mod.ModifiedAttr)
	}
}

// recordAttrs copies the folded attributes into the result: the
// hull's, each fitted type's, and each module's per state.
func (f *fitting) recordAttrs() {
	res := f.res
	res.ShipAttrs = finalAttrs(f.snap, f.ship)
	for _, ent := range f.entities {
		if ent == f.ship || ent.kind == entSkill || ent == f.char {
			continue
		}
		fa := finalAttrs(f.snap, ent)
		if _, ok := res.ItemAttrs[ent.typeID]; !ok {
			res.ItemAttrs[ent.typeID] = fa
		}
		if ent.kind == entModule && ent.state != "" {
			res.StateAttrs[StateAttrKey(ent.typeID, ent.state)] = fa
		}
	}
}

// resources totals powergrid and CPU. Offline modules are fitted
// but dark: no PG, no CPU, no effects.
func (f *fitting) resources() {
	snap, res := f.snap, f.res
	for _, ent := range f.fitted {
		if ent.kind == entDrone || !ent.online() {
			continue
		}
		res.PowergridUsed += ent.get(snap, AttrPower) * float64(maxInt(ent.instances, 1))
		res.CPUUsed += ent.get(snap, AttrCPU) * float64(maxInt(ent.instances, 1))
	}
	res.PowergridMax = f.shipAttr(AttrPowerOutput)
	res.CPUMax = f.shipAttr(AttrCPUOutput)
}

// slotsAndHardpoints counts what the hull offers and what the fit
// uses: slots by rack, turret and launcher hardpoints, calibration.
func (f *fitting) slotsAndHardpoints() {
	snap, res := f.snap, f.res
	res.HighSlots = int(f.shipAttr(AttrHiSlots))
	res.MediumSlots = int(f.shipAttr(AttrMedSlots))
	res.LowSlots = int(f.shipAttr(AttrLowSlots))
	res.RigSlots = int(f.shipAttr(AttrRigSlots))
	res.TurretHardpoints = int(f.shipAttr(AttrTurretSlotsLeft))
	res.LauncherHardpoints = int(f.shipAttr(AttrLauncherSlotsLeft))
	// T3 strategic cruisers: the hull has no slots of its own;
	// fitted subsystems grant them through the *SlotModifier
	// attributes above (plain data, summed directly).
	for _, ent := range f.fitted {
		if ent.kind != entSubsystem {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		res.HighSlots += int(ent.base[AttrHiSlotModifier] * n)
		res.MediumSlots += int(ent.base[AttrMedSlotModifier] * n)
		res.LowSlots += int(ent.base[AttrLowSlotModifier] * n)
		res.TurretHardpoints += int(ent.base[AttrTurretHardPointModifier] * n)
		res.LauncherHardpoints += int(ent.base[AttrLauncherHardPointModifier] * n)
	}
	// Slot counts are bounded by the game's own geometry: no
	// hull has more than 8 slots of a rack type. Clamp what the
	// subsystem sum produced so a corrupt count can never make
	// a fit claim tens of thousands of slots.
	clampSlots := func(v int) int {
		if v < 0 {
			return 0
		}
		if v > 8 {
			return 8
		}
		return v
	}
	res.HighSlots = clampSlots(res.HighSlots)
	res.MediumSlots = clampSlots(res.MediumSlots)
	res.LowSlots = clampSlots(res.LowSlots)
	res.RigSlots = clampSlots(res.RigSlots)
	for _, ent := range f.fitted {
		n := maxInt(ent.instances, 1)
		switch {
		case ent.kind == entRig:
			res.RigSlotsUsed += n
			res.CalibrationUsed += ent.get(snap, AttrUpgradeCost) * float64(n)
		case ent.kind == entModule:
			switch {
			case snap.HasEffect(ent.typeID, EffectHiPower):
				res.HighSlotsUsed += n
			case snap.HasEffect(ent.typeID, EffectMedPower):
				res.MediumSlotsUsed += n
			case snap.HasEffect(ent.typeID, EffectLoPower):
				res.LowSlotsUsed += n
			}
			if snap.HasEffect(ent.typeID, EffectTurretFitted) {
				res.TurretHardpointsUsed += n
			}
			if snap.HasEffect(ent.typeID, EffectLauncherFitted) {
				res.LauncherHardpointsUsed += n
			}
		}
	}
	res.CalibrationMax = f.shipAttr(AttrUpgradeCapacity)
}

// capacitor works out capacity, peak recharge, what the cycling
// modules draw, and whether the capacitor holds (and where) or
// how long it lasts.
func (f *fitting) capacitor() {
	snap, res := f.snap, f.res
	res.CapacitorCapacity = f.shipAttr(AttrCapacitorCapacity)
	capRechargeSecs := f.shipAttr(AttrCapacitorRecharge) / 1000
	if res.CapacitorCapacity > 0 && capRechargeSecs > 0 {
		res.CapacitorPeakRecharge = 2.5 * res.CapacitorCapacity / capRechargeSecs
	}
	for _, ent := range f.fitted {
		if ent.kind == entDrone || !ent.cycling() {
			continue
		}
		need := ent.get(snap, AttrCapacitorNeed)
		cycle := cycleSeconds(ent, snap)
		if need > 0 && cycle > 0 {
			res.CapacitorDraw += need / cycle * float64(maxInt(ent.instances, 1))
		}
	}
	if res.CapacitorCapacity > 0 && res.CapacitorDraw > 0 {
		if res.CapacitorDraw <= res.CapacitorPeakRecharge {
			res.CapacitorStable = true
			res.CapacitorStablePercent = capacitorEquilibriumPercent(
				res.CapacitorCapacity, capRechargeSecs, res.CapacitorDraw)
		} else {
			res.CapacitorDepletionSecs = capacitorDepletionSeconds(
				res.CapacitorCapacity, capRechargeSecs, res.CapacitorDraw)
		}
	} else {
		res.CapacitorStable = true
		res.CapacitorStablePercent = 100
	}
}

// offense totals what the cycling modules do to a target: turret
// and missile damage, and capacitor warfare.
func (f *fitting) offense() {
	snap, res := f.snap, f.res
	for _, ent := range f.fitted {
		if ent.kind != entModule {
			continue
		}
		// Offline or inactive modules neither deal damage nor
		// run cap warfare.
		if !ent.cycling() {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		cycle := cycleSeconds(ent, snap)
		// Capacitor warfare drains even where damage doesn't
		// apply, so count it before the cycle gate.
		if amt := ent.get(snap, AttrEnergyNeutralizerAmount); amt > 0 {
			res.NeutDrainPerCycle += n * amt
			if cycle > 0 {
				res.NeutDrainPerSec += n * amt / cycle
			}
		}
		if amt := ent.get(snap, AttrPowerTransferAmount); amt > 0 &&
			snap.Groups[ent.typeID] == groupNosferatu {
			res.NosDrainPerCycle += n * amt
			if cycle > 0 {
				res.NosDrainPerSec += n * amt / cycle
			}
		}
		if cycle <= 0 {
			continue
		}
		charge := f.weaponCharge[ent.typeID]
		switch {
		case snap.HasEffect(ent.typeID, EffectTurretFitted):
			if charge == nil {
				f.note(fmt.Sprintf("turret type %d has no charge selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= ent.get(snap, AttrDamageMultiplier)
			res.TurretVolley += n * volley
			res.TurretDPS += n * volley / cycle
			if maxBonus, step := ent.get(snap, AttrSpoolBonusMax), ent.get(snap, AttrSpoolBonusPerCycle); maxBonus > 0 && step > 0 {
				f.spoolExtra += n * volley * maxBonus / cycle
				res.SpoolSeconds = math.Max(res.SpoolSeconds, math.Ceil(maxBonus/step-1e-9)*cycle)
			}
			res.WeaponVolley[StateAttrKey(ent.typeID, ent.state)] = volley
			res.WeaponDPS[StateAttrKey(ent.typeID, ent.state)] = volley / cycle
		case charge != nil && charge.get(snap, AttrDotMaxDamagePerTick) > 0:
			// Breacher pods: one damage-over-time effect per
			// target, a tick a second, the lower of a flat cap and
			// a share of the target's hit points. It depends on the
			// target, so it stays out of the DPS totals.
			res.BreacherMaxPerSec = math.Max(res.BreacherMaxPerSec, charge.get(snap, AttrDotMaxDamagePerTick))
			res.BreacherHPPercent = math.Max(res.BreacherHPPercent, charge.get(snap, AttrDotMaxHPPercentPerTick))
			res.BreacherSeconds = math.Max(res.BreacherSeconds, charge.get(snap, AttrDotDuration)/1000)
		case snap.HasEffect(ent.typeID, EffectLauncherFitted):
			if charge == nil {
				f.note(fmt.Sprintf("launcher type %d has no missile selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= charge.get(snap, AttrDamageMultiplier)
			volley *= f.char.get(snap, AttrMissileDamageMult)
			res.MissileVolley += n * volley
			res.MissileDPS += n * volley / cycle
			res.WeaponVolley[StateAttrKey(ent.typeID, ent.state)] = volley
			res.WeaponDPS[StateAttrKey(ent.typeID, ent.state)] = volley / cycle
		case snap.Groups[ent.typeID] == groupSmartbomb:
			// A smartbomb's damage rides on the module itself;
			// there is no charge and no damage multiplier.
			volley := 0.0
			for _, a := range damageAttrs {
				volley += ent.get(snap, a)
			}
			if volley <= 0 {
				continue
			}
			res.SmartbombVolley += n * volley
			res.SmartbombDPS += n * volley / cycle
			res.WeaponVolley[StateAttrKey(ent.typeID, ent.state)] = volley
			res.WeaponDPS[StateAttrKey(ent.typeID, ent.state)] = volley / cycle
		}
	}
}

// droneStats counts the drones carried, then the ones that can fly
// (limited by the ship's bandwidth and the pilot's drone control
// count) and their damage. It runs after offense: the fit's total
// DPS and volley are summed here, once every source is in.
func (f *fitting) droneStats() {
	snap, res := f.snap, f.res
	res.DroneBandwidth = f.shipAttr(AttrDroneBandwidth)
	res.DroneBayCapacity = f.shipAttr(AttrDroneCapacity)
	maxActive := f.levels[AttrDronesSkill]
	var activeBudget = maxActive
	for _, ent := range f.drones {
		carried := ent.instances
		res.DronesFitted += carried
		if phys, ok := snap.Physics[ent.typeID]; ok {
			res.DroneBayUsed += phys.Volume * float64(carried)
		}
		perDroneBW := ent.get(snap, AttrDroneBandwidthUsed)
		fly := carried
		if perDroneBW > 0 && res.DroneBandwidth > 0 {
			if maxByBW := int(res.DroneBandwidth / perDroneBW); fly > maxByBW {
				fly = maxByBW
			}
		}
		if fly > activeBudget {
			fly = activeBudget
		}
		if fly < 0 {
			fly = 0
		}
		activeBudget -= fly
		res.DronesActive += fly
		res.DroneBandwidthUsed += perDroneBW * float64(fly)
		cycle := cycleSeconds(ent, snap)
		if fly > 0 && cycle > 0 {
			volley := 0.0
			for _, a := range damageAttrs {
				volley += ent.get(snap, a)
			}
			volley *= ent.get(snap, AttrDamageMultiplier)
			res.DroneVolley += float64(fly) * volley
			res.DroneDPS += float64(fly) * volley / cycle
		}
	}
	res.DPS = res.TurretDPS + res.MissileDPS + res.DroneDPS + res.SmartbombDPS + res.FighterDPS
	if f.spoolExtra > 0 {
		res.SpooledDPS = res.DPS + f.spoolExtra
	}
	res.Volley = res.TurretVolley + res.MissileVolley + res.DroneVolley + res.SmartbombVolley + res.FighterVolley
}

// fighterStats launches the hangar's fighters, a squadron per tube
// within the hull's light, support and heavy slots, in the order
// they were added, and totals the launched squadrons' sustained
// attack and their missile salvo.
func (f *fitting) fighterStats() {
	snap, res := f.snap, f.res
	res.FighterTubes = int(f.shipAttr(AttrFighterTubes))
	res.FighterLightSlots = int(f.shipAttr(AttrFighterLightSlots))
	res.FighterSupportSlots = int(f.shipAttr(AttrFighterSupportSlots))
	res.FighterHeavySlots = int(f.shipAttr(AttrFighterHeavySlots))
	res.FighterHangarCapacity = f.shipAttr(AttrFighterCapacity)
	tubes := res.FighterTubes
	for _, ent := range f.fighters {
		carried := ent.instances
		res.FightersCarried += carried
		if phys, ok := snap.Physics[ent.typeID]; ok {
			res.FighterHangarUsed += phys.Volume * float64(carried)
		}
		size := int(ent.get(snap, AttrFighterSquadronMaxSize))
		if size < 1 {
			size = 1
		}
		var used *int
		var slots int
		switch {
		case ent.base[AttrFighterSquadronIsHeavy] > 0:
			used, slots = &res.FighterHeavyUsed, res.FighterHeavySlots
		case ent.base[AttrFighterSquadronIsSupport] > 0:
			used, slots = &res.FighterSupportUsed, res.FighterSupportSlots
		default:
			used, slots = &res.FighterLightUsed, res.FighterLightSlots
		}
		attackVolley, attackCycle := fighterAbility(ent, snap, fighterAttackMissileDamage, AttrFighterAttackMissileMult, AttrFighterAttackMissileDuration)
		if attackVolley == 0 {
			attackVolley, attackCycle = fighterAbility(ent, snap, fighterAttackTurretDamage, AttrFighterAttackTurretMult, AttrFighterAttackTurretDuration)
		}
		salvo, salvoCycle := fighterAbility(ent, snap, fighterMissilesDamage, AttrFighterMissilesMult, AttrFighterMissilesDuration)
		for left := carried; left > 0 && tubes > 0 && *used < slots; left -= size {
			members := float64(min(size, left))
			tubes--
			*used++
			res.SquadronsLaunched++
			if attackCycle > 0 {
				res.FighterVolley += members * attackVolley
				res.FighterDPS += members * attackVolley / attackCycle
			}
			if salvoCycle > 0 && salvo > 0 {
				res.FighterSalvo += members * salvo
				res.FighterSalvoSeconds = math.Max(res.FighterSalvoSeconds, salvoCycle)
			}
		}
	}
}

// fighterAbility is one fighter's damage per activation of an
// ability and the ability's cycle in seconds.
func fighterAbility(ent *entity, snap *Snapshot, damage []int64, mult, duration int64) (volley, cycle float64) {
	for _, a := range damage {
		volley += ent.get(snap, a)
	}
	return volley * ent.get(snap, mult), ent.get(snap, duration) / 1000
}

// tank works out hit points and effective hit points per layer,
// passive shield regeneration, and what the cycling repair modules
// restore.
func (f *fitting) tank() {
	snap, res := f.snap, f.res
	res.ShieldHP = f.shipAttr(AttrShieldCapacity)
	res.ArmorHP = f.shipAttr(AttrArmorHP)
	res.HullHP = f.shipAttr(AttrHP)
	res.ShieldEHP = layerEHP(res.ShieldHP,
		f.shipAttr(AttrShieldEM), f.shipAttr(AttrShieldThermal), f.shipAttr(AttrShieldKinetic), f.shipAttr(AttrShieldExplosive))
	res.ArmorEHP = layerEHP(res.ArmorHP,
		f.shipAttr(AttrArmorEM), f.shipAttr(AttrArmorThermal), f.shipAttr(AttrArmorKinetic), f.shipAttr(AttrArmorExplosive))
	res.HullEHP = layerEHP(res.HullHP,
		f.shipAttr(AttrResonanceEM), f.shipAttr(AttrResonanceThermal), f.shipAttr(AttrResonanceKinetic), f.shipAttr(AttrResonanceExplosive))
	res.EHP = EHPProfile{
		Omni:      res.ShieldEHP.Omni + res.ArmorEHP.Omni + res.HullEHP.Omni,
		EM:        res.ShieldEHP.EM + res.ArmorEHP.EM + res.HullEHP.EM,
		Thermal:   res.ShieldEHP.Thermal + res.ArmorEHP.Thermal + res.HullEHP.Thermal,
		Kinetic:   res.ShieldEHP.Kinetic + res.ArmorEHP.Kinetic + res.HullEHP.Kinetic,
		Explosive: res.ShieldEHP.Explosive + res.ArmorEHP.Explosive + res.HullEHP.Explosive,
	}
	shieldRechargeSecs := f.shipAttr(AttrShieldRechargeRate) / 1000
	if res.ShieldHP > 0 && shieldRechargeSecs > 0 {
		res.ShieldRegenPeak = 2.5 * res.ShieldHP / shieldRechargeSecs
	}
	for _, ent := range f.fitted {
		if ent.kind != entModule || ent.instances < 1 || !ent.cycling() {
			continue
		}
		cycle := cycleSeconds(ent, snap)
		if cycle <= 0 {
			continue
		}
		n := float64(ent.instances)
		if amt := ent.get(snap, AttrShieldBonus); amt > 0 {
			res.ShieldBoostRate += n * amt / cycle
		}
		if amt := ent.get(snap, AttrArmorDamageAmount); amt > 0 {
			if mult := ent.get(snap, AttrChargedArmorDamageMult); mult > 0 && f.weaponCharge[ent.typeID] != nil {
				amt *= mult // paste loaded
			}
			res.ArmorRepairRate += n * amt / cycle
		}
		if amt := ent.get(snap, AttrStructureDamageAmt); amt > 0 {
			res.HullRepairRate += n * amt / cycle
		}
	}
}

// mobilityAndTargeting reads off how the ship flies and locks:
// mass, speed (with any cycling propulsion module), align time,
// signature, sensors, and the cargo hold.
func (f *fitting) mobilityAndTargeting() {
	snap, res := f.snap, f.res
	res.Mass = f.shipAttr(AttrMass)
	res.Velocity = f.shipAttr(AttrMaxVelocity)
	for _, ent := range f.fitted {
		if ent.kind != entModule || !ent.cycling() {
			continue
		}
		thrust := ent.get(snap, AttrSpeedBoostFactor)
		factor := ent.get(snap, AttrSpeedFactor)
		if thrust > 0 && factor != 0 && res.Mass > 0 {
			res.Velocity *= 1 + (factor/100)*(thrust/res.Mass)
		}
	}
	if agility := f.shipAttr(AttrAgility); res.Mass > 0 && agility > 0 {
		res.AlignSeconds = res.Mass * agility * math.Log(4) / 1e6
	}
	res.SignatureRadius = f.shipAttr(AttrSignatureRadius)
	res.MaxTargetRange = f.shipAttr(AttrMaxTargetRange)
	res.ScanResolution = f.shipAttr(AttrScanResolution)
	res.SensorStrength = math.Max(math.Max(f.shipAttr(AttrScanRadar), f.shipAttr(AttrScanLadar)),
		math.Max(f.shipAttr(AttrScanMagnetometric), f.shipAttr(AttrScanGravimetric)))
	res.MaxLockedTargets = f.shipAttr(AttrMaxLockedTargets)
	res.CargoCapacity = f.shipAttr(AttrCapacity)
	if res.CargoCapacity == 0 {
		if phys, ok := snap.Physics[f.shipTypeID]; ok {
			res.CargoCapacity = phys.Capacity
		}
	}
}

// unmodeledList is everything noted as unmodeled, sorted.
func (f *fitting) unmodeledList() []string {
	out := make([]string, 0, len(f.unmodeled))
	for s := range f.unmodeled {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
