package fit

import (
	"context"
	"sort"
	"strings"
	"sync"

	db "evesynapse/internal/db/sqlc"
)

// Command bursts: a running burst module carries up to four buffs,
// each a buff id and a value (its charge sets both; skills, the
// hull and a mindlink raise the value). The game's buff tables
// (sde_dbuffs) say what each buff id changes and how. Every ship in
// the fleet takes the strongest value of each buff on grid,
// including the booster itself.

// groupCommandBurst is the command burst modules' group.
const groupCommandBurst = 1770

// burstBuffAttrs are the module's (buff id, buff value) attribute
// pairs.
var burstBuffAttrs = [4][2]int64{{2468, 2469}, {2470, 2471}, {2472, 2473}, {2536, 2537}}

// Buff is one buff collection: what a burst's buff id changes.
type Buff struct {
	ID        int64
	Name      string
	Minimum   bool // several values: the lowest holds, else the highest
	Operation int64
	Mods      []BuffMod
}

// BuffMod is one attribute a buff changes: on the ship ("item"), on
// everything fitted ("location"), on fitted modules of a group
// ("group") or requiring a skill ("skill").
type BuffMod struct {
	Kind  string
	Attr  int64
	Group int64
	Skill int64
}

// BuffValue is one buff's strength from one burst.
type BuffValue struct {
	ID    int64
	Value float64
}

// AppliedBuff is a buff acting on the fit: its name, the value that
// holds, and whether a fleet booster (not the fit's own burst)
// supplies it.
type AppliedBuff struct {
	ID    int64
	Name  string
	Value float64
	Fleet bool
}

// BurstData is what bursts need from the SDE, cached per process:
// the buff tables, the burst modules and charges, and the hulls and
// mindlinks that raise burst strength.
type BurstData struct {
	Buffs map[int64]*Buff
	// Charges are the burst charges by name; ModuleFor maps a charge
	// group to the Tech II burst module that loads it.
	Charges   []BurstCharge
	ModuleFor map[int64]int64
	// Boosters are hulls (category 6) and mindlinks (category 20)
	// raising burst strength, each with the burst skill it bonuses.
	Boosters []BurstBooster
}

// BurstCharge is one burst charge and the family it belongs to.
type BurstCharge struct {
	TypeID int64
	Name   string
	Group  int64
	Skill  int64  // the burst family's skill (Armored Command and so on)
	Family string // "Armor", "Shield", "Skirmish", "Information", ...
}

// BurstBooster is a hull or implant raising one family's bursts.
type BurstBooster struct {
	TypeID   int64
	Name     string
	Category int64
	Skill    int64
}

var (
	burstMu    sync.Mutex
	burstCache *BurstData
)

// ResetBurstData drops the cached burst data; the SDE import calls it.
func ResetBurstData() {
	burstMu.Lock()
	burstCache = nil
	burstMu.Unlock()
}

// Bursts returns the cached burst data, loading it once.
func Bursts(ctx context.Context, q *db.Queries) (*BurstData, error) {
	burstMu.Lock()
	defer burstMu.Unlock()
	if burstCache != nil {
		return burstCache, nil
	}
	d := &BurstData{Buffs: map[int64]*Buff{}, ModuleFor: map[int64]int64{}}
	rows, err := q.ListSDEDbuffs(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		d.Buffs[r.DbuffID] = &Buff{ID: r.DbuffID, Name: r.DisplayName, Minimum: r.Aggregate == "Minimum", Operation: buffOperation(r.Operation)}
	}
	mods, err := q.ListSDEDbuffModifiers(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		if b := d.Buffs[m.DbuffID]; b != nil {
			b.Mods = append(b.Mods, BuffMod{Kind: m.Kind, Attr: m.AttributeID, Group: m.GroupID, Skill: m.SkillID})
		}
	}
	types, err := q.ListBurstTypes(ctx)
	if err != nil {
		return nil, err
	}
	familyOf := map[int64]string{} // charge group -> family
	for _, t := range types {
		if t.GroupID == groupCommandBurst {
			if _, ok := d.ModuleFor[t.ChargeGroup]; !ok || strings.HasSuffix(t.Name, " II") {
				d.ModuleFor[t.ChargeGroup] = t.TypeID
				familyOf[t.ChargeGroup] = strings.TrimSpace(strings.SplitN(t.Name, " Command Burst", 2)[0])
				if i := strings.Index(t.Name, " Foreman Burst"); i > 0 {
					familyOf[t.ChargeGroup] = t.Name[:i]
				}
			}
		}
	}
	for _, t := range types {
		if t.GroupID == groupCommandBurst {
			continue
		}
		d.Charges = append(d.Charges, BurstCharge{TypeID: t.TypeID, Name: t.Name, Group: t.GroupID, Skill: t.RequiredSkill, Family: familyOf[t.GroupID]})
	}
	sort.SliceStable(d.Charges, func(i, j int) bool {
		if d.Charges[i].Family != d.Charges[j].Family {
			return d.Charges[i].Family < d.Charges[j].Family
		}
		return d.Charges[i].Name < d.Charges[j].Name
	})
	boosters, err := q.ListBurstBoosters(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range boosters {
		d.Boosters = append(d.Boosters, BurstBooster{TypeID: b.TypeID, Name: b.Name, Category: b.CategoryID, Skill: b.SkillTypeID})
	}
	burstCache = d
	return d, nil
}

// buffOperation maps a buff's operation name to the engine's
// operation code.
func buffOperation(name string) int64 {
	switch name {
	case "PreAssignment":
		return opPreAssign
	case "PreMul":
		return opPreMultiply
	case "ModAdd":
		return opModAdd
	case "PostMul":
		return opPostMult
	case "PostDiv":
		return opPostDivide
	case "PostPercent":
		return opPostPercent
	case "PostAssignment":
		return opPostAssign
	}
	return opPostPercent
}

// BoosterBuffs is what one fleet booster's burst gives: the booster
// hull (0 for one without a burst bonus) running the Tech II burst
// that loads charge, with the given skill levels and implants.
func BoosterBuffs(snap *Snapshot, hull, charge int64, levels map[int64]int, implants []int64) []BuffValue {
	if snap.Bursts == nil {
		return nil
	}
	group := snap.Groups[charge]
	module := snap.Bursts.ModuleFor[group]
	if module == 0 {
		return nil
	}
	res := Compute(snap, hull, []ItemInput{{TypeID: module, Quantity: 1}}, levels, map[int64]int64{module: charge}, implants)
	attrs := res.ItemAttrs[module]
	var out []BuffValue
	for _, pair := range burstBuffAttrs {
		if id := int64(attrs[pair[0]]); id > 0 && attrs[pair[1]] != 0 {
			out = append(out, BuffValue{ID: id, Value: attrs[pair[1]]})
		}
	}
	return out
}

// applyBuffs applies the buffs acting on the fit, the fit's own
// running bursts and the fleet's, the strongest value of each, then
// folds again. Buffs bind like a fitted module's effects, so they
// stack-penalize with modules changing the same attribute.
func (f *fitting) applyBuffs(fleet []BuffValue) {
	snap := f.snap
	if snap.Bursts == nil || len(snap.Bursts.Buffs) == 0 {
		return
	}
	type source struct {
		value float64
		fleet bool
	}
	byID := map[int64][]source{}
	for _, ent := range f.fitted {
		if ent.kind != entModule || !ent.cycling() || snap.Groups[ent.typeID] != groupCommandBurst || f.weaponCharge[ent.typeID] == nil {
			continue
		}
		for _, pair := range burstBuffAttrs {
			if id := int64(ent.get(snap, pair[0])); id > 0 {
				if v := ent.get(snap, pair[1]); v != 0 {
					byID[id] = append(byID[id], source{value: v})
				}
			}
		}
	}
	for _, b := range fleet {
		byID[b.ID] = append(byID[b.ID], source{value: b.Value, fleet: true})
	}
	if len(byID) == 0 {
		return
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	atShip := make([]*entity, 0, len(f.fitted)+len(f.weaponCharge))
	atShip = append(atShip, f.fitted...)
	for _, ent := range f.weaponCharge {
		atShip = append(atShip, ent)
	}
	bound := false
	for _, id := range ids {
		buff := snap.Bursts.Buffs[id]
		if buff == nil || len(buff.Mods) == 0 {
			continue
		}
		best := byID[id][0]
		for _, s := range byID[id][1:] {
			if (buff.Minimum && s.value < best.value) || (!buff.Minimum && s.value > best.value) {
				best = s
			}
		}
		src := &entity{key: "buff", kind: entModule, instances: 1, base: map[int64]float64{attrBuffValue: best.value}, calc: map[int64]float64{}}
		eff := &Effect{Name: "buff " + buff.Name}
		for _, m := range buff.Mods {
			mod := Modifier{Domain: "shipID", ModifiedAttr: m.Attr, ModifyingAttr: attrBuffValue, Operation: buff.Operation, GroupID: m.Group, SkillTypeID: m.Skill}
			switch m.Kind {
			case "item":
				mod.Func = "ItemModifier"
			case "location":
				mod.Func = "LocationModifier"
			case "group":
				mod.Func = "LocationGroupModifier"
			case "skill":
				mod.Func = "LocationRequiredSkillModifier"
			default:
				continue
			}
			f.bind(atShip, src, 0, eff, mod)
			bound = true
		}
		f.res.Buffs = append(f.res.Buffs, AppliedBuff{ID: id, Name: buff.Name, Value: best.value, Fleet: best.fleet})
	}
	if bound {
		f.resetCalc()
		f.foldAttributes()
	}
}

// attrBuffValue is where a buff's synthetic source holds its value.
const attrBuffValue = 2469

// WithBursts attaches the cached burst data to a snapshot, so the
// fit's bursts and fleet boosts act on it.
func WithBursts(ctx context.Context, q *db.Queries, snap *Snapshot) (*BurstData, error) {
	d, err := Bursts(ctx, q)
	if err != nil {
		return nil, err
	}
	snap.Bursts = d
	return d, nil
}
