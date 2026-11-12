package app

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Implants and boosters come in strengths the dump does not link as
// variations: a hardwiring's six levels (SU-601 to SU-606), a pirate
// set's low, mid and high grades, a booster's Synth, Standard,
// Improved and Strong doses, and numbered ladders (Mk II, Dose III,
// Booster IV). ladderFamilies groups them by name within one item
// group and one implant or booster slot, so they get the same compare
// table as a module's variations.

const (
	attrImplantness = 331  // the implant slot
	attrBoosterness = 1087 // the booster slot
	categoryImplant = 20   // implants and boosters
)

var (
	// "Zainou 'Gnome' Shield Upgrades SU-603", "... IH-1005".
	ladderHardwiring = regexp.MustCompile(`^(.* [A-Z]{1,4}-[0-9]{1,2}0)([1-9])$`)
	// "Mid-grade Snake Alpha".
	ladderGrade = regexp.MustCompile(`^(Low|Mid|High)-grade (.+)$`)
	// "Improved Blue Pill Booster".
	ladderDose = regexp.MustCompile(`^(Synth|Standard|Improved|Strong) (.+)$`)
	// "Limited Synth Blue Pill Booster Mk III", "Agency 'Hardshell'
	// TB5 Dose II" (the code before Dose changes with the dose),
	// "Strokar Booster VI".
	ladderRoman = regexp.MustCompile(`^(.*?)(?: (Mk|(?:[A-Z]{1,3}[0-9]+ )?Dose))? ([IVX]+)$`)

	ladderGradeRank = map[string]float64{"Low": 1, "Mid": 2, "High": 3}
	ladderDoseRank  = map[string]float64{"Synth": 1, "Standard": 2, "Improved": 3, "Strong": 4}
)

// ladderStep is where one type sits on its ladder: the name it shares
// with the other steps, its strength rank and its tier label. style
// names the ladder kind, so a family's unnumbered base step can take
// the first label of its kind.
type ladderStep struct {
	stem  string
	style string
	rank  float64
	tier  string
}

// ladderOf reads a type's step from its name. An unnumbered name is
// the first step of whatever ladder shares that name.
func ladderOf(name string) ladderStep {
	if m := ladderHardwiring.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return ladderStep{stem: m[1], style: "level", rank: float64(n), tier: "Level " + m[2]}
	}
	if m := ladderGrade.FindStringSubmatch(name); m != nil {
		return ladderStep{stem: "grade " + m[2], style: "grade", rank: ladderGradeRank[m[1]], tier: m[1] + "-grade"}
	}
	if m := ladderDose.FindStringSubmatch(name); m != nil {
		return ladderStep{stem: "dose " + m[2], style: "dose", rank: ladderDoseRank[m[1]], tier: m[1]}
	}
	if m := ladderRoman.FindStringSubmatch(name); m != nil {
		if n := romanValue(m[3]); n > 0 {
			switch {
			case m[2] == "Mk":
				return ladderStep{stem: m[1], style: "mk", rank: float64(n), tier: "Mk " + m[3]}
			case strings.HasSuffix(m[2], "Dose"):
				return ladderStep{stem: m[1], style: "dosenum", rank: float64(n), tier: "Dose " + m[3]}
			default:
				return ladderStep{stem: m[1], style: "level", rank: float64(n), tier: "Level " + strconv.Itoa(n)}
			}
		}
	}
	return ladderStep{stem: name, rank: 1}
}

// romanValue is a roman numeral's value from I to X, 0 for anything
// else.
func romanValue(s string) int {
	for i, r := range []string{"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X"} {
		if s == r {
			return i + 1
		}
	}
	return 0
}

// ladderFamilies groups the published implants and boosters not
// already in a variation family into ladders of two or more steps.
// It returns each family's members by root (its lowest type ID) and
// every member's step.
func ladderFamilies(parsed *parsedSDE, published, taken map[int64]bool) (map[int64][]int64, map[int64]ladderStep) {
	implantGroup := map[int64]bool{}
	for _, g := range parsed.groups {
		if g.categoryID == categoryImplant {
			implantGroup[g.groupID] = true
		}
	}
	slot := map[int64]int64{}
	for _, ta := range parsed.typeAttrs {
		if ta.attributeID == attrImplantness || ta.attributeID == attrBoosterness {
			slot[ta.typeID] = ta.attributeID*100 + int64(ta.value)
		}
	}
	type key struct {
		group, slot int64
		stem        string
	}
	byKey := map[key][]int64{}
	steps := map[int64]ladderStep{}
	for _, t := range parsed.types {
		if !published[t.typeID] || taken[t.typeID] || !implantGroup[t.groupID] {
			continue
		}
		st := ladderOf(t.name)
		k := key{t.groupID, slot[t.typeID], st.stem}
		byKey[k] = append(byKey[k], t.typeID)
		steps[t.typeID] = st
	}
	families := map[int64][]int64{}
	kept := map[int64]ladderStep{}
	for _, ms := range byKey {
		if len(ms) < 2 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
		style := ""
		for _, id := range ms {
			if s := steps[id].style; s != "" {
				style = s
				break
			}
		}
		if style == "" {
			continue // same name, no ladder: nothing to compare
		}
		for _, id := range ms {
			st := steps[id]
			if st.tier == "" {
				st.tier = map[string]string{"mk": "Mk I", "dosenum": "Dose I", "level": "Level 1"}[style]
			}
			kept[id] = st
		}
		families[ms[0]] = ms
	}
	return families, kept
}
